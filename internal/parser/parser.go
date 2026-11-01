// Package parser turns a line of user input into a command and arguments.
//
// It splits on whitespace but keeps double-quoted text together, so
//
//	SET greeting "hello world"
//
// becomes ["SET", "greeting", "hello world"]. Inside quotes, \" and \\ are
// escapes for a literal quote and backslash.
package parser

import (
	"errors"
	"strings"
)

// ErrUnclosedQuote is returned when a quoted string never ends.
var ErrUnclosedQuote = errors.New("unclosed quote")

// Command is one parsed line of input.
type Command struct {
	Name string   // upper-cased, e.g. "SET"
	Args []string // everything after the name
}

// Parse splits line into a Command. An empty or blank line returns a Command
// with an empty Name.
func Parse(line string) (Command, error) {
	tokens, err := tokenize(line)
	if err != nil {
		return Command{}, err
	}
	if len(tokens) == 0 {
		return Command{}, nil
	}
	return Command{Name: strings.ToUpper(tokens[0]), Args: tokens[1:]}, nil
}

func tokenize(line string) ([]string, error) {
	var (
		tokens  []string
		current strings.Builder
		inQuote bool
		hasTok  bool // true once the current token has started (allows "")
	)

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		switch {
		case inQuote && r == '\\' && i+1 < len(runes) && (runes[i+1] == '"' || runes[i+1] == '\\'):
			current.WriteRune(runes[i+1])
			i++
		case r == '"':
			inQuote = !inQuote
			hasTok = true
		case !inQuote && (r == ' ' || r == '\t'):
			if hasTok {
				tokens = append(tokens, current.String())
				current.Reset()
				hasTok = false
			}
		default:
			current.WriteRune(r)
			hasTok = true
		}
	}

	if inQuote {
		return nil, ErrUnclosedQuote
	}
	if hasTok {
		tokens = append(tokens, current.String())
	}
	return tokens, nil
}
