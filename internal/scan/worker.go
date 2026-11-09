package scan

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/csvpg"
	"github.com/Santaniel0/pg-copycheck/internal/pginput"
)

const stateBadCopyFormat = "22P04"

func copyFormatProblem(msg, hint string) Problem {
	return Problem{Severity: SeverityError, SQLState: stateBadCopyFormat, Title: msg, Message: msg, Hint: hint}
}

// invalidUTF8 finds the first byte sequence PostgreSQL's UTF-8 verification
// rejects and formats it like report_invalid_encoding in mbutils.c: as many
// bytes as the lead byte announces, "0x%02x" each.
func invalidUTF8(raw []byte) (string, bool) {
	if utf8.Valid(raw) && bytes.IndexByte(raw, 0) < 0 {
		return "", false
	}
	for i := 0; i < len(raw); {
		c := raw[i]
		if c == 0 {
			return "0x00", true
		}
		if c < utf8.RuneSelf {
			i++
			continue
		}
		r, size := utf8.DecodeRune(raw[i:])
		if r != utf8.RuneError || size > 1 {
			i += size
			continue
		}
		n := 1
		switch {
		case c&0xe0 == 0xc0:
			n = 2
		case c&0xf0 == 0xe0:
			n = 3
		case c&0xf8 == 0xf0:
			n = 4
		}
		n = min(n, len(raw)-i)
		parts := make([]string, n)
		for k := range n {
			parts[k] = fmt.Sprintf("0x%02x", raw[i+k])
		}
		return strings.Join(parts, " "), true
	}
	return "", false
}

// looksWin1251 is a heuristic: nearly all high bytes fall in the Cyrillic
// letter range of Windows-1251.
func looksWin1251(raw []byte) bool {
	high, cyr := 0, 0
	for _, c := range raw {
		if c >= 0x80 {
			high++
			if c >= 0xc0 || c == 0xa8 || c == 0xb8 {
				cyr++
			}
		}
	}
	return high > 0 && cyr*10 >= high*9
}

func encodingProblem(seq string) Problem {
	return Problem{
		Severity: SeverityError,
		SQLState: "22021",
		Title:    `invalid byte sequence for encoding "UTF8"`,
		Message:  `invalid byte sequence for encoding "UTF8": ` + seq,
	}
}

// quoteValue quotes v for display, cut to 60 characters.
func quoteValue(v []byte) string {
	const limit = 60
	if utf8.RuneCount(v) <= limit {
		return strconv.Quote(string(v))
	}
	cut, n := 0, 0
	for cut < len(v) && n < limit {
		_, size := utf8.DecodeRune(v[cut:])
		cut += size
		n++
	}
	return strconv.Quote(string(v[:cut])) + "..."
}

type unsureHit struct {
	col   int
	title string
	value []byte
}

// worker checks records. Each goroutine owns one, since the splitter and
// the scratch slices are reused between records.
type worker struct {
	plan    *plan
	version copyopt.Version
	split   *csvpg.Splitter
	isNull  []bool
	vals    [][]byte
	hits    []unsureHit
	hasher  hash.Hash
	sum     []byte
}

func newWorker(p *plan, opts copyopt.Options) *worker {
	return &worker{
		plan:    p,
		version: opts.Version,
		split:   csvpg.NewSplitter(opts),
		isNull:  make([]bool, len(p.cols)),
		vals:    make([][]byte, len(p.cols)),
		hasher:  fnv.New128a(),
	}
}

