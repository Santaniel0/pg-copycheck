package pginput

import (
	"bytes"
	"fmt"
	"time"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/schema"
)

// dateTimeChecker covers date, timestamp and timestamptz. PostgreSQL reads
// all three with ParseDateTime + DecodeDateTime in
// src/backend/utils/adt/datetime.c, which accept far more than is ported
// here (month names, BC, Julian days, time zone names). The checker decides
// only these shapes and answers Unsure for the rest:
//
//	Y-M-D, D-M-Y, M-D-Y and so on with '-', '/' or '.' as the separator
//	optionally followed by ' ' or 'T' and H:MM[:SS[.ffffff]]
//	optionally followed by Z, UTC, GMT or a numeric offset (+03, +03:00, +0300)
//
// plus the special words (epoch, infinity, now, today, ...). Strings that no
// date/time input could ever accept (empty, no letters or digits, bytes
// ParseDateTime rejects) are Invalid.
type dateTimeChecker struct {
	kind      schema.TypeKind
	order     copyopt.DateOrder
	precision int

	titleSyntax   string
	titleOverflow string
	titleTZ       string
	titleUnsure   string
}

func newDateTimeChecker(t schema.Type, order copyopt.DateOrder) *dateTimeChecker {
	name := t.Kind.String()
	// The input functions name the type without typmod and use the short
	// spelling for timestamp without time zone.
	if t.Kind == schema.TypeTimestamp {
		name = "timestamp"
	}
	prec := t.Precision
	if prec == schema.NoTypmod {
		prec = 6
	}
	return &dateTimeChecker{
		kind:          t.Kind,
		order:         order,
		precision:     prec,
		titleSyntax:   syntaxTitle(name),
		titleOverflow: "date/time field value out of range",
		titleTZ:       "time zone displacement out of range",
		titleUnsure:   name + " value outside the forms checked offline",
	}
}

type dtResult uint8

const (
	dtOK dtResult = iota
	dtSpecial
	dtBadFormat
	dtFieldOverflow
	dtMDFieldOverflow // DTERR_MD_FIELD_OVERFLOW: comes with the DateStyle hint
	dtTZOverflow
	dtUnsure
)

type dateTime struct {
	year, month, day  int
	hasTime           bool
	hour, min, sec    int
	micros, fracDigit int
	hasTZ             bool
	tzSeconds         int
	tzOutOfRange      bool // DecodeTimezone: hour > MAX_TZDISP_HOUR (15) or minute > 59
}

var dateTimeSpecials = map[string]bool{
	"epoch": true, "infinity": true, "-infinity": true,
	"now": true, "today": true, "tomorrow": true, "yesterday": true,
}

func isAlpha(c byte) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// isPunct is C ispunct() for ASCII.
func isPunct(c byte) bool {
	return c >= '!' && c <= '/' || c >= ':' && c <= '@' || c >= '[' && c <= '`' || c >= '{' && c <= '~'
}

func (c *dateTimeChecker) parse(v []byte) (dateTime, dtResult) {
	var dt dateTime
	s := trimSpace(v)
	if len(s) == 0 {
		return dt, dtBadFormat
	}
	var hasDigit, hasAlpha bool
	for _, b := range s {
		switch {
		case isDigit(b):
			hasDigit = true
		case isAlpha(b):
			hasAlpha = true
		case isSpace(b) || isPunct(b):
		default:
			// ParseDateTime: "otherwise, something is not right...". Non-ASCII
			// bytes are neither alpha nor punct in C or UTF-8 locales.
			return dt, dtBadFormat
		}
	}
	if !hasDigit && !hasAlpha {
		return dt, dtBadFormat
	}
	if !hasDigit {
		if dateTimeSpecials[string(bytes.ToLower(s))] {
			return dt, dtSpecial
		}
		return dt, dtUnsure
	}

	p := dtCursor{s: s}
	f1, l1 := p.digits()
	if l1 == 0 || l1 > 4 || p.done() {
		return dt, dtUnsure
	}
	sep := p.s[p.i]
	if sep != '-' && sep != '/' && sep != '.' {
		return dt, dtUnsure
	}
	p.i++
	f2, l2 := p.digits()
	if l2 == 0 || l2 > 2 || p.done() || p.s[p.i] != sep {
		return dt, dtUnsure
	}
	p.i++
	f3, l3 := p.digits()
	if l3 == 0 || l3 > 4 {
		return dt, dtUnsure
	}
	if !c.decodeDate(&dt, [3]int{f1, f2, f3}, [3]int{l1, l2, l3}) {
		return dt, dtUnsure
	}

	if !p.done() {
		switch {
		case p.s[p.i] == 'T' || p.s[p.i] == 't':
			p.i++
		case isSpace(p.s[p.i]):
			p.spaces()
		default:
			return dt, dtUnsure
		}
		if r := p.timeOfDay(&dt); r != dtOK {
			return dt, r
		}
		p.spaces()
		if !p.done() {
			if r := p.zone(&dt); r != dtOK {
				return dt, r
			}
		}
	}
	if !p.done() {
		return dt, dtUnsure
	}

	// Error order follows DecodeDateTime: minute/second sanity during the
	// field loop, then the time zone, then ValidateDate, then time_overflows.
	if dt.hasTime && (dt.min > 59 || dt.sec > 60) {
		return dt, dtFieldOverflow
	}
	if dt.tzOutOfRange {
		return dt, dtTZOverflow
	}
	if r := c.validateDate(&dt); r != dtOK {
		return dt, r
	}
	if dt.hasTime {
		total := ((dt.hour*60+dt.min)*60+dt.sec)*1_000_000 + dt.micros
		if dt.hour > 24 || total > 24*3600*1_000_000 {
			return dt, dtFieldOverflow
		}
	}
	return dt, dtOK
}

