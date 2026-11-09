package report

import "testing"

func TestPrintable(t *testing.T) {
	tests := []struct{ in, want string }{
		{"12 шт", "12 шт"},
		{"a\x00b", `a\x00b`},
		{"\xc4\xee\xf1", `\xc4\xee\xf1`},
		{"line\r\nnext\tcol", `line\r\nnext\tcol`},
		{"\xef\xbb\xbf1", `\ufeff1`},
		{"\x1b[31mred", `\x1b[31mred`},
	}
	for _, tt := range tests {
		if got := printable(tt.in); got != tt.want {
			t.Errorf("printable(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSize(t *testing.T) {
	for n, want := range map[int64]string{512: "512 bytes", 2048: "2.0 KiB", 36865553: "35.2 MiB"} {
		if got := size(n); got != want {
			t.Errorf("size(%d) = %q, want %q", n, got, want)
		}
	}
}
