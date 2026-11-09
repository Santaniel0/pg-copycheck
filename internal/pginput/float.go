package pginput

import (
	"math"
	"strconv"
)

// floatChecker ports float4in_internal / float8in_internal from
// src/backend/utils/adt/float.c. Those call the C library's strtof/strtod,
// so forms only some C libraries accept (hex floats, "nan(...)", signed NaN)
// are Unsure.
type floatChecker struct {
	bits        int
	name        string
	titleSyntax string
	titleRange  string
}

func newFloatChecker(bits int) *floatChecker {
	name := "double precision"
	if bits == 32 {
		name = "real"
	}
	return &floatChecker{
		bits:        bits,
		name:        name,
		titleSyntax: syntaxTitle(name),
		titleRange:  "value out of range for type " + name,
	}
}

var floatWords = []string{"infinity", "inf", "nan"}

// scanDecimal returns the length of the longest prefix of s that strtod
// reads as a decimal number, and whether its mantissa has a non-zero digit.
func scanDecimal(s []byte) (n int, nonZero bool) {
	i := 0
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		i++
	}
	digits := 0
	for i < len(s) && isDigit(s[i]) {
		nonZero = nonZero || s[i] != '0'
		i++
		digits++
	}
	if i < len(s) && s[i] == '.' {
		i++
		for i < len(s) && isDigit(s[i]) {
			nonZero = nonZero || s[i] != '0'
			i++
			digits++
		}
	}
	if digits == 0 {
		return 0, false
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			j++
		}
		if j < len(s) && isDigit(s[j]) {
			for j < len(s) && isDigit(s[j]) {
				j++
			}
			i = j
		}
	}
	return i, nonZero
}

func (c *floatChecker) parse(v []byte) (float64, Outcome) {
	s := v
	for len(s) > 0 && isSpace(s[0]) {
		s = s[1:]
	}
	if len(s) == 0 {
		return 0, syntaxError(c.titleSyntax, v)
	}
	unsigned := s
	if unsigned[0] == '+' || unsigned[0] == '-' {
		unsigned = unsigned[1:]
	}
	if len(unsigned) >= 2 && unsigned[0] == '0' && (unsigned[1] == 'x' || unsigned[1] == 'X') {
		return 0, unsure("hexadecimal floating-point input depends on the server's C library")
	}

	n, nonZero := scanDecimal(s)
	if n == 0 {
		for _, w := range floatWords {
			if hasPrefixFold(unsigned, w) {
				rest := unsigned[len(w):]
				if w == "nan" && (len(unsigned) != len(s) || len(rest) > 0 && rest[0] == '(') {
					return 0, unsure("signed or extended NaN input depends on the server's C library")
				}
				if len(trimSpace(rest)) != 0 {
					return 0, syntaxError(c.titleSyntax, v)
				}
				if w == "nan" {
					return math.NaN(), valid
				}
				if s[0] == '-' {
					return math.Inf(-1), valid
				}
				return math.Inf(1), valid
			}
		}
		return 0, syntaxError(c.titleSyntax, v)
	}

	if len(trimSpace(s[n:])) != 0 {
		return 0, syntaxError(c.titleSyntax, v)
	}
	f, err := strconv.ParseFloat(string(s[:n]), c.bits)
	// strtod reports ERANGE both for overflow and for underflow to zero;
	// PostgreSQL rejects those but keeps denormals.
	if err != nil || nonZero && f == 0 {
		return 0, Outcome{
			Verdict:  Invalid,
			SQLState: StateNumericRange,
			Title:    c.titleRange,
			Message:  `"` + string(s[:n]) + `" is out of range for type ` + c.name,
		}
	}
	return f, valid
}

func (c *floatChecker) Check(v []byte) Outcome {
	_, out := c.parse(v)
	return out
}

// Canonical follows float8eq: all NaNs are equal and -0 equals 0.
func (c *floatChecker) Canonical(dst, v []byte) ([]byte, bool) {
	f, out := c.parse(v)
	if out.Verdict != Valid {
		return dst, false
	}
	if math.IsNaN(f) {
		return append(dst, "NaN"...), true
	}
	if f == 0 {
		f = 0
	}
	return strconv.AppendFloat(dst, f, 'g', -1, c.bits), true
}
