// Package csvpg reads CSV input the way PostgreSQL's COPY FROM does.
//
// The work is split in two steps, as in src/backend/commands/copyfromparse.c:
// Reader finds record boundaries (CopyReadLineText) and Splitter cuts a record
// into fields (CopyReadAttributesCSV). Keeping them apart lets the caller find
// boundaries sequentially and split records in parallel.
package csvpg

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
)

// EOL is the line ending style COPY locks onto at the first line terminator.
type EOL uint8

const (
	EOLUnknown EOL = iota
	EOLNL
	EOLCR
	EOLCRNL
)

func (e EOL) String() string {
	switch e {
	case EOLUnknown:
		return "unknown"
	case EOLNL:
		return "LF"
	case EOLCR:
		return "CR"
	case EOLCRNL:
		return "CRLF"
	}
	return "EOL(" + strconv.Itoa(int(e)) + ")"
}

// LineIssue is a problem COPY reports while looking for the end of a record.
type LineIssue uint8

const (
	IssueNone LineIssue = iota
	// IssueUnquotedCR: a carriage return outside quotes that is not the line
	// ending style of the file.
	IssueUnquotedCR
	// IssueUnquotedLF: a newline outside quotes in a CR or CRLF file.
	IssueUnquotedLF
)

func (i LineIssue) String() string {
	switch i {
	case IssueNone:
		return "none"
	case IssueUnquotedCR:
		return "unquoted carriage return found in data"
	case IssueUnquotedLF:
		return "unquoted newline found in data"
	}
	return "LineIssue(" + strconv.Itoa(int(i)) + ")"
}

// Hint returns the errhint PostgreSQL attaches to the issue.
func (i LineIssue) Hint() string {
	switch i {
	case IssueUnquotedCR:
		return "Use quoted CSV field to represent carriage return."
	case IssueUnquotedLF:
		return "Use quoted CSV field to represent newline."
	}
	return ""
}

// Record is one COPY input line: everything up to and including the line
// terminator, which may span several physical lines when quoted fields
// contain line breaks.
type Record struct {
	Num     int64 // 1-based, counts the header line too
	Line    int64 // physical line where the record starts, 1-based
	EndLine int64 // physical line where the record ends
	Offset  int64 // byte offset of the first byte
	// Raw holds the record including its terminator. It aliases the reader's
	// buffer and is valid only until the next call to Next.
	Raw     []byte
	BodyLen int // len(Raw) without the terminator
	Issue   LineIssue
}

// Body returns the record without its line terminator.
func (r Record) Body() []byte { return r.Raw[:r.BodyLen] }

// Terminated reports whether the record ended with a line terminator rather
// than at end of input.
func (r Record) Terminated() bool { return r.BodyLen < len(r.Raw) }

// IsEndMarker reports whether the record is the end-of-copy marker `\.`
// alone on a terminated line. Before PostgreSQL 18 COPY stops reading CSV
// there; at end of input without a terminator it is treated as data.
func (r Record) IsEndMarker() bool {
	return r.Terminated() && r.BodyLen == 2 && r.Raw[0] == '\\' && r.Raw[1] == '.'
}

// RecordTooLargeError means a record grew past the reader's limit, which in
// practice is an unbalanced quote swallowing the rest of the file.
type RecordTooLargeError struct {
	Line   int64
	Offset int64
	Limit  int
}

func (e *RecordTooLargeError) Error() string {
	return fmt.Sprintf("record starting at line %d (byte %d) is larger than %d bytes; most likely an opening quote is never closed",
		e.Line, e.Offset, e.Limit)
}

// DefaultMaxRecord bounds how much a single record may buffer.
const DefaultMaxRecord = 64 << 20

// Reader finds COPY record boundaries in a byte stream.
type Reader struct {
	src        io.Reader
	buf        []byte
	start, end int
	eof        bool
	readErr    error

	quote, escape byte
	escapeActive  bool
	eol           EOL
	maxRecord     int

	num, line, offset int64
}

// NewReader returns a Reader using the quote and escape characters of opts.
func NewReader(src io.Reader, opts copyopt.Options) *Reader {
	return &Reader{
		src:    src,
		buf:    make([]byte, 64<<10),
		quote:  opts.Quote,
		escape: opts.Escape,
		// CopyReadLineText ignores escape processing when it equals the quote:
		// the quote alone then works as a toggle.
		escapeActive: opts.Escape != opts.Quote,
		maxRecord:    DefaultMaxRecord,
		line:         1,
	}
}

// SetMaxRecord changes the record size limit.
func (r *Reader) SetMaxRecord(n int) { r.maxRecord = n }

