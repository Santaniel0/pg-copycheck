// Package copyopt models the options of COPY ... FROM ... (FORMAT csv) that
// change how input is parsed, plus the server settings that change how values
// are interpreted.
package copyopt

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Version is a PostgreSQL major version.
type Version int

// Supported version range. Rules that changed between majors are modelled
// only inside this range.
const (
	MinVersion Version = 14
	MaxVersion Version = 18
)

// ParseVersion accepts "17", "17.2" or "PostgreSQL 17".
func ParseVersion(s string) (Version, error) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.ToLower(strings.TrimSpace(s)), "postgresql"))
	major, _, _ := strings.Cut(s, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, fmt.Errorf("invalid PostgreSQL version %q", s)
	}
	v := Version(n)
	if v < MinVersion || v > MaxVersion {
		return 0, fmt.Errorf("PostgreSQL %d is not supported (supported: %d to %d)", n, MinVersion, MaxVersion)
	}
	return v, nil
}

// HeaderMode mirrors COPY's HEADER option.
type HeaderMode uint8

const (
	HeaderNone  HeaderMode = iota // HEADER false
	HeaderSkip                    // HEADER true: first line is skipped unread
	HeaderMatch                   // HEADER MATCH: names must equal the column list
)

func (h HeaderMode) String() string {
	switch h {
	case HeaderNone:
		return "false"
	case HeaderSkip:
		return "true"
	case HeaderMatch:
		return "match"
	}
	return "HeaderMode(" + strconv.Itoa(int(h)) + ")"
}

// ParseHeader accepts the spellings COPY accepts for HEADER.
func ParseHeader(s string) (HeaderMode, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "false", "off", "0", "no":
		return HeaderNone, nil
	case "true", "on", "1", "yes":
		return HeaderSkip, nil
	case "match":
		return HeaderMatch, nil
	}
	return 0, fmt.Errorf("header requires a Boolean value or \"match\", got %q", s)
}

// DateOrder is the field-order part of the DateStyle setting; it is the only
// part that affects how dates are read.
type DateOrder uint8

const (
	DateOrderMDY DateOrder = iota
	DateOrderDMY
	DateOrderYMD
)

func (d DateOrder) String() string {
	switch d {
	case DateOrderMDY:
		return "MDY"
	case DateOrderDMY:
		return "DMY"
	case DateOrderYMD:
		return "YMD"
	}
	return "DateOrder(" + strconv.Itoa(int(d)) + ")"
}

// ParseDateStyle reads a DateStyle value such as "ISO, DMY". Without an
// explicit order it follows check_datestyle in PostgreSQL: "German" implies
// DMY, anything else keeps the default MDY.
func ParseDateStyle(s string) (DateOrder, error) {
	order, haveOrder, german := DateOrderMDY, false, false
	for part := range strings.SplitSeq(s, ",") {
		switch strings.ToLower(strings.TrimSpace(part)) {
		case "iso", "sql", "postgres", "":
		case "german":
			german = true
		case "ymd":
			order, haveOrder = DateOrderYMD, true
		case "dmy", "euro", "european":
			order, haveOrder = DateOrderDMY, true
		case "mdy", "us", "noneuro", "noneuropean":
			order, haveOrder = DateOrderMDY, true
		default:
			return 0, fmt.Errorf("invalid DateStyle %q", s)
		}
	}
	if german && !haveOrder {
		order = DateOrderDMY
	}
	return order, nil
}

// ColumnSet is a FORCE_NULL / FORCE_NOT_NULL argument: a list of columns or *.
type ColumnSet struct {
	All   bool
	Names []string
}

// ParseColumnSet parses "a,b,c" or "*".
func ParseColumnSet(s string) ColumnSet {
	s = strings.TrimSpace(s)
	if s == "" {
		return ColumnSet{}
	}
	if s == "*" {
		return ColumnSet{All: true}
	}
	var names []string
	for n := range strings.SplitSeq(s, ",") {
		if n = strings.TrimSpace(n); n != "" {
			names = append(names, n)
		}
	}
	return ColumnSet{Names: names}
}

// Empty reports whether the set selects no columns.
func (c ColumnSet) Empty() bool { return !c.All && len(c.Names) == 0 }

// Options are the COPY FROM options for FORMAT csv.
type Options struct {
	Delimiter    byte
	Quote        byte
	Escape       byte
	Null         string
	Header       HeaderMode
	ForceNull    ColumnSet
	ForceNotNull ColumnSet
	Version      Version
	DateOrder    DateOrder
}

// Default returns COPY's CSV defaults with DateStyle "ISO, MDY" and
// PostgreSQL 17.
func Default() Options {
	return Options{
		Delimiter: ',',
		Quote:     '"',
		Escape:    '"',
		Header:    HeaderNone,
		Version:   17,
		DateOrder: DateOrderMDY,
	}
}

// ErrInvalidOptions is wrapped by every error returned from Validate.
var ErrInvalidOptions = errors.New("invalid COPY options")

// Validate applies the checks ProcessCopyOptions performs for CSV input.
func (o Options) Validate() error {
	bad := func(msg string) error { return fmt.Errorf("%w: %s", ErrInvalidOptions, msg) }
	switch {
	case o.Delimiter == '\n' || o.Delimiter == '\r':
		return bad("COPY delimiter cannot be newline or carriage return")
	case strings.ContainsAny(o.Null, "\r\n"):
		return bad("COPY null representation cannot use newline or carriage return")
	case o.Delimiter == o.Quote:
		return bad("COPY delimiter and quote must be different")
	case o.Quote == '\n' || o.Quote == '\r' || o.Escape == '\n' || o.Escape == '\r':
		// PostgreSQL does not forbid this, but such files cannot be split into
		// lines the way COPY does it, so the checker refuses them.
		return bad("quote and escape cannot be newline or carriage return")
	case strings.IndexByte(o.Null, o.Delimiter) >= 0:
		return bad("COPY delimiter character must not appear in the NULL specification")
	case strings.IndexByte(o.Null, o.Quote) >= 0:
		return bad("CSV quote character must not appear in the NULL specification")
	case o.Header == HeaderMatch && o.Version < 15:
		return bad("HEADER MATCH requires PostgreSQL 15 or later")
	case o.Version < MinVersion || o.Version > MaxVersion:
		return bad(fmt.Sprintf("PostgreSQL %d is not supported", o.Version))
	}
	return nil
}

// ParseByteOption reads a single-byte option value. "\t" is accepted as a tab
// because shells make a literal tab awkward to type.
func ParseByteOption(name, s string) (byte, error) {
	if s == `\t` {
		return '\t', nil
	}
	if len(s) != 1 {
		return 0, fmt.Errorf("%w: COPY %s must be a single one-byte character", ErrInvalidOptions, name)
	}
	return s[0], nil
}
