// Package fixture generates a realistic orders export with known faults at
// known places, for tests and the demo.
package fixture

import (
	"bufio"
	_ "embed"
	"fmt"
	"io"
	"math/rand/v2"
	"strconv"
)

// DDL is the schema the generated data is meant for.
//
//go:embed orders.sql
var DDL string

// Fault is a kind of broken row the generator injects.
type Fault uint8

const (
	FaultBadInteger Fault = iota
	FaultTooLong
	FaultNullNotNull
	FaultExtraColumn
	FaultNumericOverflow
	FaultBadEnum
	FaultDayFirstDate
	FaultWin1251
	FaultNUL
	FaultDuplicateID
	FaultBadUUID
	FaultBadJSON
	FaultBadDate
	FaultBadBool
	FaultMissingColumn
	FaultZoneName
	NumFaults // number of fault kinds
)

var faultNames = [...]string{
	FaultBadInteger:      "bad integer",
	FaultTooLong:         "value too long",
	FaultNullNotNull:     "null in NOT NULL column",
	FaultExtraColumn:     "extra column",
	FaultNumericOverflow: "numeric overflow",
	FaultBadEnum:         "unknown enum label",
	FaultDayFirstDate:    "day-first timestamp",
	FaultWin1251:         "Windows-1251 bytes",
	FaultNUL:             "NUL byte",
	FaultDuplicateID:     "duplicate primary key",
	FaultBadUUID:         "bad uuid",
	FaultBadJSON:         "bad json",
	FaultBadDate:         "impossible date",
	FaultBadBool:         "bad boolean",
	FaultMissingColumn:   "missing column",
	FaultZoneName:        "time zone name (valid, but not decided offline)",
}

func (f Fault) String() string {
	if int(f) < len(faultNames) {
		return faultNames[f]
	}
	return "Fault(" + strconv.Itoa(int(f)) + ")"
}

// Rejected reports whether PostgreSQL would reject a row with this fault
// under the default DateStyle (ISO, MDY).
func (f Fault) Rejected() bool { return f != FaultZoneName }

// Config controls generation.
type Config struct {
	Rows      int
	Seed      uint64
	FaultEach int  // one faulty row per FaultEach rows; 0 disables faults
	CRLF      bool // Windows line endings
	EndMarker bool // put a `\.` line before the last tenth of the rows
}

// Injected records where a fault was placed. Record counts the header line,
// as COPY does.
type Injected struct {
	Record int64
	Line   int64
	Fault  Fault
}

var (
	statuses   = []string{"new", "paid", "shipped", "cancelled"}
	currencies = []string{"RUB", "RUB", "RUB", "USD", "EUR"}
	names      = []string{"ivanov", "petrova", "smirnov", "kuznetsova", "popov", "volkova", "lebedev", "sokolova"}
	comments   = []string{
		"", "", "", "Позвонить за час до доставки", "Оставить у двери", "Подъезд 2, код 4512",
		"\"Срочно\", клиент ждёт", "Доставка после 18:00\nне раньше",
	}
	// "Доставка до двери" in Windows-1251.
	win1251Comment = "\xc4\xee\xf1\xf2\xe0\xe2\xea\xe0 \xe4\xee \xe4\xe2\xe5\xf0\xe8"
)

const header = "id,customer_id,status,quantity,amount,currency,email,paid,created_at,shipped_on,external_id,meta,weight_kg,comment"

// Generate writes a CSV export with a header line and returns the faults it
// injected, in order.
func Generate(w io.Writer, cfg Config) ([]Injected, error) {
	bw := bufio.NewWriterSize(w, 256<<10)
	rng := rand.New(rand.NewPCG(cfg.Seed, cfg.Seed^0x9e3779b97f4a7c15))
	eol := "\n"
	if cfg.CRLF {
		eol = "\r\n"
	}

	var injected []Injected
	line := int64(1)
	record := int64(1)
	bw.WriteString(header + eol)

	markerAt := -1
	if cfg.EndMarker && cfg.Rows >= 10 {
		markerAt = cfg.Rows - cfg.Rows/10
	}
	nextFault := Fault(0)
	buf := make([]byte, 0, 512)
	for row := 1; row <= cfg.Rows; row++ {
		if row == markerAt {
			bw.WriteString(`\.` + eol)
			line++
			record++
		}
		fault, hasFault := Fault(0), false
		if cfg.FaultEach > 0 && row%cfg.FaultEach == 0 && row > 1 {
			fault, hasFault = nextFault, true
			nextFault = (nextFault + 1) % NumFaults
		}
		buf = appendRow(buf[:0], rng, row, fault, hasFault)
		record++
		line++
		if hasFault {
			injected = append(injected, Injected{Record: record, Line: line, Fault: fault})
		}
		for _, c := range buf {
			if c == '\n' {
				line++
			}
		}
		bw.Write(buf)
		bw.WriteString(eol)
	}
	if err := bw.Flush(); err != nil {
		return nil, fmt.Errorf("write fixture: %w", err)
	}
	return injected, nil
}

