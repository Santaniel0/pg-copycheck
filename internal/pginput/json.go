package pginput

import "fmt"

// jsonChecker follows the lexer and parser in src/common/jsonapi.c. Plain
// json only checks syntax. jsonb also de-escapes strings, which rejects
// \u0000 and unpaired UTF-16 surrogates; those checks run only when the
// parser has a string buffer (lex->strval), i.e. for jsonb.
type jsonChecker struct {
	binary bool
}

// maxJSONDepth keeps the checker away from PostgreSQL's stack depth limit,
// whose exact nesting equivalent depends on server settings.
const maxJSONDepth = 1000

type jsonScanner struct {
	s      []byte
	i      int
	binary bool
	depth  int
	out    Outcome
	failed bool
}

func (c jsonChecker) Check(v []byte) Outcome {
	sc := &jsonScanner{s: v, binary: c.binary}
	sc.ws()
	sc.value()
	sc.ws()
	if !sc.failed && sc.i != len(sc.s) {
		sc.syntax("")
	}
	if sc.failed {
		return sc.out
	}
	return valid
}

func (sc *jsonScanner) syntax(detail string) {
	if sc.failed {
		return
	}
	title := syntaxTitle("json")
	sc.out = Outcome{Verdict: Invalid, SQLState: StateInvalidText, Title: title, Message: title, Detail: detail}
	sc.failed = true
}

func (sc *jsonScanner) undecided(title string) {
	if !sc.failed {
		sc.out, sc.failed = unsure(title), true
	}
}

func (sc *jsonScanner) ws() {
	for sc.i < len(sc.s) {
		switch sc.s[sc.i] {
		case ' ', '\t', '\n', '\r':
			sc.i++
		default:
			return
		}
	}
}

func isJSONAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c >= 0x80
}

func (sc *jsonScanner) value() {
	if sc.failed {
		return
	}
	if sc.i >= len(sc.s) {
		sc.syntax("The input string ended unexpectedly.")
		return
	}
	switch c := sc.s[sc.i]; {
	case c == '{':
		sc.container('}', true)
	case c == '[':
		sc.container(']', false)
	case c == '"':
		sc.str()
	case c == '-' || isDigit(c):
		sc.number()
	default:
		start := sc.i
		for sc.i < len(sc.s) && isJSONAlnum(sc.s[sc.i]) {
			sc.i++
		}
		switch string(sc.s[start:sc.i]) {
		case "true", "false", "null":
		default:
			sc.syntax("")
		}
	}
}

func (sc *jsonScanner) container(closer byte, object bool) {
	sc.depth++
	if sc.depth > maxJSONDepth {
		sc.undecided("json nested deeper than the offline limit")
		return
	}
	sc.i++
	sc.ws()
	if sc.i < len(sc.s) && sc.s[sc.i] == closer {
		sc.i++
		sc.depth--
		return
	}
	for !sc.failed {
		if object {
			if sc.i >= len(sc.s) || sc.s[sc.i] != '"' {
				sc.syntax("")
				return
			}
			sc.str()
			sc.ws()
			if sc.i >= len(sc.s) || sc.s[sc.i] != ':' {
				sc.syntax("")
				return
			}
			sc.i++
			sc.ws()
		}
		sc.value()
		sc.ws()
		if sc.i >= len(sc.s) {
			sc.syntax("The input string ended unexpectedly.")
			return
		}
		switch sc.s[sc.i] {
		case ',':
			sc.i++
			sc.ws()
		case closer:
			sc.i++
			sc.depth--
			return
		default:
			sc.syntax("")
			return
		}
	}
}

