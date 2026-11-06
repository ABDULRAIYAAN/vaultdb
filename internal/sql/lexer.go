// Package sql parses VaultDB's small SQL dialect.
//
// Running a query happens in three steps:
//
//	"SELECT name FROM students WHERE age > 20"
//	        |
//	   1. lexer    cuts the text into tokens: SELECT, name, FROM, students, ...
//	        |
//	   2. parser   arranges the tokens into a tree (the AST) that says what
//	        |      the query means: Select{Table: students, Where: age > 20}
//	   3. executor runs that tree against the stored rows (separate package)
//
// This file is step 1.
package sql

import (
	"fmt"
	"strings"
)

// TokenKind says what sort of token something is.
type TokenKind int

const (
	TokEOF     TokenKind = iota // end of input
	TokIdent                    // a name: students, age
	TokKeyword                  // a reserved word: SELECT, FROM, WHERE
	TokNumber                   // 42, 3.14
	TokString                   // 'hello'
	TokSymbol                   // ( ) , * = != <> < <= > >= ;
)

// Token is one piece of the query text.
type Token struct {
	Kind TokenKind
	Text string // keywords are upper-cased; strings have their quotes removed
	Pos  int    // byte offset in the query, for error messages
}

var keywords = map[string]bool{
	"SELECT": true, "FROM": true, "WHERE": true, "INSERT": true, "INTO": true,
	"VALUES": true, "UPDATE": true, "SET": true, "DELETE": true, "CREATE": true,
	"DROP": true, "TABLE": true, "PRIMARY": true, "KEY": true, "AND": true,
	"OR": true, "NOT": true, "NULL": true, "TRUE": true, "FALSE": true,
	"ORDER": true, "BY": true, "ASC": true, "DESC": true, "LIMIT": true,
	"COUNT": true, "INT": true, "FLOAT": true, "TEXT": true, "BOOL": true,
	"IS": true, "TABLES": true, "SHOW": true,
}

// Lex cuts query into tokens. The last token is always TokEOF.
func Lex(query string) ([]Token, error) {
	var toks []Token
	i := 0
	for i < len(query) {
		c := query[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++

		case c == '-' && i+1 < len(query) && query[i+1] == '-':
			// -- comment to end of line
			for i < len(query) && query[i] != '\n' {
				i++
			}

		case isIdentStart(rune(c)):
			start := i
			for i < len(query) && isIdentPart(rune(query[i])) {
				i++
			}
			word := query[start:i]
			if up := strings.ToUpper(word); keywords[up] {
				toks = append(toks, Token{TokKeyword, up, start})
			} else {
				toks = append(toks, Token{TokIdent, word, start})
			}

		case c >= '0' && c <= '9',
			c == '.' && i+1 < len(query) && query[i+1] >= '0' && query[i+1] <= '9':
			start := i
			seenDot := false
			for i < len(query) && (query[i] >= '0' && query[i] <= '9' || query[i] == '.' && !seenDot) {
				if query[i] == '.' {
					seenDot = true
				}
				i++
			}
			toks = append(toks, Token{TokNumber, query[start:i], start})

		case c == '\'':
			// 'it''s' -> it's  (two quotes inside a string mean one quote)
			start := i
			i++
			var b strings.Builder
			for {
				if i >= len(query) {
					return nil, fmt.Errorf("unclosed string starting at position %d", start)
				}
				if query[i] == '\'' {
					if i+1 < len(query) && query[i+1] == '\'' {
						b.WriteByte('\'')
						i += 2
						continue
					}
					i++
					break
				}
				b.WriteByte(query[i])
				i++
			}
			toks = append(toks, Token{TokString, b.String(), start})

		default:
			if two := peek2(query, i); two == "!=" || two == "<>" || two == "<=" || two == ">=" {
				toks = append(toks, Token{TokSymbol, two, i})
				i += 2
			} else if strings.IndexByte("(),*=<>;-", c) >= 0 {
				toks = append(toks, Token{TokSymbol, string(c), i})
				i++
			} else {
				return nil, fmt.Errorf("unexpected character %q at position %d", c, i)
			}
		}
	}
	return append(toks, Token{TokEOF, "", len(query)}), nil
}

func peek2(s string, i int) string {
	if i+2 <= len(s) {
		return s[i : i+2]
	}
	return ""
}

// Names are plain ASCII: letters, digits and underscores, not starting with
// a digit. (Values inside 'strings' can be any text.)
func isIdentStart(r rune) bool { return r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' }
func isIdentPart(r rune) bool  { return isIdentStart(r) || r >= '0' && r <= '9' }