// Field masks as in DecodeNumber.
const (
	maskYear = 1 << iota
	maskMonth
	maskDay
)

// decodeDate assigns three numeric fields the way DecodeDate/DecodeNumber
// do. It returns false for the day-of-year special case, which is not
// modelled.
func (c *dateTimeChecker) decodeDate(dt *dateTime, vals, lens [3]int) bool {
	fmask := 0
	is2digits := false
	for k := range 3 {
		val, flen := vals[k], lens[k]
		if flen == 3 && fmask == maskYear && val >= 1 && val <= 366 {
			return false
		}
		var field int
		switch fmask {
		case 0:
			switch {
			case flen >= 3 || c.order == copyopt.DateOrderYMD:
				field = maskYear
			case c.order == copyopt.DateOrderDMY:
				field = maskDay
			default:
				field = maskMonth
			}
		case maskYear:
			field = maskMonth
		case maskMonth, maskYear | maskMonth:
			field = maskDay
		case maskDay:
			field = maskMonth
		case maskMonth | maskDay:
			field = maskYear
		default:
			return false
		}
		switch field {
		case maskYear:
			dt.year, is2digits = val, flen <= 2
		case maskMonth:
			dt.month = val
		case maskDay:
			dt.day = val
		}
		fmask |= field
	}
	if is2digits {
		switch {
		case dt.year < 70:
			dt.year += 2000
		case dt.year < 100:
			dt.year += 1900
		}
	} else if dt.year <= 0 {
		// ValidateDate: there is no year zero in AD/BC notation.
		dt.year = 0
	}
	return true
}

var daysInMonth = [2][12]int{
	{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31},
	{31, 29, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31},
}

func isLeap(y int) bool { return y%4 == 0 && (y%100 != 0 || y%400 == 0) }

// validateDate ports ValidateDate for the fields set by decodeDate.
func (c *dateTimeChecker) validateDate(dt *dateTime) dtResult {
	if dt.year <= 0 {
		return dtFieldOverflow
	}
	if dt.month < 1 || dt.month > 12 {
		return dtMDFieldOverflow
	}
	if dt.day < 1 || dt.day > 31 {
		return dtMDFieldOverflow
	}
	leap := 0
	if isLeap(dt.year) {
		leap = 1
	}
	if dt.day > daysInMonth[leap][dt.month-1] {
		return dtFieldOverflow
	}
	return dtOK
}

type dtCursor struct {
	s []byte
	i int
}

func (p *dtCursor) done() bool { return p.i >= len(p.s) }

func (p *dtCursor) spaces() {
	for p.i < len(p.s) && isSpace(p.s[p.i]) {
		p.i++
	}
}

// digits reads up to five digits so that over-long fields are visible to the
// caller as such.
func (p *dtCursor) digits() (val, n int) {
	for p.i < len(p.s) && isDigit(p.s[p.i]) && n < 5 {
		val = val*10 + int(p.s[p.i]-'0')
		p.i++
		n++
	}
	if p.i < len(p.s) && isDigit(p.s[p.i]) {
		return 0, 99
	}
	return val, n
}