func (sc *jsonScanner) str() {
	sc.i++ // opening quote
	hiSurrogate := -1
	for sc.i < len(sc.s) {
		c := sc.s[sc.i]
		switch {
		case c == '"':
			sc.i++
			if sc.binary && hiSurrogate != -1 {
				sc.syntax("Unicode low surrogate must follow a high surrogate.")
			}
			return
		case c == '\\':
			sc.i++
			if sc.i >= len(sc.s) {
				sc.syntax("The input string ended unexpectedly.")
				return
			}
			esc := sc.s[sc.i]
			sc.i++
			if esc != 'u' {
				switch esc {
				case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				default:
					sc.syntax(fmt.Sprintf("Escape sequence \"\\%c\" is invalid.", esc))
					return
				}
				if sc.binary && hiSurrogate != -1 {
					sc.syntax("Unicode low surrogate must follow a high surrogate.")
					return
				}
				continue
			}
			ch := 0
			for k := 0; k < 4; k++ {
				if sc.i >= len(sc.s) {
					sc.syntax("The input string ended unexpectedly.")
					return
				}
				d, ok := digitValue(sc.s[sc.i], 16)
				if !ok {
					sc.syntax(`"\u" must be followed by four hexadecimal digits.`)
					return
				}
				ch = ch*16 + int(d)
				sc.i++
			}
			if !sc.binary {
				continue
			}
			switch {
			case ch >= 0xd800 && ch <= 0xdbff:
				if hiSurrogate != -1 {
					sc.syntax("Unicode high surrogate must not follow a high surrogate.")
					return
				}
				hiSurrogate = ch
				continue
			case ch >= 0xdc00 && ch <= 0xdfff:
				if hiSurrogate == -1 {
					sc.syntax("Unicode low surrogate must follow a high surrogate.")
					return
				}
				hiSurrogate = -1
				continue
			}
			if hiSurrogate != -1 {
				sc.syntax("Unicode low surrogate must follow a high surrogate.")
				return
			}
			if ch == 0 {
				sc.out = Outcome{
					Verdict:  Invalid,
					SQLState: StateUntranslatableChar,
					Title:    "unsupported Unicode escape sequence",
					Message:  "unsupported Unicode escape sequence",
					Detail:   `\u0000 cannot be converted to text.`,
				}
				sc.failed = true
				return
			}
		case c < 0x20:
			sc.syntax(fmt.Sprintf("Character with value 0x%02x must be escaped.", c))
			return
		default:
			if sc.binary && hiSurrogate != -1 {
				sc.syntax("Unicode low surrogate must follow a high surrogate.")
				return
			}
			sc.i++
		}
	}
	sc.syntax("The input string ended unexpectedly.")
}

func (sc *jsonScanner) number() {
	start := sc.i
	if sc.s[sc.i] == '-' {
		sc.i++
	}
	ok := true
	switch {
	case sc.i < len(sc.s) && sc.s[sc.i] == '0':
		sc.i++
	case sc.i < len(sc.s) && sc.s[sc.i] >= '1' && sc.s[sc.i] <= '9':
		for sc.i < len(sc.s) && isDigit(sc.s[sc.i]) {
			sc.i++
		}
	default:
		ok = false
	}
	if ok && sc.i < len(sc.s) && sc.s[sc.i] == '.' {
		sc.i++
		if sc.i >= len(sc.s) || !isDigit(sc.s[sc.i]) {
			ok = false
		}
		for sc.i < len(sc.s) && isDigit(sc.s[sc.i]) {
			sc.i++
		}
	}
	expDigits := 0
	if ok && sc.i < len(sc.s) && (sc.s[sc.i] == 'e' || sc.s[sc.i] == 'E') {
		sc.i++
		if sc.i < len(sc.s) && (sc.s[sc.i] == '+' || sc.s[sc.i] == '-') {
			sc.i++
		}
		if sc.i >= len(sc.s) || !isDigit(sc.s[sc.i]) {
			ok = false
		}
		for sc.i < len(sc.s) && isDigit(sc.s[sc.i]) {
			sc.i++
			expDigits++
		}
	}
	// Trailing letters or digits belong to the bad token, as in json_lex_number.
	for sc.i < len(sc.s) && isJSONAlnum(sc.s[sc.i]) {
		sc.i++
		ok = false
	}
	if !ok {
		sc.syntax("")
		return
	}
	// jsonb stores numbers as numeric, which has range limits of its own.
	if sc.binary && (expDigits > 4 || sc.i-start > 1000) {
		sc.undecided("jsonb number too large to decide offline")
	}
}
