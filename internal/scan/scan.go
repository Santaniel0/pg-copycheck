// Package scan runs the check: it reads COPY input once, checks records in
// parallel and writes clean and rejected records in input order.
package scan

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/csvpg"
	"github.com/Santaniel0/pg-copycheck/internal/schema"
)

// Config controls a run.
type Config struct {
	Table       *schema.Table
	Columns     []string // COPY column list; empty means every non-generated column
	Options     copyopt.Options
	Workers     int // defaults to GOMAXPROCS
	BatchSize   int // records per batch, default 1024
	CheckUnique bool
	MaxExamples int // per problem group, default 3
	MaxRecord   int // bytes, default csvpg.DefaultMaxRecord
}

// Output receives records as they are classified. Either writer may be nil.
type Output struct {
	// Clean gets every record that passed, byte for byte, plus the header.
	Clean io.Writer
	// Rejects gets one CSV row per rejected record.
	Rejects io.Writer
}

// RejectsHeader is the first row of the rejects file.
var RejectsHeader = []string{"record", "line", "column", "sqlstate", "message", "detail", "raw"}

const batchBytes = 1 << 20

type keyState uint8

const (
	keySkip      keyState = iota // NULL in a NULLS DISTINCT key: never conflicts
	keyUndecided                 // no offline canonical form
	keyReady
)

type keySlot struct {
	sum   [16]byte
	state keyState
}

type recMeta struct {
	num, line             int64
	start, body, end      int // raw = arena[start:end], body = arena[start:body]
	issue                 csvpg.LineIssue
	marker, header, first bool
}

type unsureKey struct {
	col   int
	title string
}

type unsureTally struct {
	count    int64
	examples []Example
}

// batch is the unit passed between stages. All record bytes live in one
// arena so a batch costs a handful of allocations however many records it
// holds.
type batch struct {
	seq         int64
	arena       []byte
	recs        []recMeta
	probs       [][]Problem
	keys        []keySlot
	unsure      map[unsureKey]*unsureTally
	win1251     bool
	maxExamples int
}

func (b *batch) reset() {
	b.arena = b.arena[:0]
	b.recs = b.recs[:0]
	clear(b.probs)
	b.probs = b.probs[:0]
	b.keys = b.keys[:0]
	clear(b.unsure)
	b.win1251 = false
}

func (b *batch) tallyUnsure(h unsureHit, m *recMeta) {
	k := unsureKey{h.col, h.title}
	t := b.unsure[k]
	if t == nil {
		t = &unsureTally{}
		b.unsure[k] = t
	}
	t.count++
	if len(t.examples) < b.maxExamples {
		t.examples = append(t.examples, Example{Record: m.num, Line: m.line, Message: strconv.Quote(string(h.value))})
	}
}

type runner struct {
	cfg  Config
	opts copyopt.Options
	plan *plan

	slots chan struct{}
	pool  sync.Pool

	// Written by the reader before it closes the jobs channel and read by
	// the collector after every worker has finished.
	eol     csvpg.EOL
	bom     bool
	bytes   int64
	tooBig  *csvpg.RecordTooLargeError
	lastNum int64
	lastLn  int64
}

