// Package report renders a scan.Summary for people (Text) and programs
// (JSON).
package report

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Santaniel0/pg-copycheck/internal/scan"
)

// Meta is what the summary does not know about the run.
type Meta struct {
	Input       string
	CleanPath   string
	RejectsPath string
}

// printable escapes control characters and invalid UTF-8 so values cannot
// mess up the terminal; everything else, Cyrillic included, is kept.
func printable(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size <= 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == 0xfeff:
			b.WriteString(`\ufeff`)
		case !unicode.IsPrint(r) && r != ' ':
			fmt.Fprintf(&b, `\x%02x`, s[i])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func size(n int64) string {
	switch {
	case n < 1<<10:
		return strconv.FormatInt(n, 10) + " bytes"
	case n < 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KiB"
	}
	return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MiB"
}

func byteOpt(c byte) string {
	if c == '\t' {
		return `'\t'`
	}
	return "'" + string(c) + "'"
}

var sections = []struct {
	sev   scan.Severity
	title string
}{
	{scan.SeverityError, "ERRORS: COPY would stop at the first of these"},
	{scan.SeverityDataLoss, "DATA LOSS: COPY would succeed but skip data"},
	{scan.SeverityWarning, "WARNINGS: not decided offline or worth a look"},
}

// Text writes the human-readable report.
func Text(w io.Writer, s *scan.Summary, m Meta) error {
	bw := bufio.NewWriter(w)
	o := s.Options
	fmt.Fprintf(bw, "input:   %s\n", m.Input)
	fmt.Fprintf(bw, "table:   %s (%d columns from the file)\n", s.Table, len(s.Columns))
	fmt.Fprintf(bw, "format:  CSV, DELIMITER %s, QUOTE %s, ESCAPE %s, NULL %q, HEADER %s; %s line endings\n",
		byteOpt(o.Delimiter), byteOpt(o.Quote), byteOpt(o.Escape), o.Null, o.Header, s.EOL)
	fmt.Fprintf(bw, "server:  PostgreSQL %d, DateStyle %s\n", o.Version, o.DateOrder)
	fmt.Fprintf(bw, "read:    %d records", s.DataRecords)
	if s.HeaderLines > 0 {
		fmt.Fprint(bw, " after the header line")
	}
	fmt.Fprintf(bw, ", %s, in %.2fs\n", size(s.Bytes), s.Elapsed.Seconds())

	if len(s.Setup) > 0 {
		fmt.Fprintln(bw, "\nCOMMAND: COPY would fail for every row")
		for _, p := range s.Setup {
			fmt.Fprintf(bw, "  %s  %s\n", p.SQLState, p.Message)
		}
	}
	for _, sec := range sections {
		var groups []*scan.Group
		for _, g := range s.Groups {
			if g.Severity == sec.sev {
				groups = append(groups, g)
			}
		}
		if len(groups) == 0 {
			continue
		}
		fmt.Fprintf(bw, "\n%s\n", sec.title)
		fmt.Fprintf(bw, "  %8s  %-8s  %-14s  %s\n", "RECORDS", "SQLSTATE", "COLUMN", "PROBLEM")
		for _, g := range groups {
			fmt.Fprintf(bw, "  %8d  %-8s  %-14s  %s\n", g.Count, g.SQLState, g.Column, printable(g.Title))
			for _, ex := range g.Examples {
				where := "line " + strconv.FormatInt(ex.Line, 10)
				if ex.Message != "" && ex.Message != g.Title {
					fmt.Fprintf(bw, "  %38s%s: %s\n", "", where, printable(ex.Message))
				} else {
					fmt.Fprintf(bw, "  %38s%s\n", "", where)
				}
				for _, extra := range []struct{ label, text string }{{"detail", ex.Detail}, {"hint", ex.Hint}, {"note", ex.Note}} {
					if extra.text != "" {
						fmt.Fprintf(bw, "  %40s%s: %s\n", "", extra.label, printable(extra.text))
					}
				}
			}
		}
		if sec.sev == scan.SeverityError && s.Win1251 {
			fmt.Fprintln(bw, "  The invalid bytes look like Windows-1251 text. If the whole file is in that")
			fmt.Fprintln(bw, "  encoding, COPY ... (FORMAT csv, ENCODING 'WIN1251') converts it on load.")
		}
	}

	var keyLines []string
	for _, k := range s.Keys {
		line := fmt.Sprintf("  %s (%s): ", k.Name, strings.Join(k.Columns, ", "))
		switch {
		case !k.Checked:
			line += "not checked, " + k.Reason
		case k.Undecided > 0:
			line += fmt.Sprintf("checked; %d keys could not be compared offline", k.Undecided)
		default:
			line += "checked within the file"
		}
		keyLines = append(keyLines, line)
	}
	if len(keyLines) > 0 {
		fmt.Fprintln(bw, "\nUNIQUE KEYS (rows already in the table are not known)")
		for _, l := range keyLines {
			fmt.Fprintln(bw, l)
		}
	}

	fmt.Fprintln(bw)
	switch {
	case s.Aborted != "":
		fmt.Fprintf(bw, "RESULT: reading stopped: %s\n", s.Aborted)
	case s.CopyWouldFail():
		if f := s.FirstFailure; f != nil && f.Line > 0 {
			fmt.Fprintf(bw, "RESULT: COPY would fail at line %d: %s\n", f.Line, printable(f.Message))
		} else {
			fmt.Fprintln(bw, "RESULT: COPY would fail before reading any data")
		}
	case s.LosesData():
		fmt.Fprintf(bw, "RESULT: COPY would succeed but skip %d records after an end-of-copy marker\n", s.SkippedAfterMarker)
	default:
		fmt.Fprintln(bw, "RESULT: no problems found that would stop COPY")
	}
	fmt.Fprintf(bw, "        %d of %d records rejected, %d clean\n", s.Rejected, s.DataRecords, s.Clean)
	if m.CleanPath != "" {
		fmt.Fprintf(bw, "        clean records:    %s\n", m.CleanPath)
	}
	if m.RejectsPath != "" {
		fmt.Fprintf(bw, "        rejected records: %s\n", m.RejectsPath)
	}
	return bw.Flush()
}

type jsonExample struct {
	Record  int64  `json:"record,omitempty"`
	Line    int64  `json:"line,omitempty"`
	Message string `json:"message"`
	Detail  string `json:"detail,omitempty"`
	Hint    string `json:"hint,omitempty"`
	Note    string `json:"note,omitempty"`
}

type jsonGroup struct {
	Severity string        `json:"severity"`
	SQLState string        `json:"sqlstate,omitempty"`
	Column   string        `json:"column,omitempty"`
	Title    string        `json:"problem"`
	Records  int64         `json:"records"`
	Examples []jsonExample `json:"examples"`
}

type jsonKey struct {
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	Checked   bool     `json:"checked"`
	Reason    string   `json:"reason,omitempty"`
	Undecided int64    `json:"undecided,omitempty"`
}

type jsonReport struct {
	Input              string        `json:"input"`
	Table              string        `json:"table"`
	Columns            []string      `json:"columns"`
	PGVersion          int           `json:"pg_version"`
	DateStyle          string        `json:"datestyle"`
	Header             string        `json:"header"`
	EOL                string        `json:"line_endings"`
	BOM                bool          `json:"bom"`
	Bytes              int64         `json:"bytes"`
	Records            int64         `json:"records"`
	Rejected           int64         `json:"rejected"`
	Clean              int64         `json:"clean"`
	CopyWouldFail      bool          `json:"copy_would_fail"`
	LosesData          bool          `json:"loses_data"`
	FirstFailure       *jsonExample  `json:"first_failure,omitempty"`
	Setup              []jsonExample `json:"setup_errors,omitempty"`
	Problems           []jsonGroup   `json:"problems"`
	SkippedAfterMarker int64         `json:"skipped_after_marker,omitempty"`
	Win1251Hint        bool          `json:"win1251_hint,omitempty"`
	Keys               []jsonKey     `json:"unique_keys,omitempty"`
	Aborted            string        `json:"aborted,omitempty"`
	ElapsedMS          int64         `json:"elapsed_ms"`
}

func toJSONExample(e scan.Example) jsonExample {
	return jsonExample{e.Record, e.Line, e.Message, e.Detail, e.Hint, e.Note}
}

// JSON writes the report as one JSON document.
func JSON(w io.Writer, s *scan.Summary, m Meta) error {
	r := jsonReport{
		Input:              m.Input,
		Table:              s.Table,
		Columns:            s.Columns,
		PGVersion:          int(s.Options.Version),
		DateStyle:          s.Options.DateOrder.String(),
		Header:             s.Options.Header.String(),
		EOL:                s.EOL.String(),
		BOM:                s.BOM,
		Bytes:              s.Bytes,
		Records:            s.DataRecords,
		Rejected:           s.Rejected,
		Clean:              s.Clean,
		CopyWouldFail:      s.CopyWouldFail(),
		LosesData:          s.LosesData(),
		Problems:           []jsonGroup{},
		SkippedAfterMarker: s.SkippedAfterMarker,
		Win1251Hint:        s.Win1251,
		Aborted:            s.Aborted,
		ElapsedMS:          s.Elapsed.Milliseconds(),
	}
	if s.FirstFailure != nil {
		f := toJSONExample(*s.FirstFailure)
		r.FirstFailure = &f
	}
	for _, p := range s.Setup {
		r.Setup = append(r.Setup, jsonExample{Message: p.Message, Detail: p.Detail})
	}
	for _, g := range s.Groups {
		jg := jsonGroup{Severity: g.Severity.String(), SQLState: g.SQLState, Column: g.Column, Title: g.Title, Records: g.Count}
		for _, e := range g.Examples {
			jg.Examples = append(jg.Examples, toJSONExample(e))
		}
		r.Problems = append(r.Problems, jg)
	}
	for _, k := range s.Keys {
		r.Keys = append(r.Keys, jsonKey(k))
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(r)
}
