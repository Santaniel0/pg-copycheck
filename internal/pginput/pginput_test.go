package pginput

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/schema"
)

const rainbowDDL = "create type rainbow as enum ('red', 'orange', 'yellow', 'green', 'blue', 'purple');"

func columnType(t *testing.T, typ string) schema.Type {
	t.Helper()
	cat, err := schema.ParseDDL(strings.NewReader(rainbowDDL + "create table t (c " + typ + ");"))
	if err != nil {
		t.Fatalf("type %q: %v", typ, err)
	}
	tbl, err := cat.Lookup("t")
	if err != nil {
		t.Fatal(err)
	}
	return tbl.Columns[0].Type
}

func parseSettings(t *testing.T, s string) Settings {
	t.Helper()
	set := Settings{Version: 17, DateOrder: copyopt.DateOrderMDY}
	if s == "-" {
		return set
	}
	for tok := range strings.SplitSeq(s, ",") {
		switch tok {
		case "mdy", "dmy", "ymd":
			order, err := copyopt.ParseDateStyle(tok)
			if err != nil {
				t.Fatal(err)
			}
			set.DateOrder = order
		default:
			v, err := copyopt.ParseVersion(strings.TrimPrefix(tok, "v"))
			if err != nil {
				t.Fatalf("settings %q: %v", s, err)
			}
			set.Version = v
		}
	}
	return set
}

func describe(o Outcome) string {
	s := o.Message
	if o.Hint != "" {
		s += " | HINT: " + o.Hint
	}
	if o.Detail != "" {
		s += " | DETAIL: " + o.Detail
	}
	return s
}

// TestCorpus checks every case in testdata/corpus.tsv. The checker may say
// Unsure only where the corpus allows it; otherwise its verdict and message
// must be what PostgreSQL produces.
func TestCorpus(t *testing.T) {
	f, err := os.Open("testdata/corpus.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	lineNo, cases := 0, 0
	for sc.Scan() {
		lineNo++
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 6 {
			t.Fatalf("corpus line %d: %d columns, want 6", lineNo, len(cols))
		}
		input, err := strconv.Unquote(cols[2])
		if err != nil {
			t.Fatalf("corpus line %d: input: %v", lineNo, err)
		}
		wantMsg, err := strconv.Unquote(cols[4])
		if err != nil {
			t.Fatalf("corpus line %d: message: %v", lineNo, err)
		}
		verdict, mayBeUnsure := strings.CutSuffix(cols[3], "?")
		cases++

		checker := For(columnType(t, cols[0]), parseSettings(t, cols[1]))
		out := checker.Check([]byte(input))
		got := out.Verdict.String()
		where := "corpus line " + strconv.Itoa(lineNo) + " (" + cols[0] + " " + cols[2] + ", " + cols[5] + ")"
		switch {
		case got == verdict:
		case mayBeUnsure && out.Verdict == Unsure:
			continue
		default:
			t.Errorf("%s: verdict %s, want %s (%s)", where, got, cols[3], describe(out))
			continue
		}
		if wantMsg != "" && describe(out) != wantMsg {
			t.Errorf("%s:\n got message %q\nwant message %q", where, describe(out), wantMsg)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if cases < 100 {
		t.Fatalf("only %d corpus cases read", cases)
	}
}

func TestCanonicalEquality(t *testing.T) {
	set := Settings{Version: 17, DateOrder: copyopt.DateOrderDMY}
	tests := []struct {
		typ  string
		a, b string
		same bool
	}{
		{"integer", " 17", "17", true},
		{"integer", "0x11", "17", true},
		{"integer", "17", "-17", false},
		{"numeric", "1.0", "1.00", true},
		{"numeric", "1e2", "100", true},
		{"numeric", "-0.0", "0", true},
		{"numeric(5,2)", "1.004", "1.00", true},
		{"numeric", "1.5", "15", false},
		{"double precision", "-0", "0", true},
		{"double precision", "NaN", "nan", true},
		{"boolean", "yes", "t", true},
		{"uuid", "A0EEBC99-9C0B-4EF8-BB6D-6BB9BD380A11", "{a0eebc999c0b4ef8bb6d6bb9bd380a11}", true},
		{"char(3)", "ab ", "ab", true},
		{"varchar(5)", "ab ", "ab", false},
		{"date", "05.01.2024", "2024-01-05", true},
		{"timestamp", "2024-01-05 10:00:00+03", "2024-01-05 10:00:00", true},
		{"timestamptz", "2024-01-05 10:00:00+03", "2024-01-05T07:00:00Z", true},
		{"timestamptz", "2024-01-05 10:00:00+03", "2024-01-05 10:00:00+04", false},
	}
	for _, tt := range tests {
		c, ok := For(columnType(t, tt.typ), set).(Canonicalizer)
		if !ok {
			t.Fatalf("%s: checker has no canonical form", tt.typ)
		}
		a, okA := c.Canonical(nil, []byte(tt.a))
		b, okB := c.Canonical(nil, []byte(tt.b))
		if !okA || !okB {
			t.Errorf("%s: canonical form missing for %q or %q", tt.typ, tt.a, tt.b)
			continue
		}
		if (string(a) == string(b)) != tt.same {
			t.Errorf("%s: %q -> %q, %q -> %q, want same=%v", tt.typ, tt.a, a, tt.b, b, tt.same)
		}
	}
}

func TestCanonicalUndecided(t *testing.T) {
	set := Settings{Version: 17, DateOrder: copyopt.DateOrderMDY}
	tests := []struct{ typ, v string }{
		{"timestamptz", "2024-01-05 10:00:00"}, // depends on the session TimeZone
		{"timestamp(0)", "2024-01-05 10:00:00.5"},
		{"date", "today"},
		{"integer", "nope"},
	}
	for _, tt := range tests {
		c := For(columnType(t, tt.typ), set).(Canonicalizer)
		if _, ok := c.Canonical(nil, []byte(tt.v)); ok {
			t.Errorf("%s %q: got a canonical form, want none", tt.typ, tt.v)
		}
	}
}

func BenchmarkIntegerCheck(b *testing.B) {
	c := For(schema.Type{Kind: schema.TypeInt8}, Settings{Version: 17})
	v := []byte("1234567890123")
	for b.Loop() {
		c.Check(v)
	}
}
