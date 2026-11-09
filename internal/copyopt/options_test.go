package copyopt

import (
	"errors"
	"testing"
)

func TestParseDateStyle(t *testing.T) {
	tests := []struct {
		in   string
		want DateOrder
		err  bool
	}{
		{"ISO, MDY", DateOrderMDY, false},
		{"ISO, DMY", DateOrderDMY, false},
		{"dmy", DateOrderDMY, false},
		{"SQL, European", DateOrderDMY, false},
		{"Postgres, YMD", DateOrderYMD, false},
		// check_datestyle: German without an explicit order means DMY.
		{"German", DateOrderDMY, false},
		{"German, MDY", DateOrderMDY, false},
		{"ISO", DateOrderMDY, false},
		{"ISO, DDMMYY", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseDateStyle(tt.in)
		if (err != nil) != tt.err || got != tt.want {
			t.Errorf("ParseDateStyle(%q) = %v, %v", tt.in, got, err)
		}
	}
}

func TestParseVersionAndHeader(t *testing.T) {
	for in, want := range map[string]Version{"17": 17, "16.4": 16, "PostgreSQL 18": 18, "14": 14} {
		if got, err := ParseVersion(in); err != nil || got != want {
			t.Errorf("ParseVersion(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"13", "19", "x"} {
		if _, err := ParseVersion(in); err == nil {
			t.Errorf("ParseVersion(%q) accepted", in)
		}
	}
	for in, want := range map[string]HeaderMode{"true": HeaderSkip, "ON": HeaderSkip, "0": HeaderNone, "match": HeaderMatch} {
		if got, err := ParseHeader(in); err != nil || got != want {
			t.Errorf("ParseHeader(%q) = %v, %v", in, got, err)
		}
	}
}

func TestValidate(t *testing.T) {
	mod := func(f func(*Options)) Options {
		o := Default()
		f(&o)
		return o
	}
	tests := []struct {
		name string
		opts Options
		ok   bool
	}{
		{"defaults", Default(), true},
		{"tab delimiter", mod(func(o *Options) { o.Delimiter = '\t' }), true},
		{"delimiter equals quote", mod(func(o *Options) { o.Delimiter = '"' }), false},
		{"newline delimiter", mod(func(o *Options) { o.Delimiter = '\n' }), false},
		{"delimiter in NULL", mod(func(o *Options) { o.Null = "a,b" }), false},
		{"quote in NULL", mod(func(o *Options) { o.Null = `"` }), false},
		{"header match on 14", mod(func(o *Options) { o.Header = HeaderMatch; o.Version = 14 }), false},
	}
	for _, tt := range tests {
		err := tt.opts.Validate()
		if (err == nil) != tt.ok {
			t.Errorf("%s: err = %v", tt.name, err)
		}
		if err != nil && !errors.Is(err, ErrInvalidOptions) {
			t.Errorf("%s: error does not wrap ErrInvalidOptions", tt.name)
		}
	}
}
