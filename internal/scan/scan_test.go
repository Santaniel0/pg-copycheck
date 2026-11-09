package scan

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/fixture"
	"github.com/Santaniel0/pg-copycheck/internal/schema"
)

func table(t *testing.T, ddl, name string) *schema.Table {
	t.Helper()
	cat, err := schema.ParseDDL(strings.NewReader(ddl))
	if err != nil {
		t.Fatal(err)
	}
	tbl, err := cat.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	return tbl
}

type result struct {
	sum     *Summary
	clean   string
	rejects [][]string
}

func run(t *testing.T, cfg Config, input string) result {
	t.Helper()
	var clean, rejects bytes.Buffer
	sum, err := Run(context.Background(), cfg, strings.NewReader(input), Output{Clean: &clean, Rejects: &rejects})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rows, err := csv.NewReader(&rejects).ReadAll()
	if err != nil {
		t.Fatalf("rejects file is not valid CSV: %v", err)
	}
	return result{sum, clean.String(), rows[1:]}
}

func groupTitles(s *Summary) []string {
	var out []string
	for _, g := range s.Groups {
		out = append(out, fmt.Sprintf("%s|%s|%s|%d", g.Severity, g.Column, g.Title, g.Count))
	}
	return out
}

func TestFixtureEndToEnd(t *testing.T) {
	var data bytes.Buffer
	injected, err := fixture.Generate(&data, fixture.Config{Rows: 20000, Seed: 7, FaultEach: 101, CRLF: true})
	if err != nil {
		t.Fatal(err)
	}
	opts := copyopt.Default()
	opts.Header = copyopt.HeaderMatch
	tbl := table(t, fixture.DDL, "orders")

	base := Config{Table: tbl, Options: opts, CheckUnique: true, BatchSize: 37, MaxExamples: 5}
	one, eight := base, base
	one.Workers, eight.Workers = 1, 8
	r1 := run(t, one, data.String())
	r8 := run(t, eight, data.String())

	if r1.clean != r8.clean {
		t.Fatal("clean output differs between 1 and 8 workers")
	}
	if fmt.Sprint(r1.rejects) != fmt.Sprint(r8.rejects) {
		t.Fatal("rejects differ between 1 and 8 workers")
	}
	if fmt.Sprint(groupTitles(r1.sum)) != fmt.Sprint(groupTitles(r8.sum)) {
		t.Fatalf("summaries differ:\n%v\n%v", groupTitles(r1.sum), groupTitles(r8.sum))
	}

	s := r1.sum
	if s.DataRecords != 20000 || s.Clean+s.Rejected != s.DataRecords {
		t.Fatalf("records: data=%d clean=%d rejected=%d", s.DataRecords, s.Clean, s.Rejected)
	}
	rejected := map[int64]bool{}
	for _, row := range r1.rejects {
		n, _ := strconv.ParseInt(row[0], 10, 64)
		rejected[n] = true
	}
	want := 0
	for _, inj := range injected {
		if inj.Fault.Rejected() {
			want++
			if !rejected[inj.Record] {
				t.Errorf("fault %q at record %d was not rejected", inj.Fault, inj.Record)
			}
		} else if rejected[inj.Record] {
			t.Errorf("record %d (%q) rejected, but PostgreSQL accepts it", inj.Record, inj.Fault)
		}
	}
	if len(rejected) != want {
		t.Errorf("rejected %d records, injected %d rejectable faults", len(rejected), want)
	}
	if !s.Win1251 {
		t.Error("Windows-1251 hint not set")
	}
	if s.EOL.String() != "CRLF" {
		t.Errorf("EOL = %v", s.EOL)
	}

	// The clean output must itself pass.
	again := run(t, one, r1.clean)
	if again.sum.CopyWouldFail() || again.sum.Rejected != 0 {
		t.Fatalf("clean output fails a second check: %v", groupTitles(again.sum))
	}
	if again.clean != r1.clean {
		t.Fatal("second pass changed the clean output")
	}
}