// Run checks the input against cfg.Table. It returns a *SetupError when COPY
// would refuse the command itself, and other errors only for I/O failures
// or cancellation; problems in the data are reported in the Summary.
func Run(ctx context.Context, cfg Config, in io.Reader, out Output) (*Summary, error) {
	start := time.Now()
	if err := cfg.Options.Validate(); err != nil {
		return nil, err
	}
	p, err := newPlan(cfg.Table, cfg.Columns, cfg.Options, cfg.CheckUnique)
	if err != nil {
		return nil, err
	}
	if cfg.Workers <= 0 {
		cfg.Workers = runtime.GOMAXPROCS(0)
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 1024
	}
	if cfg.MaxExamples <= 0 {
		cfg.MaxExamples = 3
	}
	if cfg.MaxRecord <= 0 {
		cfg.MaxRecord = csvpg.DefaultMaxRecord
	}

	r := &runner{
		cfg:   cfg,
		opts:  cfg.Options,
		plan:  p,
		slots: make(chan struct{}, 2*cfg.Workers+2),
	}
	r.pool.New = func() any {
		return &batch{unsure: map[unsureKey]*unsureTally{}, maxExamples: cfg.MaxExamples}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	jobs := make(chan *batch, cfg.Workers)
	done := make(chan *batch, cfg.Workers)
	var readErr error
	var wg sync.WaitGroup
	wg.Go(func() {
		defer close(jobs)
		if err := r.read(ctx, in, jobs); err != nil {
			readErr = err
			cancel()
		}
	})
	var workers sync.WaitGroup
	for range cfg.Workers {
		workers.Go(func() {
			w := newWorker(p, cfg.Options)
			for b := range jobs {
				for i := range b.recs {
					if !b.recs[i].header {
						w.check(b, i)
					}
				}
				select {
				case done <- b:
				case <-ctx.Done():
					return
				}
			}
		})
	}
	go func() {
		workers.Wait()
		close(done)
	}()

	c := newCollector(r, out)
	pending := map[int64]*batch{}
	next := int64(0)
	var collectErr error
	for b := range done {
		pending[b.seq] = b
		for {
			nb, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if collectErr == nil {
				if collectErr = c.collect(nb); collectErr != nil {
					cancel()
				}
			}
			r.release(nb)
		}
	}
	wg.Wait()

	// A write failure cancels the reader too, so report it before the
	// cancellation it caused.
	if collectErr != nil {
		return nil, collectErr
	}
	if readErr != nil {
		return nil, readErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s, err := c.finish()
	if err != nil {
		return nil, err
	}
	s.Elapsed = time.Since(start)
	return s, nil
}

func (r *runner) acquire(ctx context.Context, seq int64) *batch {
	select {
	case r.slots <- struct{}{}:
	case <-ctx.Done():
		return nil
	}
	b := r.pool.Get().(*batch)
	b.seq = seq
	return b
}

func (r *runner) release(b *batch) {
	if cap(b.arena) > 8*batchBytes {
		b.arena = nil
	}
	b.reset()
	r.pool.Put(b)
	<-r.slots
}

// read finds record boundaries and hands out batches. It is the only
// sequential stage besides the collector.
func (r *runner) read(ctx context.Context, in io.Reader, jobs chan<- *batch) error {
	rd := csvpg.NewReader(in, r.opts)
	rd.SetMaxRecord(r.cfg.MaxRecord)
	header := csvpg.NewSplitter(r.opts)
	nk := len(r.plan.keys)
	headerPending := r.opts.Header != copyopt.HeaderNone

	var seq int64
	b := r.acquire(ctx, seq)
	if b == nil {
		return ctx.Err()
	}
	send := func() bool {
		select {
		case jobs <- b:
			seq++
			return true
		case <-ctx.Done():
			return false
		}
	}
	defer func() { r.eol = rd.EOL() }()

	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		var tooBig *csvpg.RecordTooLargeError
		if errors.As(err, &tooBig) {
			r.tooBig = tooBig
			break
		}
		if err != nil {
			return fmt.Errorf("read input: %w", err)
		}
		r.bytes += int64(len(rec.Raw))
		r.lastNum, r.lastLn = rec.Num, rec.EndLine
		if rec.Num == 1 && bytes.HasPrefix(rec.Raw, []byte("\xef\xbb\xbf")) {
			r.bom = true
		}

		m := recMeta{num: rec.Num, line: rec.Line, issue: rec.Issue, marker: rec.IsEndMarker(), first: rec.Num == 1}
		m.start = len(b.arena)
		b.arena = append(b.arena, rec.Raw...)
		m.body, m.end = m.start+rec.BodyLen, len(b.arena)
		var probs []Problem
		if headerPending {
			headerPending = false
			m.header = true
			probs = r.checkHeader(header, rec)
		}
		b.recs = append(b.recs, m)
		b.probs = append(b.probs, probs)
		for range nk {
			b.keys = append(b.keys, keySlot{})
		}

		if len(b.recs) >= r.cfg.BatchSize || len(b.arena) >= batchBytes {
			if !send() {
				return ctx.Err()
			}
			if b = r.acquire(ctx, seq); b == nil {
				return ctx.Err()
			}
		}
	}
	if len(b.recs) == 0 {
		r.release(b)
		return nil
	}
	if !send() {
		return ctx.Err()
	}
	return nil
}

// checkHeader applies HEADER MATCH (NextCopyFromRawFields). HEADER true
// skips the line without looking at it, but its bytes still go through
// encoding verification.
func (r *runner) checkHeader(sp *csvpg.Splitter, rec csvpg.Record) []Problem {
	if seq, bad := invalidUTF8(rec.Raw); bad {
		return []Problem{encodingProblem(seq)}
	}
	if rec.Issue != csvpg.IssueNone {
		return []Problem{copyFormatProblem(rec.Issue.String(), rec.Issue.Hint())}
	}
	if r.opts.Header != copyopt.HeaderMatch {
		return nil
	}
	fields, err := sp.Split(rec.Body())
	if err != nil {
		return []Problem{copyFormatProblem(err.Error(), "")}
	}
	cols := r.plan.cols
	if len(fields) != len(cols) {
		return []Problem{copyFormatProblem(fmt.Sprintf("wrong number of fields in header line: got %d, expected %d", len(fields), len(cols)), "")}
	}
	for i, f := range fields {
		switch {
		case f.Null:
			return []Problem{copyFormatProblem(fmt.Sprintf("column name mismatch in header line field %d: got null value (%q), expected %q",
				i+1, r.opts.Null, cols[i].Name), "")}
		case string(f.Value) != cols[i].Name:
			return []Problem{copyFormatProblem(fmt.Sprintf("column name mismatch in header line field %d: got %q, expected %q",
				i+1, f.Value, cols[i].Name), "")}
		}
	}
	return nil
}

// collector is the ordered stage: it sees records in input order, which
// makes duplicate detection, the end-of-copy marker and the outputs
// deterministic whatever the number of workers.
type collector struct {
	r       *runner
	agg     *aggregator
	s       *Summary
	clean   *bufio.Writer
	rejects *csv.Writer
	seen    []map[[16]byte]int64
	marker  bool
	rejBuf  *bufio.Writer
}

func newCollector(r *runner, out Output) *collector {
	c := &collector{
		r:   r,
		agg: newAggregator(r.cfg.MaxExamples),
		s: &Summary{
			Table:   r.cfg.Table.Name.String(),
			Options: r.opts,
			Setup:   r.plan.setup,
			Keys:    r.plan.keyStatus,
		},
	}
	for _, col := range r.plan.cols {
		c.s.Columns = append(c.s.Columns, col.Name)
	}
	for range r.plan.keys {
		c.seen = append(c.seen, map[[16]byte]int64{})
	}
	if out.Clean != nil {
		c.clean = bufio.NewWriterSize(out.Clean, 256<<10)
	}
	if out.Rejects != nil {
		c.rejBuf = bufio.NewWriterSize(out.Rejects, 64<<10)
		c.rejects = csv.NewWriter(c.rejBuf)
		c.rejects.Write(RejectsHeader)
	}
	if len(r.plan.setup) > 0 {
		c.s.FirstFailure = &Example{Message: r.plan.setup[0].Message}
	}
	return c
}

func (c *collector) collect(b *batch) error {
	p := c.r.plan
	nk := len(p.keys)
	for i := range b.recs {
		m := &b.recs[i]
		raw := b.arena[m.start:m.end]
		if m.header {
			c.s.HeaderLines = 1
			for _, pr := range b.probs[i] {
				c.fail(pr, m)
			}
			if c.clean != nil {
				if _, err := c.clean.Write(raw); err != nil {
					return fmt.Errorf("write clean output: %w", err)
				}
			}
			continue
		}
		c.s.DataRecords++
		if c.marker {
			c.s.SkippedAfterMarker++
		}
		probs := b.probs[i]
		if m.marker {
			probs = c.endMarker(m, probs)
		}
		if len(probs) == 0 && nk > 0 {
			probs = c.checkKeys(b.keys[i*nk:(i+1)*nk], m)
		}
		if c.r.bom && m.first && len(p.cols) > 0 {
			c.agg.add(Problem{
				Severity: SeverityWarning,
				Title:    "UTF-8 byte order mark at the start of the file",
				Column:   p.cols[0].Name,
				Message:  "there is no header line, so PostgreSQL keeps the bytes EF BB BF as part of the first value",
			}, m.num, m.line)
		}
		if len(probs) == 0 {
			c.s.Clean++
			if c.clean != nil {
				if _, err := c.clean.Write(raw); err != nil {
					return fmt.Errorf("write clean output: %w", err)
				}
			}
			continue
		}
		c.s.Rejected++
		for _, pr := range probs {
			c.fail(pr, m)
		}
		if c.rejects != nil {
			first := probs[0]
			body := b.arena[m.start:m.body]
			if err := c.rejects.Write([]string{
				strconv.FormatInt(m.num, 10), strconv.FormatInt(m.line, 10), first.Column, first.SQLState,
				first.Message, first.Detail, string(body),
			}); err != nil {
				return fmt.Errorf("write rejects: %w", err)
			}
		}
	}
	for k, t := range b.unsure {
		g := c.agg.group(Problem{Severity: SeverityWarning, Title: k.title, Column: p.cols[k.col].Name}, t.examples[0].Record)
		g.Count += t.count
		for _, ex := range t.examples {
			c.agg.example(g, ex)
		}
	}
	c.s.Win1251 = c.s.Win1251 || b.win1251
	return nil
}

func (c *collector) fail(pr Problem, m *recMeta) {
	c.agg.add(pr, m.num, m.line)
	if pr.Severity == SeverityError && c.s.FirstFailure == nil {
		c.s.FirstFailure = &Example{Record: m.num, Line: m.line, Message: pr.Message, Detail: pr.Detail, Hint: pr.Hint}
	}
}

// endMarker handles a line that is "\." alone. Before PostgreSQL 18 COPY
// stops there, so the line is rejected to keep the clean output loadable.
// From 18 on the server reads it as data and only a warning remains.
func (c *collector) endMarker(m *recMeta, probs []Problem) []Problem {
	v := c.r.opts.Version
	if v >= 18 {
		c.agg.add(Problem{
			Severity: SeverityWarning,
			Title:    `line is "\." alone`,
			Message:  fmt.Sprintf(`PostgreSQL %d reads this line as data, but psql still treats it as the end of data when COPY data comes from STDIN`, v),
		}, m.num, m.line)
		return probs
	}
	c.marker = true
	return []Problem{{
		Severity: SeverityDataLoss,
		Title:    `end-of-copy marker "\." ends the data early`,
		Message:  fmt.Sprintf(`line is "\." alone; PostgreSQL %d stops reading CSV here and silently ignores the rest of the file`, v),
	}}
}

func (c *collector) checkKeys(slots []keySlot, m *recMeta) []Problem {
	p := c.r.plan
	for k, slot := range slots {
		switch slot.state {
		case keyUndecided:
			c.s.Keys[p.keys[k].status].Undecided++
		case keyReady:
			if firstLine, dup := c.seen[k][slot.sum]; dup {
				name := p.keys[k].name
				msg := fmt.Sprintf("duplicate key value violates unique constraint %q", name)
				return []Problem{{
					Severity: SeverityError, SQLState: "23505", Title: msg, Message: msg,
					Note: fmt.Sprintf("same key as line %d", firstLine),
				}}
			}
		}
	}
	for k, slot := range slots {
		if slot.state == keyReady {
			c.seen[k][slot.sum] = m.line
		}
	}
	return nil
}

func (c *collector) finish() (*Summary, error) {
	r := c.r
	c.s.Bytes, c.s.EOL, c.s.BOM = r.bytes, r.eol, r.bom
	if r.tooBig != nil {
		c.s.Aborted = r.tooBig.Error()
		pr := Problem{
			Severity: SeverityError,
			Title:    "record too large to read; most likely an unterminated quoted field",
			Message:  r.tooBig.Error(),
		}
		m := &recMeta{num: r.lastNum + 1, line: r.tooBig.Line}
		c.s.Rejected++
		c.fail(pr, m)
		if c.rejects != nil {
			c.rejects.Write([]string{strconv.FormatInt(m.num, 10), strconv.FormatInt(m.line, 10), "", "", pr.Message, "", ""})
		}
	}
	c.s.Groups = c.agg.sorted()
	if c.clean != nil {
		if err := c.clean.Flush(); err != nil {
			return nil, fmt.Errorf("write clean output: %w", err)
		}
	}
	if c.rejects != nil {
		c.rejects.Flush()
		if err := c.rejects.Error(); err != nil {
			return nil, fmt.Errorf("write rejects: %w", err)
		}
		if err := c.rejBuf.Flush(); err != nil {
			return nil, fmt.Errorf("write rejects: %w", err)
		}
	}
	return c.s, nil
}
