package scan

import (
	"cmp"
	"slices"
	"strconv"
	"time"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/csvpg"
)

// Severity says what a problem means for the load.
type Severity uint8

const (
	// SeverityError: COPY would stop with this error.
	SeverityError Severity = iota + 1
	// SeverityDataLoss: COPY would succeed but silently skip data.
	SeverityDataLoss
	// SeverityWarning: needs a look; COPY may well succeed.
	SeverityWarning
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityDataLoss:
		return "data loss"
	case SeverityWarning:
		return "warning"
	}
	return "Severity(" + strconv.Itoa(int(s)) + ")"
}

// Problem is one finding. Message, Detail and Hint carry PostgreSQL's own
// wording when SQLState is set; Note is context added by this tool.
type Problem struct {
	Severity Severity
	SQLState string
	Title    string
	Column   string
	Message  string
	Detail   string
	Hint     string
	Note     string
}

// Example is one occurrence of a problem.
type Example struct {
	Record  int64
	Line    int64
	Message string
	Detail  string
	Hint    string
	Note    string
}

// Group collects the occurrences of one problem in one column.
type Group struct {
	Severity Severity
	SQLState string
	Title    string
	Column   string
	Count    int64
	Examples []Example
	first    int64
}

// KeyStatus reports how a unique constraint was checked.
type KeyStatus struct {
	Name      string
	Columns   []string
	Checked   bool
	Reason    string // why it was not checked
	Undecided int64  // rows whose key had no offline canonical form
}

// Summary is the result of a run.
type Summary struct {
	Table   string
	Columns []string
	Options copyopt.Options

	Bytes       int64
	EOL         csvpg.EOL
	BOM         bool
	HeaderLines int64
	DataRecords int64
	Rejected    int64
	Clean       int64

	// Setup problems come from the COPY command itself rather than from any
	// record, such as a NOT NULL column without a default left out of the
	// column list. They fail every row.
	Setup  []Problem
	Groups []*Group
	// FirstFailure is where COPY would stop; nil when it would not fail.
	FirstFailure *Example
	// SkippedAfterMarker counts records after an end-of-copy marker that
	// PostgreSQL before 18 would never read.
	SkippedAfterMarker int64
	// Win1251 is set when the invalid UTF-8 bytes look like Windows-1251 text.
	Win1251 bool
	Keys    []KeyStatus
	// Aborted is set when the input could not be read to the end.
	Aborted string
	Elapsed time.Duration
}

// CopyWouldFail reports whether COPY would stop with an error.
func (s *Summary) CopyWouldFail() bool {
	if len(s.Setup) > 0 || s.Aborted != "" {
		return true
	}
	for _, g := range s.Groups {
		if g.Severity == SeverityError {
			return true
		}
	}
	return false
}

// LosesData reports whether COPY would succeed while skipping data.
func (s *Summary) LosesData() bool {
	for _, g := range s.Groups {
		if g.Severity == SeverityDataLoss {
			return true
		}
	}
	return false
}

type groupKey struct {
	severity Severity
	state    string
	title    string
	column   string
}

type aggregator struct {
	groups      map[groupKey]*Group
	maxExamples int
}

func newAggregator(maxExamples int) *aggregator {
	return &aggregator{groups: map[groupKey]*Group{}, maxExamples: maxExamples}
}

// group returns the group for p, creating it with first as its first
// occurrence.
func (a *aggregator) group(p Problem, first int64) *Group {
	k := groupKey{p.Severity, p.SQLState, p.Title, p.Column}
	g, ok := a.groups[k]
	if !ok {
		g = &Group{Severity: p.Severity, SQLState: p.SQLState, Title: p.Title, Column: p.Column, first: first}
		a.groups[k] = g
	}
	return g
}

func (a *aggregator) add(p Problem, record, line int64) {
	g := a.group(p, record)
	g.Count++
	a.example(g, Example{record, line, p.Message, p.Detail, p.Hint, p.Note})
}

func (a *aggregator) example(g *Group, ex Example) {
	if len(g.Examples) < a.maxExamples {
		g.Examples = append(g.Examples, ex)
	}
}

// sorted orders groups by severity, then by where they first occur.
func (a *aggregator) sorted() []*Group {
	out := make([]*Group, 0, len(a.groups))
	for _, g := range a.groups {
		out = append(out, g)
	}
	slices.SortFunc(out, func(x, y *Group) int {
		return cmp.Or(
			cmp.Compare(x.Severity, y.Severity),
			cmp.Compare(x.first, y.first),
			cmp.Compare(x.Column, y.Column),
			cmp.Compare(x.Title, y.Title),
		)
	})
	return out
}
