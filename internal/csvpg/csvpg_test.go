package csvpg

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
)

type recSummary struct {
	body  string
	line  int64
	issue LineIssue
}

func readAll(t *testing.T, r io.Reader, opts copyopt.Options) ([]recSummary, EOL) {
	t.Helper()
	rd := NewReader(r, opts)
	var out []recSummary
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			return out, rd.EOL()
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, recSummary{string(rec.Body()), rec.Line, rec.Issue})
	}
}

func TestReaderBoundaries(t *testing.T) {
	escOpts := copyopt.Default()
	escOpts.Escape = '\\'

	tests := []struct {
		name string
		in   string
		opts copyopt.Options
		want []recSummary
		eol  EOL
	}{
		{"lf", "a,b\nc,d\n", copyopt.Default(), []recSummary{{"a,b", 1, 0}, {"c,d", 2, 0}}, EOLNL},
		{"no final newline", "a\nb", copyopt.Default(), []recSummary{{"a", 1, 0}, {"b", 2, 0}}, EOLNL},
		{"crlf", "a\r\nb\r\n", copyopt.Default(), []recSummary{{"a", 1, 0}, {"b", 2, 0}}, EOLCRNL},
		{"cr only", "a\rb\r", copyopt.Default(), []recSummary{{"a", 1, 0}, {"b", 2, 0}}, EOLCR},
		{"quoted newline spans lines", "\"x\ny\",1\nz,2\n", copyopt.Default(),
			[]recSummary{{"\"x\ny\",1", 1, 0}, {"z,2", 3, 0}}, EOLNL},
		{"doubled quote stays inside quotes", "\"a\"\"\nb\",1\n", copyopt.Default(),
			[]recSummary{{"\"a\"\"\nb\",1", 1, 0}}, EOLNL},
		// copyfromparse.c: a quote in the middle of an unquoted field still
		// opens a quoted section.
		{"mid-field quote", "ab\"c\nd\"e\n", copyopt.Default(), []recSummary{{"ab\"c\nd\"e", 1, 0}}, EOLNL},
		{"escaped quote with backslash escape", "\"a\\\"\nb\"\n", escOpts, []recSummary{{"\"a\\\"\nb\"", 1, 0}}, EOLNL},
		{"backslash outside quotes is data", "a\\\"b\nc\"\n", escOpts, []recSummary{{"a\\\"b\nc\"", 1, 0}}, EOLNL},
		{"stray CR in LF file", "a\nb\r\nc\n", copyopt.Default(),
			[]recSummary{{"a", 1, 0}, {"b\r", 2, IssueUnquotedCR}, {"c", 3, 0}}, EOLNL},
		{"stray LF in CRLF file", "a\r\nb\nb2\r\nc\r\n", copyopt.Default(),
			[]recSummary{{"a", 1, 0}, {"b\nb2", 2, IssueUnquotedLF}, {"c", 4, 0}}, EOLCRNL},
		{"lone CR in CRLF file", "a\r\nb\rx\r\n", copyopt.Default(),
			[]recSummary{{"a", 1, 0}, {"b\rx", 2, IssueUnquotedCR}}, EOLCRNL},
		{"quoted CR is data", "\"a\rb\"\nc\n", copyopt.Default(), []recSummary{{"\"a\rb\"", 1, 0}, {"c", 3, 0}}, EOLNL},
		{"unterminated quote runs to EOF", "\"a\nb\nc\n", copyopt.Default(), []recSummary{{"\"a\nb\nc\n", 1, 0}}, EOLUnknown},
		{"empty lines are records", "\n\n", copyopt.Default(), []recSummary{{"", 1, 0}, {"", 2, 0}}, EOLNL},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, eol := readAll(t, strings.NewReader(tt.in), tt.opts)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d records %+v, want %d", len(got), got, len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("record %d = %+v, want %+v", i+1, got[i], tt.want[i])
				}
			}
			if eol != tt.eol {
				t.Errorf("EOL = %v, want %v", eol, tt.eol)
			}
		})
	}
}

func TestReaderSmallReads(t *testing.T) {
	in := "id,name\r\n1,\"multi\r\nline\"\r\n2,x\r\n"
	want, _ := readAll(t, strings.NewReader(in), copyopt.Default())
	got, _ := readAll(t, iotest.OneByteReader(strings.NewReader(in)), copyopt.Default())
	if len(got) != len(want) {
		t.Fatalf("one-byte reads gave %d records, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("record %d = %+v, want %+v", i+1, got[i], want[i])
		}
	}
}

func TestReaderEndMarker(t *testing.T) {
	rd := NewReader(strings.NewReader("a\n\\.\nb\n\\."), copyopt.Default())
	var markers []bool
	for {
		rec, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		markers = append(markers, rec.IsEndMarker())
	}
	// The last `\.` has no terminator, which copyfromparse.c treats as data.
	want := []bool{false, true, false, false}
	if len(markers) != len(want) {
		t.Fatalf("got %v, want %v", markers, want)
	}
	for i := range want {
		if markers[i] != want[i] {
			t.Errorf("record %d marker = %v, want %v", i+1, markers[i], want[i])
		}
	}
}