func appendCSV(dst []byte, s string) []byte {
	needs := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ',', '"', '\n', '\r':
			needs = true
		}
	}
	if !needs {
		return append(dst, s...)
	}
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' {
			dst = append(dst, '"')
		}
		dst = append(dst, s[i])
	}
	return append(dst, '"')
}

func appendRow(b []byte, rng *rand.Rand, row int, fault Fault, hasFault bool) []byte {
	is := func(f Fault) bool { return hasFault && fault == f }

	id := row
	if is(FaultDuplicateID) {
		// Faults sit on multiples of FaultEach, so the previous row is a
		// clean one and its id is already taken.
		id = row - 1
	}
	b = strconv.AppendInt(b, int64(id), 10)
	b = append(b, ',')
	if !is(FaultNullNotNull) {
		b = strconv.AppendInt(b, int64(1+rng.IntN(50000)), 10)
	}
	b = append(b, ',')
	if is(FaultBadEnum) {
		b = append(b, "refunded"...)
	} else {
		b = append(b, statuses[rng.IntN(len(statuses))]...)
	}
	b = append(b, ',')
	if is(FaultBadInteger) {
		b = append(b, "12 шт"...)
	} else {
		b = strconv.AppendInt(b, int64(1+rng.IntN(20)), 10)
	}
	b = append(b, ',')
	if is(FaultNumericOverflow) {
		b = append(b, "12345678901.50"...)
	} else {
		cents := 100 + rng.IntN(5_000_000)
		b = fmt.Appendf(b, "%d.%02d", cents/100, cents%100)
	}
	b = append(b, ',')
	b = append(b, currencies[rng.IntN(len(currencies))]...)
	b = append(b, ',')
	switch {
	case is(FaultTooLong):
		b = append(b, "very.long.address.that.someone.typed.into.the.wrong.field@example.com"...)
	case rng.IntN(10) > 0:
		b = fmt.Appendf(b, "%s%d@example.com", names[rng.IntN(len(names))], rng.IntN(1000))
	}
	b = append(b, ',')
	switch {
	case is(FaultBadBool):
		b = append(b, "да"...)
	case rng.IntN(2) == 0:
		b = append(b, 't')
	default:
		b = append(b, 'f')
	}
	b = append(b, ',')
	month, day := 1+rng.IntN(12), 1+rng.IntN(28)
	switch {
	case is(FaultDayFirstDate):
		b = fmt.Appendf(b, "%02d.%02d.2024 %02d:%02d", 13+rng.IntN(16), month, rng.IntN(24), rng.IntN(60))
	case is(FaultZoneName):
		b = fmt.Appendf(b, "2024-%02d-%02d %02d:%02d:00 MSK", month, day, rng.IntN(24), rng.IntN(60))
	default:
		b = fmt.Appendf(b, "2024-%02d-%02d %02d:%02d:%02d+03", month, day, rng.IntN(24), rng.IntN(60), rng.IntN(60))
	}
	b = append(b, ',')
	switch {
	case is(FaultBadDate):
		b = append(b, "2024-02-30"...)
	case rng.IntN(3) > 0:
		b = fmt.Appendf(b, "2024-%02d-%02d", month, min(day+2, 28))
	}
	b = append(b, ',')
	if is(FaultBadUUID) {
		b = append(b, "not-a-uuid"...)
	} else {
		hi, lo := rng.Uint64(), rng.Uint64()
		b = fmt.Appendf(b, "%08x-%04x-4%03x-%04x-%012x", hi>>32, hi>>16&0xffff, hi&0xfff, 0x8000|lo>>48&0x3fff, lo&0xffffffffffff)
	}
	b = append(b, ',')
	if is(FaultBadJSON) {
		b = appendCSV(b, `{"source": web}`)
	} else if rng.IntN(4) > 0 {
		b = appendCSV(b, fmt.Sprintf(`{"source":"%s","items":%d}`, []string{"web", "app", "phone"}[rng.IntN(3)], 1+rng.IntN(5)))
	}
	b = append(b, ',')
	b = fmt.Appendf(b, "%d.%02d", rng.IntN(30), rng.IntN(100))
	b = append(b, ',')
	switch {
	case is(FaultWin1251):
		b = append(b, win1251Comment...)
	case is(FaultNUL):
		b = append(b, "battery\x00pack"...)
	default:
		b = appendCSV(b, comments[rng.IntN(len(comments))])
	}
	switch {
	case is(FaultExtraColumn):
		b = append(b, ",extra"...)
	case is(FaultMissingColumn):
		b = b[:lastComma(b)]
	}
	return b
}

func lastComma(b []byte) int {
	inQuote := false
	last := len(b)
	for i, c := range b {
		switch {
		case c == '"':
			inQuote = !inQuote
		case c == ',' && !inQuote:
			last = i
		}
	}
	return last
}
