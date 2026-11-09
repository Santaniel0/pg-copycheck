package pginput

import (
	"strconv"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
)

// intChecker ports pg_strtoint16/32/64_safe from src/backend/utils/adt/numutils.c.
// PostgreSQL 16 added 0x/0o/0b prefixes and underscores between digits.
type intChecker struct {
	bits        uint
	name        string
	modern      bool // PostgreSQL 16+ syntax and overflow detection
	titleSyntax string
	titleRange  string
}

func newIntChecker(bits uint, name string, v copyopt.Version) *intChecker {
	return &intChecker{
		bits:        bits,
		name:        name,
		modern:      v >= 16,
		titleSyntax: syntaxTitle(name),
		titleRange:  "value out of range for type " + name,
	}
}

type intResult uint8

const (
	intOK intResult = iota
	intSyntax
	intRange
)

func digitValue(c byte, base uint64) (uint64, bool) {
	var d uint64
	switch {
	case c >= '0' && c <= '9':
		d = uint64(c - '0')
	case c >= 'a' && c <= 'f':
		d = uint64(c-'a') + 10
	case c >= 'A' && c <= 'F':
		d = uint64(c-'A') + 10
	default:
		return 0, false
	}
	return d, d < base
}

func (c *intChecker) parse(v []byte) (int64, intResult) {
	i, n := 0, len(v)
	for i < n && isSpace(v[i]) {
		i++
	}
	neg := false
	if i < n && v[i] == '-' {
		neg = true
		i++
	} else if i < n && v[i] == '+' {
		i++
	}

	maxMag := uint64(1) << (c.bits - 1) // magnitude of the minimum value
	base := uint64(10)
	if c.modern && i+1 < n && v[i] == '0' {
		switch v[i+1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 10 {
			i += 2
		}
	}

	first := i
	var tmp uint64
	for i < n {
		d, ok := digitValue(v[i], base)
		if ok {
			if c.modern {
				// PostgreSQL 16+ bails out as soon as another digit could
				// overflow, before looking at what follows.
				if tmp > maxMag/base {
					return 0, intRange
				}
			} else if tmp > (maxMag-d)/base {
				return 0, intRange
			}
			tmp = tmp*base + d
			i++
			continue
		}
		if c.modern && v[i] == '_' {
			// A decimal number may not start with an underscore; after a
			// 0x/0o/0b prefix one is allowed.
			if base == 10 && i == first {
				return 0, intSyntax
			}
			i++
			if i >= n {
				return 0, intSyntax
			}
			if _, ok := digitValue(v[i], base); !ok {
				return 0, intSyntax
			}
			continue
		}
		break
	}
	if i == first {
		return 0, intSyntax
	}
	for i < n && isSpace(v[i]) {
		i++
	}
	if i != n {
		return 0, intSyntax
	}
	if neg {
		if tmp > maxMag {
			return 0, intRange
		}
		return -int64(tmp-1) - 1, intOK
	}
	if tmp > maxMag-1 {
		return 0, intRange
	}
	return int64(tmp), intOK
}

func (c *intChecker) Check(v []byte) Outcome {
	switch _, r := c.parse(v); r {
	case intSyntax:
		return syntaxError(c.titleSyntax, v)
	case intRange:
		return Outcome{
			Verdict:  Invalid,
			SQLState: StateNumericRange,
			Title:    c.titleRange,
			Message:  `value "` + string(v) + `" is out of range for type ` + c.name,
		}
	}
	return valid
}

func (c *intChecker) Canonical(dst, v []byte) ([]byte, bool) {
	n, r := c.parse(v)
	if r != intOK {
		return dst, false
	}
	return strconv.AppendInt(dst, n, 10), true
}