func TestDayFirstDatesPassWithDMY(t *testing.T) {
	var data bytes.Buffer
	if _, err := fixture.Generate(&data, fixture.Config{Rows: 3000, Seed: 1, FaultEach: 7}); err != nil {
		t.Fatal(err)
	}
	opts := copyopt.Default()
	opts.Header = copyopt.HeaderSkip
	tbl := table(t, fixture.DDL, "orders")
	count := func(order copyopt.DateOrder) int64 {
		opts.DateOrder = order
		for _, g := range run(t, Config{Table: tbl, Options: opts}, data.String()).sum.Groups {
			if g.Column == "created_at" && g.Severity == SeverityError {
				return g.Count
			}
		}
		return 0
	}
	if mdy, dmy := count(copyopt.DateOrderMDY), count(copyopt.DateOrderDMY); mdy == 0 || dmy != 0 {
		t.Fatalf("created_at errors: MDY %d, DMY %d", mdy, dmy)
	}
}

const smallDDL = `
create table t (
  id integer primary key,
  name varchar(5) not null,
  qty integer,
  code char(2) unique,
  made timestamp(0) generated always as (now()) stored,
  note text not null default '',
  tags text[]
);`

func TestCases(t *testing.T) {
	tbl := table(t, smallDDL, "t")
	v18 := copyopt.Default()
	v18.Version = 18
	match := copyopt.Default()
	match.Header = copyopt.HeaderMatch
	forced := copyopt.Default()
	forced.ForceNotNull = copyopt.ColumnSet{Names: []string{"qty"}}
	forcedNull := copyopt.Default()
	forcedNull.ForceNull = copyopt.ColumnSet{All: true}

	tests := []struct {
		name     string
		opts     copyopt.Options
		cols     []string
		unique   bool
		input    string
		first    string // FirstFailure message, "" for none
		groups   []string
		clean    string
		rejected int64
	}{
		{
			name:   "quoted empty string is not NULL",
			input:  "1,ab,\"\",xy,n\n",
			first:  `invalid input syntax for type integer: ""`,
			groups: []string{`error|qty|invalid input syntax for type integer|1`}, rejected: 1,
		},
		{
			name:  "unquoted empty is NULL",
			input: "1,ab,,xy,n\n", clean: "1,ab,,xy,n\n",
		},
		{
			name:   "FORCE_NOT_NULL turns NULL into an empty string",
			opts:   forced,
			input:  "1,ab,,xy,n\n",
			first:  `invalid input syntax for type integer: ""`,
			groups: []string{`error|qty|invalid input syntax for type integer|1`}, rejected: 1,
		},
		{
			name:   "FORCE_NULL makes a quoted empty name NULL",
			opts:   forcedNull,
			input:  "1,\"\",2,xy,n\n",
			first:  `null value in column "name" of relation "t" violates not-null constraint`,
			groups: []string{`error|name|null value in column "name" of relation "t" violates not-null constraint|1`}, rejected: 1,
		},
		{
			name:  "conversion errors come before NOT NULL",
			input: "x,,2,xy,n\n",
			first: `invalid input syntax for type integer: "x"`,
			groups: []string{
				`error|id|invalid input syntax for type integer|1`,
				`error|name|null value in column "name" of relation "t" violates not-null constraint|1`,
			},
			rejected: 1,
		},
		{
			name:   "extra and missing data",
			input:  "1,ab,2,xy,n,extra\n2,ab\n",
			first:  "extra data after last expected column",
			groups: []string{`error||extra data after last expected column|1`, `error|qty|missing data for column "qty"|1`}, rejected: 2,
		},
		{
			name:   "varchar limit counts characters",
			input:  "1,абвгд,2,xy,n\n2,абвгде,2,xz,n\n",
			first:  "value too long for type character varying(5)",
			groups: []string{`error|name|value too long for type character varying(5)|1`},
			clean:  "1,абвгд,2,xy,n\n", rejected: 1,
		},
		{
			name:   "unterminated quote",
			input:  "1,ab,2,xy,n\n2,\"ab,2,xy,n\n",
			first:  "unterminated CSV quoted field",
			groups: []string{`error||unterminated CSV quoted field|1`},
			clean:  "1,ab,2,xy,n\n", rejected: 1,
		},
		{
			name:   "stray carriage return in an LF file",
			input:  "1,ab,2,xy,n\n2,ab,2,xz,n\r\n",
			first:  "unquoted carriage return found in data",
			groups: []string{`error||unquoted carriage return found in data|1`}, rejected: 1,
		},
		{
			name:   "NUL byte",
			input:  "1,a\x00b,2,xy,n\n",
			first:  `invalid byte sequence for encoding "UTF8": 0x00`,
			groups: []string{`error||invalid byte sequence for encoding "UTF8"|1`}, rejected: 1,
		},
		{
			name:   "duplicate keys compare typed values",
			unique: true,
			input:  "17,ab,1,xy,n\n 17,cd,1,xz,n\n18,ef,1,xy ,n\n",
			first:  `duplicate key value violates unique constraint "t_pkey"`,
			groups: []string{
				`error||duplicate key value violates unique constraint "t_pkey"|1`,
				`error||duplicate key value violates unique constraint "t_code_key"|1`,
			},
			clean: "17,ab,1,xy,n\n", rejected: 2,
		},
		{
			name:   "end-of-copy marker before PostgreSQL 18",
			input:  "1,ab,2,xy,n\n\\.\n2,ab,2,xz,n\n",
			groups: []string{`data loss||end-of-copy marker "\." ends the data early|1`},
			clean:  "1,ab,2,xy,n\n2,ab,2,xz,n\n", rejected: 1,
		},
		{
			name:  "end-of-copy marker is data in PostgreSQL 18",
			opts:  v18,
			input: "1,ab,2,xy,n\n\\.\n",
			first: `invalid input syntax for type integer: "\."`,
			groups: []string{
				`error|id|invalid input syntax for type integer|1`,
				`error|name|missing data for column "name"|1`,
				`warning||line is "\." alone|1`,
			},
			clean: "1,ab,2,xy,n\n", rejected: 1,
		},
		{
			name:   "header match",
			opts:   match,
			cols:   []string{"id", "name"},
			input:  "id,Name\n1,ab\n",
			first:  `column name mismatch in header line field 2: got "Name", expected "name"`,
			groups: []string{`error||column name mismatch in header line field 2: got "Name", expected "name"|1`},
			clean:  "id,Name\n1,ab\n",
		},
		{
			name:   "BOM without header",
			input:  "\xef\xbb\xbf1,ab,2,xy,n\n",
			first:  "invalid input syntax for type integer: \"\xef\xbb\xbf1\"",
			groups: []string{`error|id|invalid input syntax for type integer|1`, `warning|id|UTF-8 byte order mark at the start of the file|1`}, rejected: 1,
		},
		{
			name:   "unchecked values do not reject",
			cols:   []string{"id", "name", "note"},
			input:  "1,ab,x\n",
			clean:  "1,ab,x\n",
			groups: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := tt.opts
			if opts.Delimiter == 0 {
				opts = copyopt.Default()
			}
			cols := tt.cols
			if cols == nil {
				cols = []string{"id", "name", "qty", "code", "note"}
			}
			r := run(t, Config{Table: tbl, Columns: cols, Options: opts, CheckUnique: tt.unique, Workers: 2}, tt.input)
			first := ""
			if r.sum.FirstFailure != nil {
				first = r.sum.FirstFailure.Message
			}
			if first != tt.first {
				t.Errorf("first failure = %q, want %q", first, tt.first)
			}
			if got := groupTitles(r.sum); fmt.Sprint(got) != fmt.Sprint(tt.groups) {
				t.Errorf("groups =\n%q\nwant\n%q", got, tt.groups)
			}
			if tt.clean != "" && r.clean != tt.clean {
				t.Errorf("clean = %q, want %q", r.clean, tt.clean)
			}
			if r.sum.Rejected != tt.rejected {
				t.Errorf("rejected = %d, want %d", r.sum.Rejected, tt.rejected)
			}
		})
	}
}

