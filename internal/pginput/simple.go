package pginput

import (
	"bytes"
	"fmt"
	"unicode/utf8"

	"github.com/Santaniel0/pg-copycheck/internal/schema"
)

// boolChecker ports boolin and parse_bool_with_len from
// src/backend/utils/adt/bool.c: any prefix of true/false/yes/no is accepted,
// "on"/"off" need at least two letters, and 1/0 must stand alone.
type boolChecker struct{}

func parseBool(v []byte) (value, ok bool) {
	s := trimSpace(v)
	if len(s) == 0 {
		return false, false
	}
	prefixOf := func(word string, minLen int) bool {
		n := max(len(s), minLen)
		return n <= len(word) && len(s) == n && bytes.EqualFold(s, []byte(word[:n]))
	}
	switch s[0] {
	case 't', 'T':
		return true, prefixOf("true", 1)
	case 'f', 'F':
		return false, prefixOf("false", 1)
	case 'y', 'Y':
		return true, prefixOf("yes", 1)
	case 'n', 'N':
		return false, prefixOf("no", 1)
	case 'o', 'O':
		if prefixOf("on", 2) {
			return true, true
		}
		return false, prefixOf("off", 2)
	case '1':
		return true, len(s) == 1
	case '0':
		return false, len(s) == 1
	}
	return false, false
}

func (boolChecker) Check(v []byte) Outcome {
	if _, ok := parseBool(v); !ok {
		return syntaxError(syntaxTitle("boolean"), v)
	}
	return valid
}

func (boolChecker) Canonical(dst, v []byte) ([]byte, bool) {
	b, ok := parseBool(v)
	if !ok {
		return dst, false
	}
	if b {
		return append(dst, 't'), true
	}
	return append(dst, 'f'), true
}

// textChecker ports varchar_input and bpchar_input from
// src/backend/utils/adt/varchar.c. Length counts characters, and characters
// past the limit are dropped silently if they are all ASCII spaces.
type textChecker struct {
	limit  int // schema.NoTypmod for no limit
	bpchar bool
	title  string
}

func newTextChecker(limit int, typeName string) textChecker {
	c := textChecker{limit: limit, bpchar: typeName == "character"}
	if limit != schema.NoTypmod {
		c.title = fmt.Sprintf("value too long for type %s(%d)", typeName, limit)
	}
	return c
}

// clip returns v cut to the limit, or ok=false when non-space characters
// would be lost.
func (c textChecker) clip(v []byte) ([]byte, bool) {
	if c.limit == schema.NoTypmod || len(v) <= c.limit {
		return v, true
	}
	cut, chars := 0, 0
	for cut < len(v) && chars < c.limit {
		_, size := utf8.DecodeRune(v[cut:])
		cut += size
		chars++
	}
	for _, b := range v[cut:] {
		if b != ' ' {
			return nil, false
		}
	}
	return v[:cut], true
}

func (c textChecker) Check(v []byte) Outcome {
	if _, ok := c.clip(v); !ok {
		return Outcome{Verdict: Invalid, SQLState: StateRightTruncation, Title: c.title, Message: c.title}
	}
	return valid
}

// Canonical assumes a deterministic collation, where equality is byte
// equality. Trailing spaces are insignificant for character(n).
func (c textChecker) Canonical(dst, v []byte) ([]byte, bool) {
	s, ok := c.clip(v)
	if !ok {
		return dst, false
	}
	if c.bpchar {
		s = bytes.TrimRight(s, " ")
	}
	return append(dst, s...), true
}

// uuidChecker ports string_to_uuid from src/backend/utils/adt/uuid.c:
// optional braces, 32 hex digits, and an optional hyphen after any group of
// four. No surrounding whitespace is allowed.
type uuidChecker struct{}

func parseUUID(v []byte, dst []byte) ([]byte, bool) {
	s := v
	braces := len(s) > 0 && s[0] == '{'
	if braces {
		s = s[1:]
	}
	for i := 0; i < 16; i++ {
		if len(s) < 2 {
			return dst, false
		}
		hi, ok1 := digitValue(s[0], 16)
		lo, ok2 := digitValue(s[1], 16)
		if !ok1 || !ok2 {
			return dst, false
		}
		dst = append(dst, "0123456789abcdef"[hi], "0123456789abcdef"[lo])
		s = s[2:]
		if len(s) > 0 && s[0] == '-' && i%2 == 1 && i < 15 {
			s = s[1:]
		}
	}
	if braces {
		if len(s) == 0 || s[0] != '}' {
			return dst, false
		}
		s = s[1:]
	}
	return dst, len(s) == 0
}

func (uuidChecker) Check(v []byte) Outcome {
	var buf [32]byte
	if _, ok := parseUUID(v, buf[:0]); !ok {
		return syntaxError(syntaxTitle("uuid"), v)
	}
	return valid
}

func (uuidChecker) Canonical(dst, v []byte) ([]byte, bool) {
	out, ok := parseUUID(v, dst)
	return out, ok
}

// enumChecker ports enum_in from src/backend/utils/adt/enum.c: the label
// must match exactly, without trimming or case folding.
type enumChecker struct {
	labels map[string]struct{}
	title  string
}

func newEnumChecker(t schema.Type) enumChecker {
	labels := make(map[string]struct{}, len(t.Labels))
	for _, l := range t.Labels {
		labels[l] = struct{}{}
	}
	return enumChecker{labels: labels, title: "invalid input value for enum " + t.Name}
}

func (c enumChecker) Check(v []byte) Outcome {
	if _, ok := c.labels[string(v)]; !ok {
		return Outcome{
			Verdict:  Invalid,
			SQLState: StateInvalidText,
			Title:    c.title,
			Message:  c.title + `: "` + string(v) + `"`,
		}
	}
	return valid
}

func (c enumChecker) Canonical(dst, v []byte) ([]byte, bool) {
	if _, ok := c.labels[string(v)]; !ok {
		return dst, false
	}
	return append(dst, v...), true
}
