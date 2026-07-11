// Package source loads desired database state without connecting to a target
// database. SQL is first split into location-aware statements so parsers and
// diagnostics can retain the original source coordinates.
package source

import (
	"context"
	"fmt"
	"strings"
	"unicode"
)

// Position identifies a one-based location in a source document.
type Position struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// Statement is a complete SQL statement and its starting position.
type Statement struct {
	SQL      string   `json:"sql"`
	Position Position `json:"position"`
}

// SplitSQL separates PostgreSQL statements while respecting quoted strings,
// identifiers, comments, and dollar-quoted function bodies.
func SplitSQL(file, input string) ([]Statement, error) {
	return SplitSQLContext(context.Background(), file, input)
}

// SplitSQLContext is SplitSQL with cooperative cancellation for large inputs.
func SplitSQLContext(ctx context.Context, file, input string) ([]Statement, error) {
	var out []Statement
	start, line, column := -1, 1, 1
	startLine, startColumn := 1, 1
	quote := byte(0)
	dollarTag := ""
	lineComment, blockDepth := false, 0

	advance := func(b byte) {
		if b == '\n' {
			line++
			column = 1
		} else {
			column++
		}
	}
	flush := func(end int) {
		if start < 0 {
			return
		}
		text := strings.TrimSpace(input[start:end])
		if text != "" {
			out = append(out, Statement{SQL: text, Position: Position{File: file, Line: startLine, Column: startColumn}})
		}
		start = -1
	}

	for i := 0; i < len(input); {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		b := input[i]
		if start < 0 && !unicode.IsSpace(rune(b)) {
			start, startLine, startColumn = i, line, column
		}
		if lineComment {
			advance(b)
			i++
			if b == '\n' {
				lineComment = false
			}
			continue
		}
		if blockDepth > 0 {
			if i+1 < len(input) && input[i:i+2] == "/*" {
				blockDepth++
				advance('/')
				advance('*')
				i += 2
				continue
			}
			if i+1 < len(input) && input[i:i+2] == "*/" {
				blockDepth--
				advance('*')
				advance('/')
				i += 2
				continue
			}
			advance(b)
			i++
			continue
		}
		if dollarTag != "" {
			if strings.HasPrefix(input[i:], dollarTag) {
				for j := 0; j < len(dollarTag); j++ {
					advance(input[i+j])
				}
				i += len(dollarTag)
				dollarTag = ""
				continue
			}
			advance(b)
			i++
			continue
		}
		if quote != 0 {
			if b == quote {
				if i+1 < len(input) && input[i+1] == quote {
					advance(b)
					advance(input[i+1])
					i += 2
					continue
				}
				quote = 0
			}
			advance(b)
			i++
			continue
		}

		if i+1 < len(input) && input[i:i+2] == "--" {
			lineComment = true
			advance('-')
			advance('-')
			i += 2
			continue
		}
		if i+1 < len(input) && input[i:i+2] == "/*" {
			blockDepth = 1
			advance('/')
			advance('*')
			i += 2
			continue
		}
		if b == '\'' || b == '"' {
			quote = b
			advance(b)
			i++
			continue
		}
		if b == '$' {
			if tag, ok := dollarQuoteAt(input[i:]); ok {
				dollarTag = tag
				for j := 0; j < len(tag); j++ {
					advance(tag[j])
				}
				i += len(tag)
				continue
			}
		}
		if b == ';' {
			flush(i)
			advance(b)
			i++
			continue
		}
		advance(b)
		i++
	}
	if quote != 0 {
		return nil, fmt.Errorf("%s:%d:%d: unterminated quoted value", file, startLine, startColumn)
	}
	if dollarTag != "" {
		return nil, fmt.Errorf("%s:%d:%d: unterminated dollar quote %s", file, startLine, startColumn, dollarTag)
	}
	if blockDepth != 0 {
		return nil, fmt.Errorf("%s:%d:%d: unterminated block comment", file, startLine, startColumn)
	}
	flush(len(input))
	return out, nil
}

func dollarQuoteAt(s string) (string, bool) {
	if len(s) < 2 || s[0] != '$' {
		return "", false
	}
	for i := 1; i < len(s); i++ {
		switch b := s[i]; {
		case b == '$':
			return s[:i+1], true
		case b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || i > 1 && b >= '0' && b <= '9':
			continue
		default:
			return "", false
		}
	}
	return "", false
}
