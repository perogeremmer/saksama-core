// Package parser implements a deliberately small, fail-closed tokenizer and
// statement splitter for the MySQL ALTER TABLE / CREATE TABLE subset that
// Saksama supports.
//
// Design rules (see PRD FR-1):
//   - Never guess. Anything the tokenizer cannot classify is an error.
//   - Handles backtick identifiers, quoted strings, comments, and nested parens.
//   - Multi-statement input is rejected, not split.
package parser

import (
	"fmt"
	"strings"
)

// Kind classifies a token.
type Kind int

const (
	KindWord   Kind = iota // bare keyword or unquoted identifier
	KindIdent              // `backticked`
	KindString             // 'single' or "double" quoted, quotes stripped
	KindNumber             // numeric literal
	KindPunct              // ( ) , = + - etc.
)

// Token is a single lexical unit.
type Token struct {
	Kind   Kind
	Value  string // unquoted/normalized value
	Raw    string // original source text
	Offset int    // byte offset of the token start
}

// IsWord reports whether the token is a bare word equal to s (case-insensitive).
func (t Token) IsWord(s string) bool {
	return t.Kind == KindWord && strings.EqualFold(t.Value, s)
}

// IsPunct reports whether the token is the given punctuation character.
func (t Token) IsPunct(s string) bool {
	return t.Kind == KindPunct && t.Value == s
}

// IsIdentifier reports whether the token can name an object.
func (t Token) IsIdentifier() bool {
	return t.Kind == KindIdent || t.Kind == KindWord
}

func (t Token) String() string {
	switch t.Kind {
	case KindString:
		return "'" + t.Value + "'"
	case KindIdent:
		return "`" + t.Value + "`"
	default:
		return t.Raw
	}
}

// Tokenize converts SQL source into tokens. Comments are dropped.
func Tokenize(src string) ([]Token, error) {
	var out []Token
	i := 0
	n := len(src)

	for i < n {
		c := src[i]

		// Whitespace.
		if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' || c == '\v' {
			i++
			continue
		}

		// Comments: -- ... , # ... , /* ... */
		if c == '-' && i+1 < n && src[i+1] == '-' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '#' {
			for i < n && src[i] != '\n' {
				i++
			}
			continue
		}
		if c == '/' && i+1 < n && src[i+1] == '*' {
			end := strings.Index(src[i+2:], "*/")
			if end < 0 {
				return nil, fmt.Errorf("unterminated block comment at offset %d", i)
			}
			i += 2 + end + 2
			continue
		}

		// Backtick identifier.
		if c == '`' {
			start := i
			i++
			var b strings.Builder
			closed := false
			for i < n {
				if src[i] == '`' {
					if i+1 < n && src[i+1] == '`' { // escaped backtick
						b.WriteByte('`')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteByte(src[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated backtick identifier at offset %d", start)
			}
			out = append(out, Token{Kind: KindIdent, Value: b.String(), Raw: src[start:i], Offset: start})
			continue
		}

		// Quoted string.
		if c == '\'' || c == '"' {
			start := i
			quote := c
			i++
			var b strings.Builder
			closed := false
			for i < n {
				if src[i] == '\\' && i+1 < n {
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				if src[i] == quote {
					if i+1 < n && src[i+1] == quote { // doubled quote escape
						b.WriteByte(quote)
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				b.WriteByte(src[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string literal at offset %d", start)
			}
			out = append(out, Token{Kind: KindString, Value: b.String(), Raw: src[start:i], Offset: start})
			continue
		}

		// Number.
		if c >= '0' && c <= '9' {
			start := i
			for i < n && (src[i] >= '0' && src[i] <= '9' || src[i] == '.') {
				i++
			}
			out = append(out, Token{Kind: KindNumber, Value: src[start:i], Raw: src[start:i], Offset: start})
			continue
		}

		// Word.
		if isWordStart(c) {
			start := i
			for i < n && isWordPart(src[i]) {
				i++
			}
			out = append(out, Token{Kind: KindWord, Value: src[start:i], Raw: src[start:i], Offset: start})
			continue
		}

		// Punctuation (single byte).
		if strings.ContainsRune("(),;=.+-*/<>!@|&^~%?:", rune(c)) {
			out = append(out, Token{Kind: KindPunct, Value: string(c), Raw: string(c), Offset: i})
			i++
			continue
		}

		return nil, fmt.Errorf("unexpected character %q at offset %d", string(rune(c)), i)
	}

	return out, nil
}

func isWordStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$' || c >= 0x80
}

func isWordPart(c byte) bool {
	return isWordStart(c) || c >= '0' && c <= '9'
}

// SplitTopLevel splits tokens on commas that are not nested inside parentheses.
func SplitTopLevel(tokens []Token) [][]Token {
	var parts [][]Token
	depth := 0
	start := 0
	for i, t := range tokens {
		switch {
		case t.IsPunct("("):
			depth++
		case t.IsPunct(")"):
			if depth > 0 {
				depth--
			}
		case t.IsPunct(",") && depth == 0:
			parts = append(parts, tokens[start:i])
			start = i + 1
		}
	}
	if start < len(tokens) {
		parts = append(parts, tokens[start:])
	}
	return parts
}

// RejectMultiStatement errors if the token stream contains a top-level semicolon
// that is followed by anything other than more semicolons.
func RejectMultiStatement(tokens []Token) error {
	for i, t := range tokens {
		if !t.IsPunct(";") {
			continue
		}
		for _, rest := range tokens[i+1:] {
			if rest.IsPunct(";") {
				continue
			}
			return fmt.Errorf("multi-statement input is not allowed (second statement starts at offset %d)", rest.Offset)
		}
	}
	return nil
}

// TrimSemicolons removes trailing top-level semicolons.
func TrimSemicolons(tokens []Token) []Token {
	for len(tokens) > 0 && tokens[len(tokens)-1].IsPunct(";") {
		tokens = tokens[:len(tokens)-1]
	}
	return tokens
}

// JoinTokens renders tokens back into a readable normalized form.
func JoinTokens(tokens []Token) string {
	var b strings.Builder
	for i, t := range tokens {
		if i > 0 && needsSpace(tokens[i-1], t) {
			b.WriteByte(' ')
		}
		b.WriteString(t.String())
	}
	return b.String()
}

func needsSpace(prev, cur Token) bool {
	if cur.IsPunct(",") || cur.IsPunct(")") {
		return false
	}
	if prev.IsPunct("(") {
		return false
	}
	if prev.IsPunct(",") {
		return true
	}
	return true
}
