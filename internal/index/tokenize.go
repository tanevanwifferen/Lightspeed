package index

import (
	"strings"
	"unicode"
)

// Tokenize splits an identifier, a signature or a sentence into lowercase
// search tokens. Boundaries are: anything that is not a letter or a digit
// (`_`, `-`, `.`, spaces, punctuation), a lower-to-upper camel step
// (`parseConfig` → parse, config), the end of an acronym run
// (`HTTPServer` → http, server), and the step between letters and digits
// (`v2` → v, 2). Empty tokens are dropped. It is unicode-aware: a token is a
// run of one script's letters or of digits, and a rune with no case (CJK)
// never splits.
func Tokenize(s string) []string {
	var (
		out []string
		cur []rune
	)
	flush := func() {
		if len(cur) > 0 {
			out = append(out, strings.ToLower(string(cur)))
			cur = cur[:0]
		}
	}
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			flush()
			continue
		}
		if len(cur) > 0 {
			prev := cur[len(cur)-1]
			switch {
			case unicode.IsDigit(prev) != unicode.IsDigit(r):
				flush() // v2 → v, 2
			case unicode.IsLetter(prev) && unicode.IsLetter(r) && cased(prev) != cased(r):
				flush() // 名前Name → 名前, name: an uncased script never joins a cased one
			case unicode.IsLower(prev) && unicode.IsUpper(r):
				flush() // parseConfig → parse, Config
			case unicode.IsUpper(prev) && unicode.IsUpper(r) &&
				i+1 < len(runes) && unicode.IsLower(runes[i+1]):
				flush() // HTTPServer → HTTP, Server
			}
		}
		cur = append(cur, r)
	}
	flush()
	return out
}

func cased(r rune) bool { return unicode.IsUpper(r) || unicode.IsLower(r) }

// normalise is a name with case and every separator removed: the form in which
// `parse_config`, `ParseConfig` and `parse config` are the same word.
func normalise(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}
