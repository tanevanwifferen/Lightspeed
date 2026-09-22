package index

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// A GlobSet is a list of include and exclude patterns over
// workspace-relative paths, with the semantics of `search_text --glob`: a
// pattern starting with `!` excludes, any other includes, and a file must match
// some include when there are any and no exclude. A pattern with no `/` matches
// at any depth (`*.go` is `**/*.go`), and any pattern also matches everything
// below a directory of that name (`internal/cli` is `internal/cli/**`), as in
// ripgrep. Patterns are the router's globs (`**`, `{a,b}`, `[a-z]`).
type GlobSet struct {
	include, exclude []*router.Glob
	root             string
}

// CompileGlobs compiles patterns for paths relative to root. An empty or
// malformed pattern is an error.
func CompileGlobs(root string, patterns []string) (*GlobSet, error) {
	gs := &GlobSet{root: root}
	for _, p := range patterns {
		exclude := strings.HasPrefix(p, "!")
		p = strings.TrimPrefix(p, "!")
		if p == "" {
			return nil, fmt.Errorf("empty glob pattern")
		}
		forms := []string{p}
		if !strings.HasPrefix(p, "/") {
			p = strings.TrimPrefix(p, "./")
			if !strings.Contains(p, "/") {
				p = "**/" + p
			}
			forms = []string{p}
		}
		if !strings.HasSuffix(forms[0], "**") {
			forms = append(forms, strings.TrimSuffix(forms[0], "/")+"/**")
		}
		for _, f := range forms {
			g, err := router.CompileGlob(f)
			if err != nil {
				return nil, fmt.Errorf("glob: %v", err)
			}
			if exclude {
				gs.exclude = append(gs.exclude, g)
			} else {
				gs.include = append(gs.include, g)
			}
		}
	}
	return gs, nil
}

func (gs *GlobSet) matches(globs []*router.Glob, rel string) bool {
	for _, g := range globs {
		p := rel
		if g.Absolute() {
			p = filepath.ToSlash(filepath.Join(gs.root, filepath.FromSlash(rel)))
		}
		if g.Match(p) {
			return true
		}
	}
	return false
}

// Allows reports whether a workspace-relative path passes the globs. A nil
// set allows everything.
func (gs *GlobSet) Allows(rel string) bool {
	if gs == nil {
		return true
	}
	if len(gs.include) > 0 && !gs.matches(gs.include, rel) {
		return false
	}
	return !gs.matches(gs.exclude, rel)
}
