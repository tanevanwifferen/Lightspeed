package cli

import (
	"context"
	"flag"
	"fmt"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// --symbol: naming a position by what it is rather than by where it is.
//
// PLAN §4 calls this the ergonomic win agents actually need, and the
// reason is unflattering but real: an agent asked for a byte column
// guesses, and a guessed column silently answers about the wrong
// identifier. A dotted name is something the agent already knows.
//
// Resolution goes through workspace/symbol — the one LSP request that
// searches by name — and then converts the answer back into the
// ordinary `file:line:col` location syntax. That is deliberate: the
// symbol path is resolved to a location and the commands then run
// exactly as if the caller had typed that location, so there is one
// query path in this package and not two. It also means the location
// is reported in the warnings, so the caller can see which symbol was
// picked and paste it back.
//
// Ambiguity is never resolved by guessing. See chooseSymbol.

// symbolCandidateLimit bounds how many candidates an error message
// names. An agent needs enough to disambiguate, not a directory
// listing.
const symbolCandidateLimit = 10

// symbolFlags are the flags that let a location-shaped command take a
// symbol path instead of a location.
type symbolFlags struct {
	// query is --symbol, e.g. "pkg.Type.Method".
	query string
	// path is the directory whose workspace is searched. A symbol
	// path names no file, so unlike every other location there is
	// nothing in the argument itself to route on.
	path string
}

func (f *symbolFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.query, "symbol", "",
		"resolve a dotted symbol path (pkg.Type.Method) to a location, instead of naming one")
	fs.StringVar(&f.path, "path", ".",
		"directory whose workspace --symbol searches")
}

// parseLocationFlags parses a command that names a position: either as
// a location argument or with --symbol.
//
// extra is the number of positional arguments that follow the
// location (1 for `rename <loc> <newname>`, 0 for the rest), so the
// accepted count is extra or extra+1 depending on which way the
// position was named. loc is the location argument as typed, empty
// when --symbol was used; rest is the remaining positional arguments.
func parseLocationFlags(e *env, c *command, args []string, extra int, register func(*flag.FlagSet)) (
	common *commonFlags, sf *symbolFlags, loc string, rest []string, err error) {
	sf = &symbolFlags{}
	common, positional, err := parseFlagsRange(e, c, args, extra, extra+1, func(fs *flag.FlagSet) {
		sf.register(fs)
		if register != nil {
			register(fs)
		}
	})
	if err != nil {
		return nil, nil, "", nil, err
	}

	if sf.query == "" {
		if len(positional) != extra+1 {
			return nil, nil, "", nil, render.Errorf(render.CodeUsage,
				"%s: expected a location (%s), or --symbol to name the position instead", c.Name, c.Args)
		}
		return common, sf, positional[0], positional[1:], nil
	}
	if len(positional) == extra+1 {
		return nil, nil, "", nil, render.Errorf(render.CodeUsage,
			"%s: %q and --symbol %q both name a position; give one or the other",
			c.Name, positional[0], sf.query)
	}
	return common, sf, "", positional, nil
}

// location resolves however the caller named the position into the
// location syntax the rest of this package speaks. It returns the
// warnings the resolution produced — including which symbol was
// chosen, which is not a detail a caller should have to infer.
func (e *env) location(common *commonFlags, sf *symbolFlags, loc string) (string, []string, error) {
	if sf == nil || sf.query == "" {
		return loc, nil, nil
	}
	return resolveSymbol(e, common, sf)
}

// resolveSymbol turns a dotted symbol path into a location.
//
// It runs in its own short-lived session: the symbol names no file, so
// the server to ask is the one that handles --path, and the file the
// answer points at may be handled by a different one. Two sessions is
// the honest cost of that — with the daemon's server pool it is two
// lookups rather than two server startups.
func resolveSymbol(e *env, common *commonFlags, sf *symbolFlags) (string, []string, error) {
	segments, err := symbolSegments(sf.query)
	if err != nil {
		return "", nil, err
	}

	match, err := resolveWorkspace(sf.path, "", common.server)
	if err != nil {
		return "", nil, err
	}

	connectCtx, cancelConnect := context.WithTimeout(context.Background(), common.timeout)
	defer cancelConnect()
	s, err := startSession(connectCtx, e, match, common.gateOptions())
	if err != nil {
		return "", nil, err
	}
	defer s.close()

	ctx, cancel := context.WithTimeout(context.Background(), common.timeout+gateSlack)
	defer cancel()
	// The query is the last segment only. workspace/symbol matches
	// names, not paths: servers differ on whether they do substring,
	// fuzzy or prefix matching, and every one of them can find a
	// symbol by its own name. The path is applied here instead, where
	// the rule is written down and testable.
	res, err := s.query(ctx, methodWorkspaceSymbol, map[string]any{"query": segments[len(segments)-1]})
	if err != nil {
		return "", nil, err
	}
	syms, err := decodeWorkspaceSymbols(res.Result)
	if err != nil {
		return "", nil, err
	}
	loc, warnings, err := chooseSymbol(s, sf.query, segments, syms)
	return loc, append(res.Warnings, warnings...), err
}

// symbolSegments splits a dotted symbol path, rejecting the shapes
// that cannot match anything.
func symbolSegments(query string) ([]string, error) {
	segments := strings.Split(query, ".")
	for _, seg := range segments {
		if strings.TrimSpace(seg) == "" {
			return nil, render.Errorf(render.CodeUsage,
				"--symbol %q is not a symbol path; write a dotted name such as pkg.Type.Method", query)
		}
	}
	return segments, nil
}

