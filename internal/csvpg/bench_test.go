package csvpg

import (
	"bytes"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/Santaniel0/pg-copycheck/internal/copyopt"
)

// BenchmarkReader measures record boundary detection alone, the one
// sequential stage of the pipeline. Set PGCC_BENCH_FILE to a large CSV.
func BenchmarkReader(b *testing.B) {
	path := os.Getenv("PGCC_BENCH_FILE")
	if path == "" {
		b.Skip("PGCC_BENCH_FILE not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		rd := NewReader(bytes.NewReader(data), copyopt.Default())
		for {
			if _, err := rd.Next(); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				b.Fatal(err)
			}
		}
	}
}
