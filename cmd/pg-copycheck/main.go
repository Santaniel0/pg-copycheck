// Command pg-copycheck reads a CSV file the way PostgreSQL's COPY FROM would
// and reports every row that would make the load fail, without a database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
	"github.com/Santaniel0/pg-copycheck/internal/report"
	"github.com/Santaniel0/pg-copycheck/internal/scan"
	"github.com/Santaniel0/pg-copycheck/internal/schema"
)

// Exit codes.
const (
	exitOK       = 0 // nothing found that would stop COPY or lose data
	exitProblems = 1 // COPY would fail or skip data
	exitUsage    = 2 // bad arguments, unreadable input, or a COPY command that cannot work
)

const usage = `usage: pg-copycheck --ddl schema.sql [flags] FILE|-

Checks FILE (or standard input) against a table definition taken from
pg_dump --schema-only output, the way COPY table FROM ... (FORMAT csv) would
read it, and reports every row that would make COPY fail.

Flags:
`

type cliError struct{ msg string }

func (e *cliError) Error() string { return e.msg }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

type options struct {
	ddl, table, columns      string
	delimiter, quote, escape string
	null, header             string
	forceNull, forceNotNull  string
	dateStyle, pgVersion     string
	checkUnique              bool
	workers, maxExamples     int
	cleanPath, rejectsPath   string
	reportFormat             string
	escapeSet                bool
	input                    string
}

// parseArgs accepts flags before and after the file name.
func parseArgs(args []string, stderr io.Writer) (*options, error) {
	o := &options{}
	fs := flag.NewFlagSet("pg-copycheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() {
		fmt.Fprint(stderr, usage)
		fs.PrintDefaults()
	}
	fs.StringVar(&o.ddl, "ddl", "", "`file` with CREATE TABLE statements (pg_dump --schema-only output)")
	fs.StringVar(&o.table, "table", "", "target table, [schema.]name; may be omitted when the DDL has one table")
	fs.StringVar(&o.columns, "columns", "", "COPY column list, comma-separated; default is every non-generated column")
	fs.StringVar(&o.delimiter, "delimiter", ",", `DELIMITER; use \t for tab`)
	fs.StringVar(&o.quote, "quote", `"`, "QUOTE")
	fs.StringVar(&o.escape, "escape", "", "ESCAPE (default: same as QUOTE)")
	fs.StringVar(&o.null, "null", "", "NULL string")
	fs.StringVar(&o.header, "header", "false", "HEADER: false, true or match")
	fs.StringVar(&o.forceNull, "force-null", "", "FORCE_NULL columns, comma-separated, or *")
	fs.StringVar(&o.forceNotNull, "force-not-null", "", "FORCE_NOT_NULL columns, comma-separated, or *")
	fs.StringVar(&o.dateStyle, "datestyle", "ISO, MDY", "server DateStyle; only the field order (MDY, DMY, YMD) matters")
	fs.StringVar(&o.pgVersion, "pg-version", "17", "PostgreSQL major version of the target server (14 to 18)")
	fs.BoolVar(&o.checkUnique, "check-unique", false, "find duplicate PRIMARY KEY / UNIQUE values within the file (uses memory per row)")
	fs.IntVar(&o.workers, "workers", 0, "checking goroutines (default: number of CPUs)")
	fs.IntVar(&o.maxExamples, "max-examples", 3, "examples shown per problem")
	fs.StringVar(&o.cleanPath, "clean", "", "write records that pass to `file`")
	fs.StringVar(&o.rejectsPath, "rejects", "", "write rejected records with the reason to `file` (CSV)")
	fs.StringVar(&o.reportFormat, "report", "text", "report format: text or json")

	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "escape" {
			o.escapeSet = true
		}
	})
	switch {
	case len(positional) != 1:
		return nil, &cliError{"expected exactly one input file (use - for standard input)"}
	case o.ddl == "":
		return nil, &cliError{"--ddl is required"}
	case o.reportFormat != "text" && o.reportFormat != "json":
		return nil, &cliError{"--report must be text or json"}
	}
	o.input = positional[0]
	return o, nil
}

