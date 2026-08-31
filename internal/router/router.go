package router

import (
	"cmp"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// CodeNoServer is the machine-readable error code (PLAN §4) for "no
// server handles this file". The CLI maps it to exit code 3.
const CodeNoServer = "no_server"

// ErrNoServer is the sentinel behind every [NoServerError], for
// callers that only need errors.Is to decide on exit code 3.
var ErrNoServer = errors.New("no language server handles this file")

// A NoServerError reports that no definition claimed a path, and says
// enough about why for the user to fix it: the path as resolved and
// the language id that was looked for.
type NoServerError struct {
	// Path is the absolute, symlink-resolved path that was queried.
	Path string
	// LanguageID is the language id detected for Path, or "" if the
	// file type is not recognised, which is usually the real cause.
	LanguageID string
}

func (e *NoServerError) Error() string {
	if e.LanguageID == "" {
		return fmt.Sprintf("%s: %s (unrecognised file type)", e.Path, ErrNoServer)
	}
	return fmt.Sprintf("%s: %s (language %q)", e.Path, ErrNoServer, e.LanguageID)
}

func (e *NoServerError) Unwrap() error { return ErrNoServer }

// A Match is one server that handles a path, together with the
// workspace root it should be run in.
type Match struct {
	// Server is the definition that claimed the path. It is the
	// caller's own copy in the sense that the router never mutates
	// it.
	Server *serverdef.ServerDef

	// Root is the resolved workspace root: absolute, cleaned, with
	// symlinks resolved, so that it is usable as a session key.
	Root string

	// RootMarker is the marker that resolved Root, or "" if no
	// marker was found and Root fell back to the file's own
	// directory.
	RootMarker string

	// LanguageID is the language id used for the decision, and the
	// one to send in textDocument/didOpen. It may be "" when the
	// match came from a glob.
	LanguageID string

	// MatchedGlob is the activation glob that claimed the path, or
	// "" when the language id alone did.
	MatchedGlob string
}

// Fallback reports whether Root is the file's own directory because no
// root marker was found. Callers that need a real project root — or
// that want to warn — can check this instead of comparing paths.
func (m Match) Fallback() bool { return m.RootMarker == "" }

// A Router resolves paths against a fixed set of server definitions.
// It is immutable after [New] and safe for concurrent use; the only
// state it touches is the filesystem, read-only, to find roots.
type Router struct {
	entries []*entry
}

// entry is a definition with its activation patterns compiled once.
type entry struct {
	def   *serverdef.ServerDef
	globs []*Glob
}

// New builds a router over defs. Every definition is validated and
// every glob compiled here, so that a bad definition is one loud error
// at startup rather than a file that mysteriously has no server.
func New(defs ...*serverdef.ServerDef) (*Router, error) {
	r := &Router{}
	seen := make(map[string]bool, len(defs))
	for _, def := range defs {
		if err := def.Validate(); err != nil {
			return nil, err
		}
		if seen[def.Name] {
			return nil, fmt.Errorf("router: server %q is defined twice", def.Name)
		}
		seen[def.Name] = true

		e := &entry{def: def}
		for _, pattern := range def.Activation.Globs {
			g, err := CompileGlob(pattern)
			if err != nil {
				return nil, fmt.Errorf("server %q: %w", def.Name, err)
			}
			e.globs = append(e.globs, g)
		}
		r.entries = append(r.entries, e)
	}
	return r, nil
}

// Servers returns the definitions the router was built with, in the
// order they were given.
func (r *Router) Servers() []*serverdef.ServerDef {
	out := make([]*serverdef.ServerDef, len(r.entries))
	for i, e := range r.entries {
		out[i] = e.def
	}
	return out
}

// Resolve returns the servers that handle path, best first, each with
// its own workspace root. The path need not exist — an about-to-be
// created file still has a language and a root — and may be a
// directory, in which case the root search starts at the directory
// itself.
//
// If nothing claims the path, the error is a [*NoServerError] and the
// returned slice is nil.
func (r *Router) Resolve(path string) ([]Match, error) {
	return r.ResolveAs(path, "")
}

// ResolveAs is [Resolve] with the language id supplied by the caller,
// for the `--language` case and for files whose type this build does
// not recognise. An empty languageID means "detect it".
func (r *Router) ResolveAs(path, languageID string) ([]Match, error) {
	abs, err := canonical(path)
	if err != nil {
		return nil, err
	}
	if languageID == "" {
		languageID = LanguageID(abs)
	}
	start := startDir(abs)

	var matches []Match
	for _, e := range r.entries {
		// The cheap test first: a definition that neither knows the
		// language nor has globs cannot claim the file, and does not
		// deserve a walk up the tree.
		byLanguage := languageID != "" && slices.Contains(e.def.Activation.Languages, languageID)
		if !byLanguage && len(e.globs) == 0 {
			continue
		}

		root, marker := findRoot(start, e.def.Activation.RootMarkers)
		if root == "" {
			root, marker = start, ""
		}

		// The matched glob is recorded even when the language id
		// already claimed the file: it is the more specific reason,
		// and useful when explaining a decision.
		glob := ""
		if g := e.matchGlob(root, abs); g != nil {
			glob = g.Pattern()
		} else if !byLanguage {
			continue
		}

		matches = append(matches, Match{
			Server:      e.def,
			Root:        root,
			RootMarker:  marker,
			LanguageID:  languageID,
			MatchedGlob: glob,
		})
	}
	if len(matches) == 0 {
		return nil, &NoServerError{Path: abs, LanguageID: languageID}
	}
	sortMatches(matches)
	return matches, nil
}

// A Group is one server session's worth of work: the paths from a
// batch that share a server and a workspace root.
type Group struct {
	// Server, Root and RootMarker are as in [Match].
	Server     *serverdef.ServerDef
	Root       string
	RootMarker string

	// Paths are the inputs in this group, as they were passed in, in
	// input order.
	Paths []string
}

// Group resolves a batch of paths and buckets them by (server, root),
// which is how the daemon will want them: one bucket is one session,
// and a polyglot or multi-root batch simply yields more buckets.
//
// A path may appear in several groups, once per server that claims it.
// Paths that no server claims are returned in unclaimed, as given,
// rather than failing the batch: the caller decides whether one
// unhandled file among many is fatal. Groups are ordered as [Resolve]
// orders matches, then by root.
func (r *Router) Group(paths []string) (groups []Group, unclaimed []string, err error) {
	type key struct{ name, root string }
	index := map[key]int{}

	for _, p := range paths {
		matches, err := r.Resolve(p)
		if err != nil {
			if errors.Is(err, ErrNoServer) {
				unclaimed = append(unclaimed, p)
				continue
			}
			return nil, nil, err
		}
		for _, m := range matches {
			k := key{m.Server.Name, m.Root}
			i, ok := index[k]
			if !ok {
				groups = append(groups, Group{
					Server:     m.Server,
					Root:       m.Root,
					RootMarker: m.RootMarker,
				})
				i = len(groups) - 1
				index[k] = i
			}
			groups[i].Paths = append(groups[i].Paths, p)
		}
	}
	sortGroups(groups)
	return groups, unclaimed, nil
}

// matchGlob returns the first activation glob that claims abs, or nil.
// Relative patterns see the path relative to the definition's own
// root, absolute ones see the absolute path.
func (e *entry) matchGlob(root, abs string) *Glob {
	rel := relativeTo(root, abs)
	for _, g := range e.globs {
		subject := rel
		if g.Absolute() {
			subject = filepath.ToSlash(abs)
		}
		if g.Match(subject) {
			return g
		}
	}
	return nil
}

// sortMatches imposes the total order documented on the package:
// priority descending, then name.
func sortMatches(matches []Match) {
	slices.SortStableFunc(matches, func(x, y Match) int {
		a, b := x.Server, y.Server
		if c := cmp.Compare(b.Activation.Priority, a.Activation.Priority); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
}

func sortGroups(groups []Group) {
	slices.SortStableFunc(groups, func(a, b Group) int {
		if c := cmp.Compare(b.Server.Activation.Priority, a.Server.Activation.Priority); c != 0 {
			return c
		}
		if c := strings.Compare(a.Server.Name, b.Server.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Root, b.Root)
	})
}

// findRoot walks up from startDir looking for each marker in turn.
// Marker order is significance order, so an outer go.work beats an
// inner go.mod; see the package documentation.
func findRoot(startDir string, markers []string) (root, marker string) {
	for _, m := range markers {
		for dir := startDir; ; {
			if _, err := os.Lstat(filepath.Join(dir, m)); err == nil {
				return dir, m
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", ""
}

// startDir is the directory the root walk starts from: the path
// itself if it is a directory, otherwise its parent.
func startDir(abs string) string {
	if fi, err := os.Stat(abs); err == nil && fi.IsDir() {
		return abs
	}
	return filepath.Dir(abs)
}

// canonical makes a path absolute and resolves symlinks, so that two
// spellings of one file produce one workspace root and therefore one
// session. Paths that do not exist yet are canonicalised as far as
// their deepest existing ancestor.
func canonical(path string) (string, error) {
	if path == "" {
		return "", errors.New("router: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("router: %w", err)
	}
	return evalDeepest(filepath.Clean(abs)), nil
}

func evalDeepest(abs string) string {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs
	}
	return filepath.Join(evalDeepest(parent), filepath.Base(abs))
}

// relativeTo returns abs relative to root in slash form. Root is
// always an ancestor of abs by construction; if that ever fails to
// hold, the absolute path is the safe subject to match against.
func relativeTo(root, abs string) string {
	rel, err := filepath.Rel(root, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(abs)
	}
	return filepath.ToSlash(rel)
}