// check validates record i of b. The order of the problems follows the
// order in which COPY would hit them, so the first one is what COPY reports.
func (w *worker) check(b *batch, i int) {
	m := &b.recs[i]
	raw := b.arena[m.start:m.end]
	if seq, bad := invalidUTF8(raw); bad {
		b.probs[i] = []Problem{encodingProblem(seq)}
		b.win1251 = b.win1251 || looksWin1251(raw)
		return
	}
	if m.issue != csvpg.IssueNone {
		b.probs[i] = []Problem{copyFormatProblem(m.issue.String(), m.issue.Hint())}
		return
	}
	if m.marker && w.version < 18 {
		return // the collector reports it, since it affects every later record
	}

	p := w.plan
	body := b.arena[m.start:m.body]
	if len(p.cols) == 0 {
		if len(body) != 0 {
			b.probs[i] = []Problem{copyFormatProblem("extra data after last expected column", "")}
		}
		return
	}
	fields, err := w.split.Split(body)
	if err != nil {
		b.probs[i] = []Problem{copyFormatProblem(csvpg.ErrUnterminatedQuote.Error(), "")}
		return
	}
	if len(fields) > len(p.cols) {
		b.probs[i] = []Problem{copyFormatProblem("extra data after last expected column", "")}
		return
	}

	var probs []Problem
	w.hits = w.hits[:0]
	seen := len(p.cols)
	for c, col := range p.cols {
		if c >= len(fields) {
			msg := fmt.Sprintf("missing data for column %q", col.Name)
			probs = append(probs, Problem{Severity: SeverityError, SQLState: stateBadCopyFormat, Title: msg, Column: col.Name, Message: msg})
			seen = c
			break
		}
		f := fields[c]
		null, val := f.Null, f.Value
		switch {
		case null && p.forceNotNull[c]:
			null, val = false, p.nullStr
		case !null && p.forceNull[c] && bytes.Equal(val, p.nullStr):
			null = true
		}
		w.isNull[c], w.vals[c] = null, val
		if null {
			continue
		}
		out := p.checkers[c].Check(val)
		switch out.Verdict {
		case pginput.Invalid:
			pr := Problem{
				Severity: SeverityError, SQLState: out.SQLState, Title: out.Title, Column: col.Name,
				Message: out.Message, Detail: out.Detail, Hint: out.Hint,
			}
			// Some messages (json, length and numeric overflow) do not quote
			// the value, which is the first thing one wants to see.
			if len(val) > 0 && !strings.Contains(out.Message, string(val)) {
				pr.Note = "value: " + quoteValue(val)
			}
			probs = append(probs, pr)
		case pginput.Unsure:
			w.hits = append(w.hits, unsureHit{c, out.Title, val})
		}
	}
	// NOT NULL is enforced after the whole row is formed, so it comes after
	// every conversion error.
	for c := range seen {
		if w.isNull[c] && p.notNull[c] {
			probs = append(probs, notNullProblem(p.cols[c].Name, p.rel))
		}
	}
	if len(probs) > 0 {
		b.probs[i] = probs
		return
	}
	for _, h := range w.hits {
		b.tallyUnsure(h, m)
	}
	w.keys(b, i)
}

// keys stores a 128-bit hash of each unique key's canonical values. Equal
// hashes are treated as equal keys; with FNV-128a over inputs this small a
// false match is not a practical concern.
func (w *worker) keys(b *batch, i int) {
	nk := len(w.plan.keys)
	for k, kp := range w.plan.keys {
		slot := &b.keys[i*nk+k]
		w.hasher.Reset()
		state := keyReady
		for j, c := range kp.cols {
			if w.isNull[c] {
				if !kp.nullsNotDistinct {
					state = keySkip
					break
				}
				w.hasher.Write([]byte{0})
				continue
			}
			canon, ok := kp.canon[j].Canonical(w.sum[:0], w.vals[c])
			w.sum = canon
			if !ok {
				state = keyUndecided
				break
			}
			var lenBuf [binary.MaxVarintLen64 + 1]byte
			lenBuf[0] = 1
			n := binary.PutUvarint(lenBuf[1:], uint64(len(canon)))
			w.hasher.Write(lenBuf[:n+1])
			w.hasher.Write(canon)
		}
		slot.state = state
		if state == keyReady {
			w.hasher.Sum(slot.sum[:0])
		}
	}
}
