package index

import (
	"context"
	"strings"
)

// SymbolsQuery asks for the symbols of a set of files, unranked: the raw
// material the composed commands (blast_radius, dead_code, changed_symbols, …)
// need to map a line onto its enclosing symbol or to walk every declaration.
// Search ranks; this enumerates.
type SymbolsQuery struct {
	// Files, when non-empty, is an exact list of workspace-relative files;
	// otherwise Path, Globs and Languages scope the walk as they do for a
	// search.
	Files     []string `json:"files,omitempty"`
	Path      string   `json:"path,omitempty"`
	Globs     []string `json:"globs,omitempty"`
	Languages []string `json:"languages,omitempty"`
	// Kinds keeps only symbols of these kinds (case-insensitive).
	Kinds []string `json:"kinds,omitempty"`
}

// FileSymbols is one file's symbols as the index has them, revalidated against
// the disk by the query that returned them.
type FileSymbols struct {
	File     string   `json:"file"`
	Language string   `json:"language,omitempty"`
	Hash     string   `json:"hash,omitempty"`
	Symbols  []Symbol `json:"symbols,omitempty"`
}

// SymbolsOutcome is the answer to a Symbols query and how fresh it is.
type SymbolsOutcome struct {
	// Files holds every in-scope file that has an outline, sorted by path,
	// including files with no symbols: a file that was outlined and is empty is
	// told apart from one that was never outlined by being listed at all.
	Files  []FileSymbols `json:"files"`
	Report *SyncReport   `json:"report"`
}

// Symbols revalidates the files in scope, builds what is stale and lists their
// symbols. As for every query, nothing in the answer is older than the file it
// describes (D32).
func (m *Manager) Symbols(ctx context.Context, q SymbolsQuery) (*SymbolsOutcome, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sm, err := m.compileScope(Scope{Path: q.Path, Globs: q.Globs, Languages: q.Languages, Files: q.Files})
	if err != nil {
		return nil, err
	}
	_, rep, err := m.sync(ctx, NeedOutline, sm)
	if err != nil {
		return nil, err
	}
	kinds := map[string]bool{}
	for _, k := range q.Kinds {
		kinds[strings.ToLower(k)] = true
	}
	out := &SymbolsOutcome{Report: rep}
	for _, f := range m.fileList(sm, NeedOutline) {
		fs := FileSymbols{File: f.Path, Language: f.Language, Hash: f.Hash}
		for _, s := range f.Symbols {
			if len(kinds) > 0 && !kinds[strings.ToLower(s.Kind)] {
				continue
			}
			fs.Symbols = append(fs.Symbols, s)
		}
		out.Files = append(out.Files, fs)
	}
	return out, nil
}