func TestReaderTooLarge(t *testing.T) {
	rd := NewReader(strings.NewReader("ok\n\""+strings.Repeat("x", 5000)), copyopt.Default())
	rd.SetMaxRecord(1000)
	if _, err := rd.Next(); err != nil {
		t.Fatal(err)
	}
	_, err := rd.Next()
	var tooLarge *RecordTooLargeError
	if !errors.As(err, &tooLarge) {
		t.Fatalf("err = %v, want RecordTooLargeError", err)
	}
	if tooLarge.Line != 2 {
		t.Errorf("Line = %d, want 2", tooLarge.Line)
	}
}

func TestReaderPropagatesReadError(t *testing.T) {
	boom := errors.New("disk on fire")
	rd := NewReader(io.MultiReader(strings.NewReader("a\nb"), iotest.ErrReader(boom)), copyopt.Default())
	if _, err := rd.Next(); err != nil {
		t.Fatal(err)
	}
	if _, err := rd.Next(); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
}

type fieldSummary struct {
	val    string
	null   bool
	quoted bool
}

func TestSplit(t *testing.T) {
	semi := copyopt.Default()
	semi.Delimiter = ';'
	semi.Null = `\N`
	bs := copyopt.Default()
	bs.Escape = '\\'

	tests := []struct {
		name string
		opts copyopt.Options
		body string
		want []fieldSummary
		err  error
	}{
		{"plain", copyopt.Default(), "a,b,c", []fieldSummary{{"a", false, false}, {"b", false, false}, {"c", false, false}}, nil},
		{"unquoted empty is NULL, quoted empty is not", copyopt.Default(), `,""`,
			[]fieldSummary{{"", true, false}, {"", false, true}}, nil},
		{"empty body is one NULL field", copyopt.Default(), "", []fieldSummary{{"", true, false}}, nil},
		{"doubled quote", copyopt.Default(), `"say ""hi""",x`, []fieldSummary{{`say "hi"`, false, true}, {"x", false, false}}, nil},
		{"delimiter inside quotes", copyopt.Default(), `"a,b",c`, []fieldSummary{{"a,b", false, true}, {"c", false, false}}, nil},
		{"mid-field quotes", copyopt.Default(), `ab"c,d"e,f`, []fieldSummary{{"abc,de", false, true}, {"f", false, false}}, nil},
		{"quoted value equal to NULL string is not NULL", semi, `"\N";\N`, []fieldSummary{{`\N`, false, true}, {`\N`, true, false}}, nil},
		{"backslash escape", bs, `"a\"b\\c\d"`, []fieldSummary{{`a"b\c\d`, false, true}}, nil},
		{"spaces are kept", copyopt.Default(), ` 1 , "x" `, []fieldSummary{{" 1 ", false, false}, {` x `, false, true}}, nil},
		{"unterminated", copyopt.Default(), `a,"b`, nil, ErrUnterminatedQuote},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fields, err := NewSplitter(tt.opts).Split([]byte(tt.body))
			if !errors.Is(err, tt.err) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if tt.err != nil {
				return
			}
			if len(fields) != len(tt.want) {
				t.Fatalf("got %d fields, want %d", len(fields), len(tt.want))
			}
			for i, f := range fields {
				got := fieldSummary{string(f.Value), f.Null, f.Quoted}
				if got != tt.want[i] {
					t.Errorf("field %d = %+v, want %+v", i+1, got, tt.want[i])
				}
			}
		})
	}
}

func TestSplitReusesScratch(t *testing.T) {
	s := NewSplitter(copyopt.Default())
	first, err := s.Split([]byte(`"long quoted value",x`))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(first[0].Value); got != "long quoted value" {
		t.Fatalf("got %q", got)
	}
	second, err := s.Split([]byte(`"a","b"`))
	if err != nil {
		t.Fatal(err)
	}
	if string(second[0].Value) != "a" || string(second[1].Value) != "b" {
		t.Fatalf("got %q %q", second[0].Value, second[1].Value)
	}
}

// FuzzReader checks the property the rest of the tool relies on: records
// never lose or invent bytes, so the clean and rejects outputs together
// always account for the whole input.
func FuzzReader(f *testing.F) {
	for _, s := range []string{
		"a,b\nc,d\n", "\"x\ny\"\r\n1\r\n", "a\rb\r", "\"unterminated", "\\.\n", "a\nb\r\nc", "\"\"\"\n",
	} {
		f.Add([]byte(s), false)
	}
	f.Fuzz(func(t *testing.T, data []byte, backslashEscape bool) {
		opts := copyopt.Default()
		if backslashEscape {
			opts.Escape = '\\'
		}
		rd := NewReader(bytes.NewReader(data), opts)
		sp := NewSplitter(opts)
		var joined []byte
		var prevNum, prevLine int64
		for {
			rec, err := rd.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("Next: %v", err)
			}
			if rec.Num != prevNum+1 || rec.Line < prevLine || rec.EndLine < rec.Line {
				t.Fatalf("bad numbering: %+v after num %d line %d", rec, prevNum, prevLine)
			}
			prevNum, prevLine = rec.Num, rec.Line
			if len(rec.Raw) == 0 {
				t.Fatal("empty record")
			}
			joined = append(joined, rec.Raw...)
			fields, err := sp.Split(rec.Body())
			if err == nil && len(fields) == 0 {
				t.Fatal("split returned no fields")
			}
		}
		if !bytes.Equal(joined, data) {
			t.Fatalf("records do not add up to the input:\n got %q\nwant %q", joined, data)
		}
	})
}
