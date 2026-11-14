package sql

import (
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestLikeMatch(t *testing.T) {
	cases := []struct {
		text, pattern string
		want          bool
	}{
		{"", "", true},
		{"", "%", true},
		{"", "_", false},
		{"abc", "abc", true},
		{"abc", "ab", false},
		{"abc", "a%", true},
		{"abc", "%c", true},
		{"abc", "%b%", true},
		{"abc", "a_c", true},
		{"abc", "a__c", false},
		{"abc", "%%%", true},
		{"abc", "ABC", false}, // case-sensitive
		{"aaab", "%a%a%a%b", true},
		{"aaaa", "%a%a%a%a%b", false},
		{"mississippi", "m%iss%ppi", true},
		{"mississippi", "m%iss%ppx", false},
		{"50%", `50\%`, true},
		{"500", `50\%`, false},
		{"a_b", `a\_b`, true},
		{"axb", `a\_b`, false},
		{`a\b`, `a\\b`, true},
		{"héllo", "h_llo", true}, // _ is one character, not one byte
		{"日本語", "日_語", true},
		{"日本語", "%語", true},
	}
	for _, c := range cases {
		got, err := likeMatch(c.text, c.pattern)
		if err != nil {
			t.Fatalf("likeMatch(%q, %q): %v", c.text, c.pattern, err)
		}
		if got != c.want {
			t.Errorf("likeMatch(%q, %q) = %v, want %v", c.text, c.pattern, got, c.want)
		}
	}
	if _, err := likeMatch("x", `x\`); err == nil {
		t.Error("a pattern ending in a lone backslash should be an error")
	}
}

// likeRegexp is a slow but obviously correct LIKE: turn the pattern into a
// regular expression and let Go's regexp package do the matching.
func likeRegexp(text, pattern string) (bool, bool) {
	var b strings.Builder
	b.WriteString(`(?s)\A`)
	pr := []rune(pattern)
	for i := 0; i < len(pr); i++ {
		switch pr[i] {
		case '\\':
			if i+1 == len(pr) {
				return false, false
			}
			i++
			b.WriteString(regexp.QuoteMeta(string(pr[i])))
		case '%':
			b.WriteString(`.*`)
		case '_':
			b.WriteString(`.`)
		default:
			b.WriteString(regexp.QuoteMeta(string(pr[i])))
		}
	}
	b.WriteString(`\z`)
	return regexp.MustCompile(b.String()).MatchString(text), true
}

// FuzzLikeMatch checks the hand-written matcher against the regexp version
// on random texts and patterns. Run longer with:
//
//	go test ./internal/sql -fuzz FuzzLikeMatch
func FuzzLikeMatch(f *testing.F) {
	for _, s := range [][2]string{
		{"abc", "a%c"}, {"aaab", "%a%a%b"}, {"x", `\%`}, {"%", `\%`},
		{"héllo", "h_llo"}, {"", "%_"}, {"ab", "%%b%"}, {`a\`, `a\\`},
	} {
		f.Add(s[0], s[1])
	}
	f.Fuzz(func(t *testing.T, text, pattern string) {
		if !utf8.ValidString(text) || !utf8.ValidString(pattern) {
			t.Skip() // regexp and []rune disagree on broken UTF-8
		}
		want, ok := likeRegexp(text, pattern)
		got, err := likeMatch(text, pattern)
		if !ok {
			if err == nil {
				t.Fatalf("likeMatch(%q, %q) should reject the lone backslash", text, pattern)
			}
			return
		}
		if err != nil || got != want {
			t.Fatalf("likeMatch(%q, %q) = %v, %v; regexp says %v", text, pattern, got, err, want)
		}
	})
}