// chooseSymbol picks the one symbol a path names, or refuses.
//
// The matching rule, in full, because a caller has to be able to
// predict it:
//
//  1. A candidate's own dotted path is the server's containerName and
//     name joined with a dot — exactly what `workspace_symbol` prints.
//  2. The query matches when its segments are a suffix of that path,
//     compared segment by segment and case-sensitively. So
//     `Type.Method` matches `pkg.Type.Method`, and `Method` matches
//     both, and `Other.Method` matches neither.
//  3. If any candidate matches the query *exactly*, the inexact ones
//     are discarded. Otherwise `Server` would be ambiguous with
//     `Server.Server` in a package that has both.
//  4. Candidates that resolve to the same file and range are one
//     candidate. Servers do send duplicates, and duplicate-driven
//     ambiguity would be ambiguity we invented.
//
// Anything still ambiguous after that is an error naming every
// candidate as a location — never a silent first match. Picking one
// for the caller is how an agent renames the wrong Handle.
func chooseSymbol(s *session, query string, segments []string, syms []symbol) (string, []string, error) {
	var (
		matches []symbol
		exact   []symbol
	)
	for _, sym := range syms {
		if !matchesSymbolPath(sym, segments) {
			continue
		}
		matches = append(matches, sym)
		if sym.Qualified == query {
			exact = append(exact, sym)
		}
	}
	if len(exact) > 0 {
		matches = exact
	}
	matches = dedupeSymbols(matches)

	switch len(matches) {
	case 0:
		return "", nil, noSuchSymbolError(query, syms)
	case 1:
	default:
		return "", nil, ambiguousSymbolError(s, query, matches)
	}

	sym := matches[0]
	if !sym.HasRange {
		return "", nil, render.Errorf(render.CodeNotFound,
			"--symbol %q: %s told us the symbol is in %s but not where; name a location instead",
			query, s.match.Server.Name, sym.URI)
	}
	where, err := symbolLocation(s, sym)
	if err != nil {
		return "", nil, err
	}
	return where, []string{fmt.Sprintf("--symbol %q resolved to %s", query, where)}, nil
}

// matchesSymbolPath reports whether the query segments are a
// segment-boundary suffix of the symbol's own dotted path. Comparing
// whole segments is what keeps `Handler` from matching `MyHandler`.
func matchesSymbolPath(sym symbol, segments []string) bool {
	have := strings.Split(sym.Qualified, ".")
	if len(segments) > len(have) {
		return false
	}
	return slices.Equal(have[len(have)-len(segments):], segments)
}

// dedupeSymbols collapses candidates that point at the same place.
func dedupeSymbols(syms []symbol) []symbol {
	type key struct {
		uri  protocol.DocumentURI
		rng  protocol.Range
		name string
	}
	seen := make(map[key]bool, len(syms))
	out := make([]symbol, 0, len(syms))
	for _, sym := range syms {
		k := key{sym.URI, sym.Range, sym.Qualified}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, sym)
	}
	return out
}

// symbolLocation renders a symbol as a location argument: a 1-based
// line and *byte* column, converted from the server's UTF-16 position
// through the vendored Mapper, so that feeding it back to lightspeed
// asks about the same identifier (PLAN §5.1).
func symbolLocation(s *session, sym symbol) (string, error) {
	m, err := s.docs.MapperForURI(sym.URI)
	if err != nil {
		return "", render.Errorf(render.CodeIOError,
			"--symbol: the symbol is in %s, which cannot be read: %v", sym.URI, err)
	}
	span, err := render.NewSpan(m, sym.Range)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:%d:%d", span.Path, span.Start.Line, span.Start.Column), nil
}

// noSuchSymbolError reports a symbol path that matched nothing, and
// names what the server did offer for the same last segment — which is
// usually enough to see that the container prefix is wrong.
func noSuchSymbolError(query string, syms []symbol) error {
	names := make([]string, 0, len(syms))
	for _, sym := range syms {
		if len(names) == symbolCandidateLimit {
			names = append(names, "…")
			break
		}
		names = append(names, sym.Qualified)
	}
	msg := fmt.Sprintf("--symbol %q matched no symbol", query)
	if len(names) > 0 {
		msg += "; the server offered: " + strings.Join(names, ", ")
	}
	return render.Errorf(render.CodeNotFound, "%s", msg).
		WithDetails(map[string]any{"symbol": query, "offered": names})
}

// ambiguousSymbolError refuses to choose between several matches.
//
// It is exit 2 rather than exit 1: the command did not fail to find
// anything, the invocation failed to identify one thing, and the fix
// is on the command line. Every candidate is reported as a location so
// that the retry is a copy-paste rather than a second search.
func ambiguousSymbolError(s *session, query string, matches []symbol) error {
	type candidate struct {
		Symbol   string `json:"symbol"`
		Kind     string `json:"kind,omitempty"`
		Location string `json:"location"`
	}
	candidates := make([]candidate, 0, len(matches))
	described := make([]string, 0, len(matches))
	for _, sym := range matches {
		where, err := symbolLocation(s, sym)
		if err != nil || !sym.HasRange {
			// Unreadable or position-less: still a candidate, and
			// hiding it would understate the ambiguity.
			where = string(sym.URI)
		}
		candidates = append(candidates, candidate{Symbol: sym.Qualified, Kind: sym.Kind, Location: where})
		if len(described) < symbolCandidateLimit {
			described = append(described, fmt.Sprintf("%s at %s", sym.Qualified, where))
		}
	}
	if len(matches) > len(described) {
		described = append(described, fmt.Sprintf("… and %d more", len(matches)-len(described)))
	}
	return render.Errorf(render.CodeUsage,
		"--symbol %q matches %d symbols; pass one of these locations instead: %s",
		query, len(matches), strings.Join(described, "; ")).
		WithDetails(map[string]any{"symbol": query, "candidates": candidates})
}