func TestSetup(t *testing.T) {
	tbl := table(t, smallDDL, "t")
	tests := []struct {
		cols []string
		err  string
	}{
		{[]string{"id", "nope"}, `column "nope" of relation "t" does not exist`},
		{[]string{"id", "id"}, `column "id" specified more than once`},
		{[]string{"id", "made"}, `column "made" is a generated column (Generated columns cannot be used in COPY.)`},
	}
	for _, tt := range tests {
		_, err := Run(context.Background(), Config{Table: tbl, Columns: tt.cols, Options: copyopt.Default()}, strings.NewReader(""), Output{})
		var se *SetupError
		if !errors.As(err, &se) || err.Error() != tt.err {
			t.Errorf("columns %v: err = %v, want %q", tt.cols, err, tt.err)
		}
	}

	// A NOT NULL column without a default left out of the list fails every
	// row, which is reported once.
	r := run(t, Config{Table: tbl, Columns: []string{"id", "qty"}, Options: copyopt.Default()}, "1,2\n")
	if len(r.sum.Setup) != 1 || !r.sum.CopyWouldFail() ||
		r.sum.Setup[0].Message != `null value in column "name" of relation "t" violates not-null constraint` {
		t.Fatalf("setup = %+v", r.sum.Setup)
	}
}