// EOL returns the line ending style detected so far.
func (r *Reader) EOL() EOL { return r.eol }

// fill makes more input available, compacting the buffer first. It returns
// false when no more data will ever arrive.
func (r *Reader) fill() bool {
	if r.eof {
		return false
	}
	if r.start > 0 {
		n := copy(r.buf, r.buf[r.start:r.end])
		r.start, r.end = 0, n
	}
	if r.end == len(r.buf) {
		nb := make([]byte, 2*len(r.buf))
		copy(nb, r.buf[:r.end])
		r.buf = nb
	}
	for {
		n, err := r.src.Read(r.buf[r.end:])
		r.end += n
		if err != nil {
			r.eof = true
			if !errors.Is(err, io.EOF) {
				r.readErr = err
			}
			return n > 0
		}
		if n > 0 {
			return true
		}
	}
}

// byteAt returns the byte at pos relative to r.start, reading more input if
// needed.
func (r *Reader) byteAt(pos int) (byte, bool) {
	for r.start+pos >= r.end {
		if pos >= r.maxRecord+2 {
			return 0, false
		}
		if !r.fill() {
			return 0, false
		}
	}
	return r.buf[r.start+pos], true
}

// Next returns the next record. It returns io.EOF after the last record and
// *RecordTooLargeError when a record exceeds the size limit.
func (r *Reader) Next() (Record, error) {
	if _, ok := r.byteAt(0); !ok {
		if r.readErr != nil {
			return Record{}, r.readErr
		}
		return Record{}, io.EOF
	}

	var (
		pos      int
		inQuote  bool
		lastEsc  bool
		issue    LineIssue
		bodyLen  = -1
		quote    = r.quote
		esc      = r.escape
		escapeOn = r.escapeActive
	)
	for bodyLen < 0 {
		c, ok := r.byteAt(pos)
		if !ok {
			if r.readErr != nil {
				return Record{}, r.readErr
			}
			if pos > r.maxRecord {
				return Record{}, &RecordTooLargeError{Line: r.line, Offset: r.offset, Limit: r.maxRecord}
			}
			bodyLen = pos
			break
		}
		if pos > r.maxRecord {
			return Record{}, &RecordTooLargeError{Line: r.line, Offset: r.offset, Limit: r.maxRecord}
		}
		pos++

		if escapeOn && inQuote && c == esc {
			lastEsc = !lastEsc
		}
		if c == quote && !lastEsc {
			inQuote = !inQuote
		}
		if !escapeOn || c != esc {
			lastEsc = false
		}
		if inQuote {
			continue
		}

		switch c {
		case '\r':
			switch r.eol {
			case EOLUnknown, EOLCRNL:
				if next, ok := r.byteAt(pos); ok && next == '\n' {
					r.eol = EOLCRNL
					bodyLen = pos - 1
					pos++
				} else if r.eol == EOLCRNL {
					// COPY raises an error here. Keep scanning as if it were
					// data so one stray byte costs one record, not a cascade.
					issue = firstIssue(issue, IssueUnquotedCR)
				} else {
					r.eol = EOLCR
					bodyLen = pos - 1
				}
			case EOLNL:
				issue = firstIssue(issue, IssueUnquotedCR)
			case EOLCR:
				bodyLen = pos - 1
			}
		case '\n':
			switch r.eol {
			case EOLCR, EOLCRNL:
				issue = firstIssue(issue, IssueUnquotedLF)
			default:
				r.eol = EOLNL
				bodyLen = pos - 1
			}
		}
	}

	raw := r.buf[r.start : r.start+pos]
	breaks := countLineBreaks(raw)
	rec := Record{
		Num:     r.num + 1,
		Line:    r.line,
		EndLine: r.line + breaks,
		Offset:  r.offset,
		Raw:     raw,
		BodyLen: bodyLen,
		Issue:   issue,
	}
	if rec.Terminated() && breaks > 0 {
		rec.EndLine--
	}
	r.num++
	r.line += breaks
	r.offset += int64(pos)
	r.start += pos
	return rec, nil
}

func firstIssue(cur, next LineIssue) LineIssue {
	if cur != IssueNone {
		return cur
	}
	return next
}

// countLineBreaks counts physical line breaks the way an editor shows them:
// "\r\n", a lone "\r" and a lone "\n" each count once.
func countLineBreaks(b []byte) int64 {
	n := int64(bytes.Count(b, []byte{'\n'}))
	if bytes.IndexByte(b, '\r') < 0 {
		return n
	}
	for i, c := range b {
		if c == '\r' && (i+1 == len(b) || b[i+1] != '\n') {
			n++
		}
	}
	return n
}