func (o *options) copyOptions() (copyopt.Options, error) {
	opts := copyopt.Default()
	var err error
	if opts.Delimiter, err = copyopt.ParseByteOption("delimiter", o.delimiter); err != nil {
		return opts, err
	}
	if opts.Quote, err = copyopt.ParseByteOption("quote", o.quote); err != nil {
		return opts, err
	}
	opts.Escape = opts.Quote
	if o.escapeSet {
		if opts.Escape, err = copyopt.ParseByteOption("escape", o.escape); err != nil {
			return opts, err
		}
	}
	opts.Null = o.null
	if opts.Header, err = copyopt.ParseHeader(o.header); err != nil {
		return opts, err
	}
	opts.ForceNull = copyopt.ParseColumnSet(o.forceNull)
	opts.ForceNotNull = copyopt.ParseColumnSet(o.forceNotNull)
	if opts.DateOrder, err = copyopt.ParseDateStyle(o.dateStyle); err != nil {
		return opts, err
	}
	if opts.Version, err = copyopt.ParseVersion(o.pgVersion); err != nil {
		return opts, err
	}
	return opts, opts.Validate()
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o, err := parseArgs(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "pg-copycheck:", err)
		return exitUsage
	}
	code, err := check(ctx, o, stdin, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "pg-copycheck:", err)
		return exitUsage
	}
	return code
}

func check(ctx context.Context, o *options, stdin io.Reader, stdout io.Writer) (int, error) {
	opts, err := o.copyOptions()
	if err != nil {
		return 0, err
	}
	ddl, err := os.Open(o.ddl)
	if err != nil {
		return 0, err
	}
	cat, err := schema.ParseDDL(ddl)
	ddl.Close()
	if err != nil {
		return 0, fmt.Errorf("%s: %w", o.ddl, err)
	}
	tbl, err := cat.Lookup(o.table)
	if err != nil {
		return 0, err
	}

	in := stdin
	name := "standard input"
	if o.input != "-" {
		f, err := os.Open(o.input)
		if err != nil {
			return 0, err
		}
		defer f.Close()
		in, name = f, o.input
	}

	if o.cleanPath != "" && o.cleanPath == o.rejectsPath {
		return 0, &cliError{"--clean and --rejects must be different files"}
	}
	var out scan.Output
	var created []*os.File
	for _, target := range []struct {
		path string
		w    *io.Writer
	}{{o.cleanPath, &out.Clean}, {o.rejectsPath, &out.Rejects}} {
		if target.path == "" {
			continue
		}
		if err := notSameFile(o.input, target.path); err != nil {
			return 0, err
		}
		f, err := os.Create(target.path)
		if err != nil {
			return 0, err
		}
		created = append(created, f)
		*target.w = f
	}

	sum, runErr := scan.Run(ctx, scan.Config{
		Table:       tbl,
		Columns:     splitList(o.columns),
		Options:     opts,
		Workers:     o.workers,
		CheckUnique: o.checkUnique,
		MaxExamples: o.maxExamples,
	}, in, out)
	for _, f := range created {
		if err := f.Close(); err != nil && runErr == nil {
			runErr = err
		}
		if runErr != nil {
			os.Remove(f.Name())
		}
	}
	if runErr != nil {
		var se *scan.SetupError
		if errors.As(runErr, &se) {
			return 0, fmt.Errorf("COPY would refuse this command: %w", runErr)
		}
		return 0, runErr
	}

	meta := report.Meta{Input: name, CleanPath: o.cleanPath, RejectsPath: o.rejectsPath}
	render := report.Text
	if o.reportFormat == "json" {
		render = report.JSON
	}
	if err := render(stdout, sum, meta); err != nil {
		return 0, fmt.Errorf("write report: %w", err)
	}
	if sum.CopyWouldFail() || sum.LosesData() {
		return exitProblems, nil
	}
	return exitOK, nil
}

// notSameFile keeps an output path from truncating the input.
func notSameFile(input, output string) error {
	if input == "-" {
		return nil
	}
	a, err := os.Stat(input)
	if err != nil {
		return nil
	}
	b, err := os.Stat(output)
	if err != nil {
		return nil
	}
	if os.SameFile(a, b) {
		return &cliError{fmt.Sprintf("refusing to overwrite the input file with %s", output)}
	}
	return nil
}