func TestRecordTooLarge(t *testing.T) {
	tbl := table(t, smallDDL, "t")
	input := "1,ab,2,xy,n\n2,\"" + strings.Repeat("x", 4000)
	r := run(t, Config{Table: tbl, Columns: []string{"id", "name", "qty", "code", "note"}, Options: copyopt.Default(), MaxRecord: 1000}, input)
	if r.sum.Aborted == "" || r.sum.Clean != 1 || r.sum.Rejected != 1 || len(r.rejects) != 1 || r.rejects[0][1] != "2" {
		t.Fatalf("aborted=%q clean=%d rejected=%d rejects=%v", r.sum.Aborted, r.sum.Clean, r.sum.Rejected, r.rejects)
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

func TestWriteErrorStopsRun(t *testing.T) {
	var data bytes.Buffer
	if _, err := fixture.Generate(&data, fixture.Config{Rows: 50000, Seed: 3}); err != nil {
		t.Fatal(err)
	}
	opts := copyopt.Default()
	opts.Header = copyopt.HeaderSkip
	boom := errors.New("disk full")
	_, err := Run(context.Background(), Config{Table: table(t, fixture.DDL, "orders"), Options: opts, Workers: 4},
		&data, Output{Clean: failingWriter{boom}})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

// endless yields "n,x\n" rows forever and counts what has been read.
type endless struct {
	n    int64
	read atomic.Int64
}

func (e *endless) Read(p []byte) (int, error) {
	total := 0
	for len(p)-total > 32 {
		e.n++
		total += copy(p[total:], strconv.FormatInt(e.n, 10)+",x\n")
	}
	e.read.Add(int64(total))
	return total, nil
}

type blockingWriter struct {
	release chan struct{}
	writes  atomic.Int64
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	if w.writes.Add(1) == 1 {
		<-w.release
	}
	return len(p), nil
}

// TestBackpressure checks that a stalled output stops the reader instead of
// buffering the input in memory.
func TestBackpressure(t *testing.T) {
	tbl := table(t, "create table p (id bigint, v text);", "p")
	src := &endless{}
	out := &blockingWriter{release: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, Config{Table: tbl, Options: copyopt.Default(), Workers: 4, BatchSize: 256}, src, Output{Clean: out})
		done <- err
	}()

	// Wait for the pipeline to fill up and stall.
	var last int64 = -1
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		cur := src.read.Load()
		if cur == last && cur > 0 {
			break
		}
		last = cur
	}
	stalledAt := src.read.Load()
	cancel()
	close(out.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// 10 batches of 256 short rows plus the clean writer's buffer and the
	// reader's buffer: far below this limit if memory is bounded.
	if stalledAt > 4<<20 {
		t.Fatalf("read %d bytes while the output was blocked", stalledAt)
	}
}

func TestCancelledBeforeStart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Run(ctx, Config{Table: table(t, "create table p (id int);", "p"), Options: copyopt.Default()},
		strings.NewReader("1\n2\n"), Output{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

var _ io.Writer = (*blockingWriter)(nil)
