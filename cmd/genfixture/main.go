// Command genfixture writes a demo orders export with known faults and the
// schema it is meant for.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Santaniel0/pg-copycheck/internal/fixture"
)

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "genfixture:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("genfixture", flag.ContinueOnError)
	fs.SetOutput(stderr)
	rows := fs.Int("rows", 100000, "data rows to write")
	seed := fs.Uint64("seed", 1, "random seed; the same seed gives the same file")
	every := fs.Int("fault-every", 1000, "put one faulty row in every N rows (0: no faults)")
	crlf := fs.Bool("crlf", false, "use Windows line endings")
	marker := fs.Bool("end-marker", false, `insert a "\." line before the last tenth of the rows`)
	out := fs.String("o", "", "output `file` (default: standard output)")
	ddl := fs.String("ddl", "", "also write the matching schema to `file`")
	list := fs.Bool("list", false, "print where each fault was put to standard error")
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *ddl != "" {
		if err := os.WriteFile(*ddl, []byte(fixture.DDL), 0o644); err != nil {
			return err
		}
	}
	w := stdout
	if *out != "" {
		f, err := os.Create(*out)
		if err != nil {
			return err
		}
		defer f.Close()
		w = f
	}
	injected, err := fixture.Generate(w, fixture.Config{Rows: *rows, Seed: *seed, FaultEach: *every, CRLF: *crlf, EndMarker: *marker})
	if err != nil {
		return err
	}
	counts := map[fixture.Fault]int{}
	for _, inj := range injected {
		counts[inj.Fault]++
		if *list {
			fmt.Fprintf(stderr, "line %d: %s\n", inj.Line, inj.Fault)
		}
	}
	fmt.Fprintf(stderr, "%d rows, %d faulty:", *rows, len(injected))
	for f := range fixture.NumFaults {
		if counts[f] > 0 {
			fmt.Fprintf(stderr, " %s %d;", f, counts[f])
		}
	}
	fmt.Fprintln(stderr)
	return nil
}
