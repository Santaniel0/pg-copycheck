package pginput

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"strconv"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/schema"
)

// numericChecker ports numeric_in, set_var_from_str and apply_typmod from
// src/backend/utils/adt/numeric.c.
type numericChecker struct {
	modern      bool // PostgreSQL 16+: underscores and 0x/0o/0b integers
	precision   int  // schema.NoTypmod for unconstrained numeric
	scale       int
	typmodOK    bool // scale within 0..precision, the range modelled here
	titleSyntax string
}

func newNumericChecker(t schema.Type, v copyopt.Version) *numericChecker {
	return &numericChecker{
		modern:      v >= 16,
		precision:   t.Precision,
		scale:       t.Scale,
		typmodOK:    t.Precision == schema.NoTypmod || t.Scale >= 0 && t.Scale <= t.Precision,
		titleSyntax: syntaxTitle("numeric"),
	}
}

type numKind uint8

const (
	numFinite numKind = iota
	numNaN
	numPosInf
	numNegInf
)

// decimal is 0.digits × 10^point with no leading zeros in digits; an empty
// digits slice is zero.
type decimal struct {
	kind   numKind
	neg    bool
	digits []byte
	point  int64
	dscale int64
}

type numError uint8

const (
	numOK numError = iota
	numSyntax
	numOverflowFormat // "value overflows numeric format"
)

func hasPrefixFold(v []byte, word string) bool {
	return len(v) >= len(word) && bytes.EqualFold(v[:len(word)], []byte(word))
}

func (c *numericChecker) parse(v []byte) (decimal, numError) {
	i, n := 0, len(v)
	for i < n && isSpace(v[i]) {
		i++
	}
	numStart := i
	var d decimal
	if i < n && v[i] == '+' {
		i++
	} else if i < n && v[i] == '-' {
		d.neg = true
		i++
	}

	if i >= n || !isDigit(v[i]) && v[i] != '.' {
		switch {
		case hasPrefixFold(v[numStart:], "nan"): // NaN takes no sign
			d.kind, i = numNaN, numStart+3
		case hasPrefixFold(v[i:], "infinity"):
			i += 8
			d.kind = numPosInf
		case hasPrefixFold(v[i:], "inf"):
			i += 3
			d.kind = numPosInf
		default:
			return d, numSyntax
		}
		if d.kind == numPosInf && d.neg {
			d.kind = numNegInf
		}
		return d, trailing(v, i)
	}

	if c.modern && i+1 < n && v[i] == '0' {
		base := 0
		switch v[i+1] {
		case 'x', 'X':
			base = 16
		case 'o', 'O':
			base = 8
		case 'b', 'B':
			base = 2
		}
		if base != 0 {
			return c.parseNonDecimal(v, i+2, base, d.neg)
		}
	}

	haveDP := false
	if v[i] == '.' {
		haveDP = true
		i++
	}
	if i >= n || !isDigit(v[i]) {
		return d, numSyntax
	}
	var raw []byte
	dweight, dscale := int64(-1), int64(0)
	for i < n {
		ch := v[i]
		switch {
		case isDigit(ch):
			raw = append(raw, ch)
			if haveDP {
				dscale++
			} else {
				dweight++
			}
			i++
			continue
		case ch == '.':
			if haveDP {
				return d, numSyntax
			}
			haveDP = true
			i++
			if c.modern && i < n && v[i] == '_' {
				return d, numSyntax
			}
			continue
		case c.modern && ch == '_':
			i++
			if i >= n || !isDigit(v[i]) {
				return d, numSyntax
			}
			continue
		}
		break
	}
	if i < n && (v[i] == 'e' || v[i] == 'E') {
		i++
		expNeg := false
		if i < n && v[i] == '+' {
			i++
		} else if i < n && v[i] == '-' {
			expNeg = true
			i++
		}
		if i >= n || !isDigit(v[i]) {
			return d, numSyntax
		}
		var exp int64
		for i < n {
			switch {
			case isDigit(v[i]):
				exp = exp*10 + int64(v[i]-'0')
				if exp > math.MaxInt32/2 {
					return d, numOverflowFormat
				}
				i++
				continue
			case c.modern && v[i] == '_':
				i++
				if i >= n || !isDigit(v[i]) {
					return d, numSyntax
				}
				continue
			}
			break
		}
		if expNeg {
			exp = -exp
		}
		dweight += exp
		dscale = max(dscale-exp, 0)
	}
	if r := trailing(v, i); r != numOK {
		return d, r
	}
	lead := 0
	for lead < len(raw) && raw[lead] == '0' {
		lead++
	}
	d.digits = raw[lead:]
	d.point = dweight + 1 - int64(lead)
	d.dscale = dscale
	return d, numOK
}

