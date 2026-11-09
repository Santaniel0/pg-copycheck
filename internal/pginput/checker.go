// Package pginput decides offline whether PostgreSQL's type input functions
// would accept a value.
//
// Every rule here is ported from src/backend/utils/adt in the PostgreSQL
// source. Where a value falls outside what was ported, the checker answers
// Unsure instead of guessing; testdata/corpus.tsv lists the cases and where
// each expected verdict comes from.
package pginput

import (
	"strconv"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/schema"
)

// Verdict is the answer for one value.
type Verdict uint8

const (
	Valid Verdict = iota
	Invalid
	Unsure
)

func (v Verdict) String() string {
	switch v {
	case Valid:
		return "valid"
	case Invalid:
		return "invalid"
	case Unsure:
		return "unsure"
	}
	return "Verdict(" + strconv.Itoa(int(v)) + ")"
}

// SQLSTATE codes used by the input functions this package models.
const (
	StateInvalidText        = "22P02"
	StateNumericRange       = "22003"
	StateRightTruncation    = "22001"
	StateDatetimeOverflow   = "22008"
	StateInvalidDatetime    = "22007"
	StateTZDisplacement     = "22009"
	StateUntranslatableChar = "22P05"
)

// Outcome is a verdict plus, for Invalid, the error PostgreSQL would raise.
// Title is the message without the offending value, for grouping; for
// Unsure it says why the value was not decided.
type Outcome struct {
	Verdict  Verdict
	SQLState string
	Title    string
	Message  string
	Detail   string
	Hint     string
}

var valid = Outcome{}

func unsure(title string) Outcome { return Outcome{Verdict: Unsure, Title: title} }

// Checker validates one column's values. Implementations are safe for
// concurrent use.
type Checker interface {
	Check(v []byte) Outcome
}

// Canonicalizer is implemented by checkers that can give the equality form
// of a value, which the unique-key check hashes. It appends to dst and
// reports false when the form cannot be computed offline.
type Canonicalizer interface {
	Canonical(dst, v []byte) ([]byte, bool)
}

// Settings are the server settings that change how input is read.
type Settings struct {
	Version   copyopt.Version
	DateOrder copyopt.DateOrder
}

// For returns the checker for a column type.
func For(t schema.Type, s Settings) Checker {
	switch t.Kind {
	case schema.TypeInt2:
		return newIntChecker(16, "smallint", s.Version)
	case schema.TypeInt4:
		return newIntChecker(32, "integer", s.Version)
	case schema.TypeInt8:
		return newIntChecker(64, "bigint", s.Version)
	case schema.TypeNumeric:
		return newNumericChecker(t, s.Version)
	case schema.TypeFloat4:
		return newFloatChecker(32)
	case schema.TypeFloat8:
		return newFloatChecker(64)
	case schema.TypeBool:
		return boolChecker{}
	case schema.TypeText:
		return textChecker{limit: schema.NoTypmod}
	case schema.TypeVarchar:
		return newTextChecker(t.Length, "character varying")
	case schema.TypeBpchar:
		return newTextChecker(t.Length, "character")
	case schema.TypeUUID:
		return uuidChecker{}
	case schema.TypeJSON:
		return jsonChecker{binary: false}
	case schema.TypeJSONB:
		return jsonChecker{binary: true}
	case schema.TypeEnum:
		return newEnumChecker(t)
	case schema.TypeDate, schema.TypeTimestamp, schema.TypeTimestampTZ:
		return newDateTimeChecker(t, s.DateOrder)
	}
	return unknownChecker{title: "type " + t.Name + " is not checked offline"}
}

type unknownChecker struct{ title string }

func (c unknownChecker) Check([]byte) Outcome { return unsure(c.title) }

// isSpace is C isspace() in the C locale, which is what the input functions
// use for the bytes that matter here.
func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func trimSpace(v []byte) []byte {
	for len(v) > 0 && isSpace(v[0]) {
		v = v[1:]
	}
	for len(v) > 0 && isSpace(v[len(v)-1]) {
		v = v[:len(v)-1]
	}
	return v
}

// syntaxError builds PostgreSQL's most common input error.
func syntaxError(title string, v []byte) Outcome {
	return Outcome{
		Verdict:  Invalid,
		SQLState: StateInvalidText,
		Title:    title,
		Message:  title + `: "` + string(v) + `"`,
	}
}

func syntaxTitle(typeName string) string { return "invalid input syntax for type " + typeName }
