package csvpg

import (
	"bytes"
	"errors"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
)

// ErrUnterminatedQuote is COPY's "unterminated CSV quoted field".
var ErrUnterminatedQuote = errors.New("unterminated CSV quoted field")

// Field is one CSV field after COPY's quote processing.
type Field struct {
	Value []byte
	// Null is set when the raw field contained no quote character and equals
	// the NULL string; a quoted empty string is therefore not NULL.
	Null   bool
	Quoted bool
}

// Splitter cuts record bodies into fields. It keeps scratch memory between
// calls, so each goroutine needs its own.
type Splitter struct {
	delim, quote, escape byte
	null                 []byte
	fields               []Field
	buf                  []byte
}

// NewSplitter returns a Splitter for the given options.
func NewSplitter(opts copyopt.Options) *Splitter {
	return &Splitter{
		delim:  opts.Delimiter,
		quote:  opts.Quote,
		escape: opts.Escape,
		null:   []byte(opts.Null),
	}
}

// Split returns the fields of body, which must not include the line
// terminator. The returned slices are valid until the next call.
func (s *Splitter) Split(body []byte) ([]Field, error) {
	s.fields = s.fields[:0]
	// Values never grow during unquoting, so one allocation of len(body)
	// keeps every field slice stable while later fields are appended.
	if cap(s.buf) < len(body) {
		s.buf = make([]byte, 0, len(body))
	}
	s.buf = s.buf[:0]

	cur := 0
	for {
		start := cur
		outStart := len(s.buf)
		sawQuote, foundDelim, copied := false, false, false
		end := cur

	scan:
		for {
			// Not in quote.
			for {
				end = cur
				if cur >= len(body) {
					break scan
				}
				c := body[cur]
				cur++
				if c == s.delim {
					foundDelim = true
					break scan
				}
				if c == s.quote {
					if !copied {
						s.buf = append(s.buf, body[start:cur-1]...)
						copied = true
					}
					sawQuote = true
					break
				}
				if copied {
					s.buf = append(s.buf, c)
				}
			}
			// In quote.
			for {
				if cur >= len(body) {
					return s.fields, ErrUnterminatedQuote
				}
				c := body[cur]
				cur++
				if c == s.escape && cur < len(body) {
					if next := body[cur]; next == s.escape || next == s.quote {
						s.buf = append(s.buf, next)
						cur++
						continue
					}
				}
				// Checked after the escape test because quote and escape
				// are usually the same character.
				if c == s.quote {
					break
				}
				s.buf = append(s.buf, c)
			}
		}

		var f Field
		if copied {
			f.Value = s.buf[outStart:len(s.buf):len(s.buf)]
		} else {
			f.Value = body[start:end:end]
		}
		f.Quoted = sawQuote
		f.Null = !sawQuote && bytes.Equal(body[start:end], s.null)
		s.fields = append(s.fields, f)
		if !foundDelim {
			return s.fields, nil
		}
	}
}