func (p *dtCursor) timeOfDay(dt *dateTime) dtResult {
	h, hl := p.digits()
	if hl == 0 || hl > 2 || p.done() || p.s[p.i] != ':' {
		return dtUnsure
	}
	p.i++
	m, ml := p.digits()
	if ml != 2 {
		return dtUnsure
	}
	dt.hasTime, dt.hour, dt.min = true, h, m
	if p.done() || p.s[p.i] != ':' {
		return dtOK
	}
	p.i++
	sec, sl := p.digits()
	if sl != 2 {
		return dtUnsure
	}
	dt.sec = sec
	if p.done() || p.s[p.i] != '.' {
		return dtOK
	}
	p.i++
	start := p.i
	for p.i < len(p.s) && isDigit(p.s[p.i]) {
		p.i++
	}
	n := p.i - start
	// More than six digits are rounded to microseconds, which can carry
	// into the seconds; that path is not modelled.
	if n == 0 || n > 6 {
		return dtUnsure
	}
	micros := 0
	for _, b := range p.s[start:p.i] {
		micros = micros*10 + int(b-'0')
	}
	for k := n; k < 6; k++ {
		micros *= 10
	}
	dt.micros, dt.fracDigit = micros, n
	return dtOK
}

// zone reads what DecodeTimezone accepts in the shapes listed above.
func (p *dtCursor) zone(dt *dateTime) dtResult {
	c := p.s[p.i]
	if isAlpha(c) {
		start := p.i
		for p.i < len(p.s) && isAlpha(p.s[p.i]) {
			p.i++
		}
		switch string(bytes.ToLower(p.s[start:p.i])) {
		case "z", "utc", "gmt":
			dt.hasTZ, dt.tzSeconds = true, 0
			p.spaces()
			return dtOK
		}
		return dtUnsure
	}
	if c != '+' && c != '-' {
		return dtUnsure
	}
	p.i++
	h, hl := p.digits()
	var hours, mins int
	switch {
	case hl == 1 || hl == 2:
		hours = h
		if !p.done() && p.s[p.i] == ':' {
			p.i++
			m, ml := p.digits()
			if ml != 2 {
				return dtUnsure
			}
			mins = m
		}
	case hl == 4:
		hours, mins = h/100, h%100
	default:
		return dtUnsure
	}
	p.spaces()
	dt.hasTZ = true
	dt.tzSeconds = hours*3600 + mins*60
	if c == '-' {
		dt.tzSeconds = -dt.tzSeconds
	}
	dt.tzOutOfRange = hours > 15 || mins > 59
	return dtOK
}

func (c *dateTimeChecker) Check(v []byte) Outcome {
	_, r := c.parse(v)
	return c.outcome(r, v)
}

func (c *dateTimeChecker) outcome(r dtResult, v []byte) Outcome {
	switch r {
	case dtOK, dtSpecial:
		return valid
	case dtBadFormat:
		return Outcome{Verdict: Invalid, SQLState: StateInvalidDatetime, Title: c.titleSyntax,
			Message: c.titleSyntax + `: "` + string(v) + `"`}
	case dtFieldOverflow, dtMDFieldOverflow:
		out := Outcome{Verdict: Invalid, SQLState: StateDatetimeOverflow, Title: c.titleOverflow,
			Message: c.titleOverflow + `: "` + string(v) + `"`}
		if r == dtMDFieldOverflow {
			out.Hint = `Perhaps you need a different "datestyle" setting.`
		}
		return out
	case dtTZOverflow:
		return Outcome{Verdict: Invalid, SQLState: StateTZDisplacement, Title: c.titleTZ,
			Message: c.titleTZ + `: "` + string(v) + `"`}
	}
	return unsure(c.titleUnsure)
}

// Canonical gives the instant a unique index compares. timestamp ignores an
// explicit zone; timestamptz without one depends on the session TimeZone and
// is left undecided, as are special words and fractions that need rounding.
func (c *dateTimeChecker) Canonical(dst, v []byte) ([]byte, bool) {
	dt, r := c.parse(v)
	if r != dtOK || dt.fracDigit > c.precision {
		return dst, false
	}
	if c.kind == schema.TypeDate {
		return fmt.Appendf(dst, "%04d-%02d-%02d", dt.year, dt.month, dt.day), true
	}
	zone := time.UTC
	if c.kind == schema.TypeTimestampTZ {
		if !dt.hasTZ {
			return dst, false
		}
		zone = time.FixedZone("", dt.tzSeconds)
	}
	t := time.Date(dt.year, time.Month(dt.month), dt.day, dt.hour, dt.min, dt.sec, dt.micros*1000, zone)
	return t.UTC().AppendFormat(dst, "2006-01-02T15:04:05.000000"), true
}
