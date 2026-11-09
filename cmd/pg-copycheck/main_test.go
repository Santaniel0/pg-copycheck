package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testDDL = `
CREATE TABLE public.items (
    id integer NOT NULL,
    title character varying(10) NOT NULL,
    price numeric(6,2),
    added date
);
ALTER TABLE ONLY public.items ADD CONSTRAINT items_pkey PRIMARY KEY (id);
`

func write(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func runCLI(stdin string, args ...string) (int, string, string) {
	var out, errOut bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestExitCodes(t *testing.T) {
	dir := t.TempDir()
	ddl := write(t, dir, "schema.sql", testDDL)
	good := write(t, dir, "good.csv", "id,title,price,added\n1,pen,1.50,2024-01-05\n2,ink,,\n")
	bad := write(t, dir, "bad.csv", "id,title,price,added\n1,pen,1.50,2024-01-05\n2,ink,10000,31.12.2024\n")

	tests := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"clean file", []string{"--ddl", ddl, "--header", "match", good}, 0, "RESULT: no problems found", ""},
		{"flags after the file", []string{good, "--ddl", ddl, "--header=true"}, 0, "0 of 2 records rejected, 2 clean", ""},
		{"problems", []string{"--ddl", ddl, "--header", "true", bad}, 1, "RESULT: COPY would fail at line 3", ""},
		{"DMY accepts the date", []string{"--ddl", ddl, "--header", "true", "--datestyle", "ISO, DMY", bad}, 1, "numeric field overflow", ""},
		{"missing --ddl", []string{good}, 2, "", "--ddl is required"},
		{"no input", []string{"--ddl", ddl}, 2, "", "expected exactly one input file"},
		{"unknown table", []string{"--ddl", ddl, "--table", "nope", good}, 2, "", "table not found"},
		{"unknown column", []string{"--ddl", ddl, "--columns", "id,nope", good}, 2, "", `COPY would refuse this command: column "nope" of relation "items" does not exist`},
		{"bad version", []string{"--ddl", ddl, "--pg-version", "12", good}, 2, "", "PostgreSQL 12 is not supported"},
		{"bad delimiter", []string{"--ddl", ddl, "--delimiter", ";;", good}, 2, "", "single one-byte character"},
		{"missing input file", []string{"--ddl", ddl, filepath.Join(dir, "absent.csv")}, 2, "", "absent.csv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, stdout, stderr := runCLI("", tt.args...)
			if code != tt.code {
				t.Errorf("exit code %d, want %d\nstdout:\n%s\nstderr:\n%s", code, tt.code, stdout, stderr)
			}
			if !strings.Contains(stdout, tt.stdout) {
				t.Errorf("stdout lacks %q:\n%s", tt.stdout, stdout)
			}
			if !strings.Contains(stderr, tt.stderr) {
				t.Errorf("stderr lacks %q:\n%s", tt.stderr, stderr)
			}
		})
	}
}

func TestOutputsAndJSON(t *testing.T) {
	dir := t.TempDir()
	ddl := write(t, dir, "schema.sql", testDDL)
	in := "1,pen,1.50,2024-01-05\n1,pen again,2,\nx,ink,,\n"
	clean, rejects := filepath.Join(dir, "clean.csv"), filepath.Join(dir, "rejects.csv")

	code, stdout, stderr := runCLI(in, "--ddl", ddl, "--check-unique", "--report", "json", "--clean", clean, "--rejects", rejects, "-")
	if code != 1 {
		t.Fatalf("exit code %d, stderr %s", code, stderr)
	}
	var rep struct {
		Input         string `json:"input"`
		Records       int64  `json:"records"`
		Rejected      int64  `json:"rejected"`
		CopyWouldFail bool   `json:"copy_would_fail"`
		FirstFailure  struct {
			Line    int64  `json:"line"`
			Message string `json:"message"`
		} `json:"first_failure"`
		Problems []struct {
			Problem string `json:"problem"`
			Records int64  `json:"records"`
		} `json:"problems"`
	}
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("report is not JSON: %v\n%s", err, stdout)
	}
	if rep.Input != "standard input" || rep.Records != 3 || rep.Rejected != 2 || !rep.CopyWouldFail ||
		rep.FirstFailure.Line != 2 || len(rep.Problems) != 2 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if rep.FirstFailure.Message != `duplicate key value violates unique constraint "items_pkey"` {
		t.Errorf("first failure = %q", rep.FirstFailure.Message)
	}

	gotClean, _ := os.ReadFile(clean)
	if string(gotClean) != "1,pen,1.50,2024-01-05\n" {
		t.Errorf("clean = %q", gotClean)
	}
	gotRejects, _ := os.ReadFile(rejects)
	wantRejects := "record,line,column,sqlstate,message,detail,raw\n" +
		"2,2,,23505,\"duplicate key value violates unique constraint \"\"items_pkey\"\"\",,\"1,pen again,2,\"\n" +
		"3,3,id,22P02,\"invalid input syntax for type integer: \"\"x\"\"\",,\"x,ink,,\"\n"
	if string(gotRejects) != wantRejects {
		t.Errorf("rejects =\n%s\nwant\n%s", gotRejects, wantRejects)
	}
}

func TestRefusesToOverwriteInput(t *testing.T) {
	dir := t.TempDir()
	ddl := write(t, dir, "schema.sql", testDDL)
	content := "1,pen,1.50,2024-01-05\n"
	in := write(t, dir, "in.csv", content)
	code, _, stderr := runCLI("", "--ddl", ddl, "--clean", in, in)
	if code != 2 || !strings.Contains(stderr, "refusing to overwrite the input file") {
		t.Fatalf("code %d, stderr %q", code, stderr)
	}
	if got, _ := os.ReadFile(in); string(got) != content {
		t.Fatalf("input was modified: %q", got)
	}
}
