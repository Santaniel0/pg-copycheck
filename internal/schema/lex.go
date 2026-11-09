package schema

import (
	"fmt"
	"strings"
)

type tokKind uint8

const (
	tokIdent  tokKind = iota // unquoted identifier or keyword, lower-cased
	tokQuoted                // "quoted identifier", case kept
	tokString                // 'string', E'string' or $$string$$
	tokNumber
	tokPunct
)

type token struct {
	kind tokKind
	text string
	line int
}

// word returns the keyword text of an unquoted identifier and "" for
// anything else, so that a quoted "check" never matches the keyword.
func (t token) word() string {
	if t.kind == tokIdent {
		return t.text
	}
	return ""
}

func (t token) is(punct string) bool { return t.kind == tokPunct && t.text == punct }

func (t token) isName() bool { return t.kind == tokIdent || t.kind == tokQuoted }

// SyntaxError reports DDL the lexer cannot tokenize.
type SyntaxError struct {
	Line int
	Msg  string
}

func (e *SyntaxError) Error() string { return fmt.Sprintf("DDL line %d: %s", e.Line, e.Msg) }

func isIdentStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func isIdentPart(c byte) bool {
	return isIdentStart(c) || c >= '0' && c <= '9' || c == '$'
}

// lex tokenizes SQL the way the parts of the PostgreSQL lexer that matter for
// schema dumps do. psql meta-commands such as \restrict, which recent
// pg_dump versions emit, are skipped when they start a statement.
func lex(src string) ([]token, error) {
	var toks []token
	line := 1
	stmtStart := true
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r' || c == '\f' || c == '\v':
			i++
		case c == '-' && strings.HasPrefix(src[i:], "--"):
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '/' && strings.HasPrefix(src[i:], "/*"):
			start, depth := line, 0
			for {
				if i >= len(src) {
					return nil, &SyntaxError{start, "unterminated /* comment"}
				}
				switch {
				case strings.HasPrefix(src[i:], "/*"):
					depth++
					i += 2
				case strings.HasPrefix(src[i:], "*/"):
					depth--
					i += 2
				default:
					if src[i] == '\n' {
						line++
					}
					i++
				}
				if depth == 0 {
					break
				}
			}
		case c == '\\' && stmtStart:
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case c == '\'':
			s, n, lines, err := lexString(src[i:], false)
			if err != nil {
				return nil, &SyntaxError{line, err.Error()}
			}
			toks = append(toks, token{tokString, s, line})
			i += n
			line += lines
		case c == '"':
			j := i + 1
			var b strings.Builder
			for {
				if j >= len(src) {
					return nil, &SyntaxError{line, "unterminated quoted identifier"}
				}
				if src[j] == '"' {
					if j+1 < len(src) && src[j+1] == '"' {
						b.WriteByte('"')
						j += 2
						continue
					}
					break
				}
				b.WriteByte(src[j])
				j++
			}
			toks = append(toks, token{tokQuoted, b.String(), line})
			i = j + 1
		case c == '$':
			end := strings.IndexByte(src[i+1:], '$')
			tag := ""
			if end >= 0 {
				tag = src[i : i+end+2]
			}
			if end < 0 || !validDollarTag(tag) {
				toks = append(toks, token{tokPunct, "$", line})
				i++
				break
			}
			body := src[i+len(tag):]
			close := strings.Index(body, tag)
			if close < 0 {
				return nil, &SyntaxError{line, "unterminated dollar-quoted string"}
			}
			toks = append(toks, token{tokString, body[:close], line})
			line += strings.Count(body[:close], "\n")
			i += len(tag) + close + len(tag)
		case c >= '0' && c <= '9':
			j := i
			for j < len(src) && (src[j] >= '0' && src[j] <= '9' || src[j] == '.') {
				j++
			}
			toks = append(toks, token{tokNumber, src[i:j], line})
			i = j
		case isIdentStart(c):
			j := i
			for j < len(src) && isIdentPart(src[j]) {
				j++
			}
			word := strings.ToLower(src[i:j])
			if (word == "e") && j < len(src) && src[j] == '\'' {
				s, n, lines, err := lexString(src[j:], true)
				if err != nil {
					return nil, &SyntaxError{line, err.Error()}
				}
				toks = append(toks, token{tokString, s, line})
				i = j + n
				line += lines
				break
			}
			toks = append(toks, token{tokIdent, word, line})
			i = j
		case c == ':' && strings.HasPrefix(src[i:], "::"):
			toks = append(toks, token{tokPunct, "::", line})
			i += 2
		default:
			toks = append(toks, token{tokPunct, string(c), line})
			i++
		}
		if n := len(toks); n > 0 {
			stmtStart = toks[n-1].is(";")
		}
	}
	return toks, nil
}

func validDollarTag(tag string) bool {
	inner := tag[1 : len(tag)-1]
	if inner == "" {
		return true
	}
	if !isIdentStart(inner[0]) {
		return false
	}
	for i := 1; i < len(inner); i++ {
		if !isIdentPart(inner[i]) || inner[i] == '$' {
			return false
		}
	}
	return true
}

// lexString reads a single-quoted string literal that starts at s[0]. It returns the
// value, the bytes consumed and the newlines crossed.
func lexString(s string, backslash bool) (string, int, int, error) {
	var b strings.Builder
	lines := 0
	for j := 1; j < len(s); j++ {
		c := s[j]
		switch {
		case c == '\'' && j+1 < len(s) && s[j+1] == '\'':
			b.WriteByte('\'')
			j++
		case c == '\'':
			return b.String(), j + 1, lines, nil
		case c == '\\' && backslash && j+1 < len(s):
			j++
			b.WriteByte(s[j])
		default:
			if c == '\n' {
				lines++
			}
			b.WriteByte(c)
		}
	}
	return "", 0, 0, fmt.Errorf("unterminated string literal")
}