func (c *numericChecker) parseNonDecimal(v []byte, i, base int, neg bool) (decimal, numError) {
	var digits []byte
	for i < len(v) {
		ch := v[i]
		if dv, ok := digitValue(ch, uint64(base)); ok {
			digits = append(digits, "0123456789abcdef"[dv])
			i++
			continue
		}
		if ch == '_' {
			i++
			if i >= len(v) {
				return decimal{}, numSyntax
			}
			if _, ok := digitValue(v[i], uint64(base)); !ok {
				return decimal{}, numSyntax
			}
			continue
		}
		break
	}
	if len(digits) == 0 {
		return decimal{}, numSyntax
	}
	if r := trailing(v, i); r != numOK {
		return decimal{}, r
	}
	var b big.Int
	b.SetString(string(digits), base)
	dec := []byte(b.String())
	d := decimal{neg: neg}
	if b.Sign() != 0 {
		d.digits = dec
		d.point = int64(len(dec))
	}
	return d, numOK
}

func trailing(v []byte, i int) numError {
	for ; i < len(v); i++ {
		if !isSpace(v[i]) {
			return numSyntax
		}
	}
	return numOK
}

// roundTo rounds d half away from zero to scale fractional digits, as
// round_var does.
func roundTo(d decimal, scale int64) decimal {
	keep := d.point + scale
	if d.kind != numFinite || keep >= int64(len(d.digits)) {
		return d
	}
	if keep < 0 {
		d.digits = nil
		return d
	}
	up := d.digits[keep] >= '5'
	out := append([]byte(nil), d.digits[:keep]...)
	if up {
		j := len(out) - 1
		for ; j >= 0 && out[j] == '9'; j-- {
			out[j] = '0'
		}
		if j >= 0 {
			out[j]++
		} else {
			out = append([]byte{'1'}, out...)
			d.point++
		}
	}
	out = bytes.TrimRight(out, "0")
	if len(out) == 0 {
		d.point = 0
	}
	d.digits = out
	return d
}

func (c *numericChecker) Check(v []byte) Outcome {
	_, out := c.eval(v)
	return out
}

func (c *numericChecker) eval(v []byte) (decimal, Outcome) {
	d, err := c.parse(v)
	switch err {
	case numSyntax:
		return d, syntaxError(c.titleSyntax, v)
	case numOverflowFormat:
		return d, overflowsFormat()
	}
	if c.precision == schema.NoTypmod {
		// make_result fails for weights or display scales beyond what the
		// on-disk format holds; inputs that big are not modelled.
		if d.kind == numFinite && (d.point > 131072 || d.dscale > 16383) {
			return d, unsure("numeric value too large to decide offline")
		}
		return d, valid
	}
	if !c.typmodOK {
		return d, unsure(fmt.Sprintf("numeric(%d,%d) is not checked offline", c.precision, c.scale))
	}
	maxDigits := int64(c.precision - c.scale)
	switch d.kind {
	case numNaN:
		return d, valid
	case numPosInf, numNegInf:
		return d, c.fieldOverflow(fmt.Sprintf("A field with precision %d, scale %d cannot hold an infinite value.", c.precision, c.scale))
	}
	if len(d.digits) > 0 && d.point > maxDigits {
		return d, c.fieldOverflow(c.overflowDetail())
	}
	d = roundTo(d, int64(c.scale))
	if len(d.digits) > 0 && d.point > maxDigits {
		return d, c.fieldOverflow(c.overflowDetail())
	}
	return d, valid
}

func (c *numericChecker) overflowDetail() string {
	maxDigits := c.precision - c.scale
	if maxDigits == 0 {
		return fmt.Sprintf("A field with precision %d, scale %d must round to an absolute value less than 1.", c.precision, c.scale)
	}
	return fmt.Sprintf("A field with precision %d, scale %d must round to an absolute value less than 10^%d.", c.precision, c.scale, maxDigits)
}

func (c *numericChecker) fieldOverflow(detail string) Outcome {
	return Outcome{
		Verdict:  Invalid,
		SQLState: StateNumericRange,
		Title:    "numeric field overflow",
		Message:  "numeric field overflow",
		Detail:   detail,
	}
}

func overflowsFormat() Outcome {
	return Outcome{
		Verdict:  Invalid,
		SQLState: StateNumericRange,
		Title:    "value overflows numeric format",
		Message:  "value overflows numeric format",
	}
}

// Canonical gives a form where equal numerics compare equal: 1.0 and 1.00
// are the same value to the unique index.
func (c *numericChecker) Canonical(dst, v []byte) ([]byte, bool) {
	d, out := c.eval(v)
	if out.Verdict != Valid {
		return dst, false
	}
	switch d.kind {
	case numNaN:
		return append(dst, "NaN"...), true
	case numPosInf:
		return append(dst, "Inf"...), true
	case numNegInf:
		return append(dst, "-Inf"...), true
	}
	digits := bytes.TrimRight(d.digits, "0")
	if len(digits) == 0 {
		return append(dst, '0'), true
	}
	if d.neg {
		dst = append(dst, '-')
	}
	dst = append(dst, digits...)
	dst = append(dst, 'e')
	return strconv.AppendInt(dst, d.point, 10), true
}
