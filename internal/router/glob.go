package router

import (
	"fmt"
	"path"
	"strings"
)

// A Glob is a compiled activation pattern. Compiling up front turns a
// malformed pattern into one clear error at config-load time instead
// of a silent non-match on every query.
type Glob struct {
	pattern string
	// alts are the brace-free alternatives the pattern expands to,
	// each already split into slash-separated segments.
	alts [][]string
	// absolute records that the pattern was rooted at "/" and so is
	// matched against absolute paths rather than workspace-relative
	// ones.
	absolute bool
}

// maxGlobAlternatives caps brace expansion. Nested braces multiply,
// and a definition file is untrusted input in the sense that it may
// simply be wrong; refusing is better than allocating for a while.
const maxGlobAlternatives = 256

// CompileGlob compiles one activation pattern. The supported syntax is
// the usual editor/LSP glob:
//
//	"*"      any run of characters within one path segment
//	"?"      any single character within one path segment
//	"[a-z]"  character class, as in [path.Match]
//	"**"     zero or more whole path segments
//	"{a,b}"  alternatives
//
// "**" means zero or more segments everywhere, so "**/*.go" claims
// "main.go" at the root and a trailing "a/**" claims "a" itself as
// well as everything below it.
//
// Matching is case-sensitive and always uses "/" as the separator. A
// pattern beginning with "/" is matched against the absolute path;
// every other pattern is matched against the path relative to the
// resolved workspace root, which is what keeps an anchored pattern
// such as "src/**/*.rs" from claiming files in a nested project.
func CompileGlob(pattern string) (*Glob, error) {
	if pattern == "" {
		return nil, fmt.Errorf("glob: pattern is empty")
	}
	expanded, err := expandBraces(pattern)
	if err != nil {
		return nil, fmt.Errorf("glob %q: %w", pattern, err)
	}
	g := &Glob{pattern: pattern, absolute: strings.HasPrefix(pattern, "/")}
	for _, alt := range expanded {
		alt = strings.TrimPrefix(alt, "/")
		segments := strings.Split(alt, "/")
		for _, seg := range segments {
			// path.Match reports ErrBadPattern for an unterminated
			// character class; find that now rather than never.
			if _, err := path.Match(seg, ""); err != nil {
				return nil, fmt.Errorf("glob %q: bad segment %q: %w", pattern, seg, err)
			}
		}
		g.alts = append(g.alts, segments)
	}
	return g, nil
}

// Pattern returns the pattern the glob was compiled from.
func (g *Glob) Pattern() string { return g.pattern }

// Absolute reports whether the glob matches absolute paths.
func (g *Glob) Absolute() bool { return g.absolute }

// Match reports whether the slash-separated path matches. A leading
// "./" or "/" on the path is ignored; empty path segments are not.
func (g *Glob) Match(p string) bool {
	p = strings.TrimPrefix(p, "./")
	p = strings.TrimPrefix(p, "/")
	if p == "" {
		return false
	}
	segments := strings.Split(p, "/")
	for _, alt := range g.alts {
		if matchSegments(alt, segments) {
			return true
		}
	}
	return false
}

// matchSegments matches pattern segments against path segments,
// treating "**" as zero or more segments.
func matchSegments(pattern, name []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			// Collapse runs of "**" and handle a trailing one,
			// which matches everything that is left.
			rest := pattern[1:]
			for len(rest) > 0 && rest[0] == "**" {
				rest = rest[1:]
			}
			if len(rest) == 0 {
				return true
			}
			// "**" matches zero segments first, so that "**/*.go"
			// also matches "main.go" at the root.
			for i := 0; i <= len(name); i++ {
				if matchSegments(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		ok, err := path.Match(pattern[0], name[0])
		if err != nil || !ok {
			return false
		}
		pattern, name = pattern[1:], name[1:]
	}
	return len(name) == 0
}

// expandBraces rewrites "a{b,c}d" into ["abd","acd"], recursively.
func expandBraces(pattern string) ([]string, error) {
	open := strings.IndexByte(pattern, '{')
	if open < 0 {
		if strings.IndexByte(pattern, '}') >= 0 {
			return nil, fmt.Errorf("unmatched '}'")
		}
		return []string{pattern}, nil
	}

	// Find the matching brace, respecting nesting.
	depth, closing := 0, -1
	for i := open; i < len(pattern); i++ {
		switch pattern[i] {
		case '{':
			depth++
		case '}':
			if depth--; depth == 0 {
				closing = i
			}
		}
		if closing >= 0 {
			break
		}
	}
	if closing < 0 {
		return nil, fmt.Errorf("unmatched '{'")
	}

	prefix, suffix := pattern[:open], pattern[closing+1:]
	var out []string
	for _, choice := range splitAlternatives(pattern[open+1 : closing]) {
		expanded, err := expandBraces(prefix + choice + suffix)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded...)
		if len(out) > maxGlobAlternatives {
			return nil, fmt.Errorf("brace expansion exceeds %d alternatives", maxGlobAlternatives)
		}
	}
	return out, nil
}

// splitAlternatives splits on commas at nesting depth zero.
func splitAlternatives(s string) []string {
	var out []string
	depth, start := 0, 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '{':
			depth++
		case '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}
