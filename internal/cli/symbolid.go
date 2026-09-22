package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
)

// Stable symbol ids (docs/DECISIONS.md D21).
//
// An agent that lists a file's symbols and then wants one symbol's source
// needs a name for the symbol that survives between the two calls and does not
// depend on a byte column it would have to guess. The id is
//
//	<workspace-relative path>::<Container.Name>#<kind>[~N]
//
// for example `internal/cli/cli.go::Main#function` or
// `internal/edit/stage.go::Stage.Apply#method`. It is built from what the
// language server says about the file (textDocument/documentSymbol, in either
// of its two shapes), so it is as good as the server's symbol table and needs
// nothing of ours to be kept in step with the file: an id is recomputed from
// the current file every time it is used, and one that no longer names a
// symbol is an error that says so, never a guess (the spirit of D11).

// idSeparator separates the path from the symbol, and idKindMark the symbol
// from its kind. The path is split at the *first* separator and the kind at
// the *last* mark, because a qualified name can contain either (`ns::Foo` in
// C++) and a path cannot contain the first.
const (
	idSeparator = "::"
	idKindMark  = "#"
)

// maxIDFiles bounds how many distinct files an id lookup for a symbol list
// (workspace_symbol, call_hierarchy) will ask a server for the outline of.
// The lookup exists because a `~N` suffix depends on every duplicate in a
// file, which the list does not carry; each file costs a request.
const maxIDFiles = 30

// canonPath makes a path absolute and resolves its symlinks, as far as it
// exists — the deepest existing ancestor is resolved and the rest appended — so
// that it can be compared with a workspace root (which is canonical:
// daemon.Workspace) even when the file itself is not there.
func canonPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	rest := ""
	for dir := abs; ; {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(real, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

// relToRoot is a file's path relative to the workspace root, with forward
// slashes, and whether it is inside the root at all.
func relToRoot(root, file string) (string, bool) {
	rel, err := filepath.Rel(root, canonPath(file))
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", false
	}
	return filepath.ToSlash(rel), true
}

// The id rules are internal/symbols'; these are their names inside this package.
var (
	idName        = symbols.IDName
	idKind        = symbols.IDKind
	baseID        = symbols.BaseID
	symbolIDs     = symbols.IDs
	posBefore     = symbols.PosBefore
	rangeContains = symbols.RangeContains
	pinpointNames = symbols.PinpointNames
	lastSegment   = symbols.LastSegment
)

// decodeFileSymbols decodes a documentSymbol answer for doc and pinpoints the
// names of a flat one.
func decodeFileSymbols(raw json.RawMessage, doc *docstore.Document) ([]symbol, []string, error) {
	syms, err := decodeDocumentSymbols(raw, doc.URI)
	if err != nil {
		return nil, nil, err
	}
	return syms, pinpointNames(doc.Mapper, syms), nil
}

// An idParts is a symbol id taken apart.
type idParts struct {
	Path string
	Name string
	Kind string
	// N is 1 for the first symbol of an id and 2 or more for the `~N`
	// duplicates.
	N int
}

// String is the id, in the form symbolIDs writes it.
func (p idParts) String() string {
	id := p.Path + idSeparator + p.Name + idKindMark + p.Kind
	if p.N > 1 {
		id += "~" + strconv.Itoa(p.N)
	}
	return id
}

// isSymbolID reports whether an argument is a symbol id rather than a
// location: an id has a `::` and a `#kind` after it, and a location, which has
// neither, never does.
func isSymbolID(arg string) bool {
	i := strings.Index(arg, idSeparator)
	return i > 0 && strings.Contains(arg[i+len(idSeparator):], idKindMark)
}

var dupSuffix = regexp.MustCompile(`~(\d+)$`)

// parseSymbolID takes an id apart, and refuses one that is not well formed or
// whose path could leave the workspace. It reads nothing.
func parseSymbolID(id string) (idParts, error) {
	bad := func(why string) error {
		return render.Errorf(render.CodeUsage,
			"%q is not a symbol id (%s); ids look like internal/cli/cli.go::Main#function and come from outline, symbols and workspace_symbol", id, why)
	}
	i := strings.Index(id, idSeparator)
	if i <= 0 {
		return idParts{}, bad("no path before " + idSeparator)
	}
	filePart, rest := id[:i], id[i+len(idSeparator):]
	k := strings.LastIndex(rest, idKindMark)
	if k <= 0 || k == len(rest)-1 {
		return idParts{}, bad("no name and #kind after " + idSeparator)
	}
	parts := idParts{Name: rest[:k], Kind: rest[k+1:], N: 1}
	if m := dupSuffix.FindStringSubmatch(parts.Kind); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n < 2 {
			return idParts{}, bad("a ~N suffix starts at ~2; the first symbol has none")
		}
		parts.N = n
		parts.Kind = strings.TrimSuffix(parts.Kind, m[0])
	}
	if parts.Kind == "" {
		return idParts{}, bad("empty kind")
	}
	// The path is relative to the workspace and cannot climb out of it. An
	// absolute path or a `..` is refused here, before anything is read.
	if strings.HasPrefix(filePart, "/") || filepath.IsAbs(filePart) || strings.Contains(filePart, `\`) {
		return idParts{}, render.Errorf(render.CodeOutsideWorkspace,
			"symbol id %q names an absolute or non-portable path; an id's path is relative to the workspace and uses /", id)
	}
	clean := path.Clean(filePart)
	if clean == ".." || strings.HasPrefix(clean, "../") || clean == "." {
		return idParts{}, render.Errorf(render.CodeOutsideWorkspace,
			"symbol id %q names a path outside the workspace", id)
	}
	parts.Path = clean
	return parts, nil
}

// idFile is the absolute path an id's file is at, refusing one that is not
// inside the workspace after symlinks are followed and reporting one that is
// not there as a stale id.
func idFile(root string, parts idParts, id string, candidates func() []idCandidate) (string, error) {
	abs := filepath.Join(root, filepath.FromSlash(parts.Path))
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", staleIDError(id, "the file "+parts.Path+" does not exist", candidates())
	}
	if _, in := relToRoot(root, real); !in {
		return "", render.Errorf(render.CodeOutsideWorkspace,
			"symbol id %q resolves outside the workspace (%s is a symlink out of it)", id, parts.Path)
	}
	if err := mustBeFile(real); err != nil {
		return "", staleIDError(id, "the path "+parts.Path+" is not a file", candidates())
	}
	return real, nil
}

// An idCandidate is a nearest match named by a stale-id error.
type idCandidate struct {
	ID   string `json:"id,omitempty"`
	File string `json:"file,omitempty"`
	Kind string `json:"kind,omitempty"`
	Line int    `json:"line,omitempty"`
}

// candidateLimit bounds the candidates a stale-id error names.
const candidateLimit = 5

// staleIDError refuses an id that no longer names anything. The message and
// error.data name the nearest candidates, ranked by how close they are and
// otherwise in document order, so the retry is a copy of one of them; none is
// used.
func staleIDError(id, why string, candidates []idCandidate) error {
	msg := fmt.Sprintf("symbol id %q no longer resolves: %s", id, why)
	if len(candidates) > 0 {
		names := make([]string, 0, len(candidates))
		for _, c := range candidates {
			if c.ID != "" {
				names = append(names, c.ID)
			} else {
				names = append(names, c.File)
			}
		}
		msg += "; nearest: " + strings.Join(names, ", ")
	} else {
		msg += "; nothing similar was found — list the file again with outline"
	}
	if candidates == nil {
		candidates = []idCandidate{}
	}
	return render.Errorf(render.CodeStaleID, "%s", msg).
		WithDetails(map[string]any{"id": id, "reason": why, "candidates": candidates})
}

// nearestSymbols ranks a file's symbols by how close they are to a missing id:
// the same name of another kind or duplicate first, then the same last
// segment, then names that contain one another or differ by a few edits.
func nearestSymbols(want idParts, doc *docstore.Document, syms []symbol, ids []string) []idCandidate {
	type scored struct {
		i     int
		score int
	}
	wantName := strings.ToLower(want.Name)
	wantLast := lastSegment(wantName)
	var ranked []scored
	for i, sym := range syms {
		name := strings.ToLower(idName(sym.Qualified))
		last := lastSegment(name)
		score := -1
		switch d, dLast := editDistance(name, wantName), editDistance(last, wantLast); {
		case name == wantName:
			score = 0
		case last == wantLast:
			score = 1
		case strings.Contains(last, wantLast) || strings.Contains(wantLast, last):
			score = 2
		case d <= 3:
			// A near miss of the whole name ranks by how near.
			score = 3 + d
		case dLast <= 2:
			score = 10 + dLast
		}
		if score >= 0 {
			ranked = append(ranked, scored{i, score})
		}
	}
	sort.SliceStable(ranked, func(a, b int) bool { return ranked[a].score < ranked[b].score })
	var out []idCandidate
	for _, r := range ranked {
		if len(out) == candidateLimit {
			break
		}
		c := idCandidate{ID: ids[r.i], Kind: idKind(syms[r.i].Kind)}
		if span, err := render.NewSpan(doc.Mapper, syms[r.i].Range); err == nil {
			c.Line = span.Start.Line
		}
		out = append(out, c)
	}
	return out
}

// editDistance is the Levenshtein distance between two strings, in runes.
func editDistance(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	prev := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		cur := make([]int, len(rb)+1)
		cur[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(rb)]
}

// A fileSymbols is one open file and what the server says is in it.
type fileSymbols struct {
	// session is the session the file was opened and outlined in.
	session *session
	doc     *docstore.Document
	// rel is the file's path relative to the workspace root, "" when it is
	// outside it — in which case there are no ids.
	rel  string
	syms []symbol
	// ids is parallel to syms; nil when the file is outside the workspace.
	ids      []string
	warnings []string
}

// documentSymbols asks the session's server for the symbols of an open
// document.
func (s *session) documentSymbols(doc *docstore.Document) ([]symbol, []string, error) {
	ctx, cancel := s.requestContext()
	defer cancel()
	res, err := s.query(ctx, methodDocumentSymbol, map[string]any{
		"textDocument": map[string]any{"uri": string(doc.URI)},
	})
	if err != nil {
		return nil, nil, err
	}
	syms, warnings, err := decodeFileSymbols(res.Result, doc)
	if err != nil {
		return nil, nil, err
	}
	return syms, append(slices.Clone(res.Warnings), warnings...), nil
}

// loadFile opens a file, asks for its symbols and computes their ids against
// the workspace root.
func (s *session) loadFile(root, file string) (*fileSymbols, error) {
	doc, err := s.open(file)
	if err != nil {
		return nil, err
	}
	syms, warnings, err := s.documentSymbols(doc)
	if err != nil {
		return nil, err
	}
	ff := &fileSymbols{session: s, doc: doc, syms: syms, warnings: warnings}
	if rel, in := relToRoot(root, file); in {
		ff.rel = rel
		ff.ids = symbolIDs(rel, syms)
	} else {
		ff.warnings = append(ff.warnings, fmt.Sprintf(
			"%s is outside the workspace %s, so its symbols have no ids", file, root))
	}
	return ff, nil
}

// find returns the index of the symbol an id names in this file, or a stale-id
// error naming the nearest candidates.
func (ff *fileSymbols) find(parts idParts, id string) (int, error) {
	want := parts.String()
	if i := slices.Index(ff.ids, want); i >= 0 {
		return i, nil
	}
	why := "the file has no symbol " + parts.Name + " of kind " + parts.Kind
	if parts.N > 1 {
		why = fmt.Sprintf("the file has fewer than %d symbols named %s of kind %s", parts.N, parts.Name, parts.Kind)
	}
	return -1, staleIDError(id, why, nearestSymbols(parts, ff.doc, ff.syms, ff.ids))
}

// sameNameFiles are the files of the workspace that share a stale id's file
// name: where the file most likely went. It is computed only when the file is
// missing.
func sameNameFiles(ctx context.Context, root string, parts idParts) func() []idCandidate {
	return func() []idCandidate {
		listing, err := listWorkspaceFiles(ctx, root, root)
		if err != nil {
			return nil
		}
		base := path.Base(parts.Path)
		var same []string
		for _, f := range listing.Files {
			if path.Base(f) == base {
				same = append(same, f)
			}
		}
		// The closest path first, then alphabetically: where the file most
		// likely moved to.
		sort.SliceStable(same, func(a, b int) bool {
			return editDistance(same[a], parts.Path) < editDistance(same[b], parts.Path)
		})
		var out []idCandidate
		for _, f := range same {
			if len(out) < candidateLimit {
				out = append(out, idCandidate{File: f})
			}
		}
		return out
	}
}

// resolveIDLocation turns --id into the `file:line:col` the rest of the
// package speaks, exactly as resolveSymbol does for --symbol: in a session of
// its own, because the id names the file to route by.
func resolveIDLocation(e *env, common *commonFlags, sf *symbolFlags) (string, []string, error) {
	root, err := commandWorkspace(sf.path)
	if err != nil {
		return "", nil, err
	}
	parts, err := parseSymbolID(sf.id)
	if err != nil {
		return "", nil, err
	}
	file, err := idFile(root, parts, sf.id, sameNameFiles(e.base(), root, parts))
	if err != nil {
		return "", nil, err
	}
	match, err := e.resolveTarget(file, "", common.server)
	if err != nil {
		return "", nil, err
	}
	connectCtx, cancelConnect := context.WithTimeout(e.base(), common.timeout)
	defer cancelConnect()
	s, err := startSession(connectCtx, e, match, common.gateOptions())
	if err != nil {
		return "", nil, err
	}
	defer s.close()

	ff, err := s.loadFile(root, file)
	if err != nil {
		return "", nil, err
	}
	i, err := ff.find(parts, sf.id)
	if err != nil {
		return "", nil, err
	}
	where, err := symbolLocation(s, ff.syms[i])
	if err != nil {
		return "", nil, err
	}
	return where, append(ff.warnings, fmt.Sprintf("--id %q resolved to %s", sf.id, where)), nil
}

// An idIndex computes ids for symbols that arrive without their file's
// outline — a workspace_symbol answer, a call hierarchy — by asking the
// session's server for the outline of each distinct file, at most maxIDFiles
// of them, and matching each symbol to the outline entry at its position.
//
// A result it cannot name is left without an id, and the summary says how
// many: an id that might point at a duplicate other than the one meant would
// be the guess D21 rules out.
type idIndex struct {
	e      *env
	s      *session
	root   string
	files  map[protocol.DocumentURI]*fileSymbols
	failed map[protocol.DocumentURI]bool

	outside, other, unmatched, capped int
	warnings                          []string
}

func newIDIndex(e *env, s *session, root string) *idIndex {
	return &idIndex{e: e, s: s, root: root,
		files: map[protocol.DocumentURI]*fileSymbols{}, failed: map[protocol.DocumentURI]bool{}}
}

// idAt is the id of the symbol whose name is at sel in uri, or "".
func (x *idIndex) idAt(uri protocol.DocumentURI, sel protocol.Range) string {
	file := uri.Path()
	if _, in := relToRoot(x.root, file); !in {
		x.outside++
		return ""
	}
	ff, ok := x.files[uri]
	if !ok {
		switch {
		case x.failed[uri]:
			return ""
		case len(x.files) >= maxIDFiles:
			x.capped++
			return ""
		}
		// Another language's server cannot outline this file; only the
		// session's own can be asked.
		if m, err := x.e.resolveTarget(file, "", ""); err != nil || m.Server.Name != x.s.match.Server.Name || m.Root != x.s.match.Root {
			x.other++
			x.failed[uri] = true
			return ""
		}
		loaded, err := x.s.loadFile(x.root, file)
		if err != nil {
			x.warnings = append(x.warnings, fmt.Sprintf("no ids for %s: %v", file, err))
			x.failed[uri] = true
			return ""
		}
		x.files[uri], ff = loaded, loaded
	}
	for i, sym := range ff.syms {
		if sym.Range.Start == sel.Start && ff.ids != nil {
			return ff.ids[i]
		}
	}
	x.unmatched++
	return ""
}

// summary is the warning that says how many results have no id, and why.
func (x *idIndex) summary() []string {
	out := slices.Clone(x.warnings)
	// A result outside the workspace has no id by definition (an id's path
	// is relative to the workspace), which is not worth a warning on every
	// call that reaches into a standard library.
	var parts []string
	if x.other > 0 {
		parts = append(parts, fmt.Sprintf("%d in files another server handles", x.other))
	}
	if x.capped > 0 {
		parts = append(parts, fmt.Sprintf("%d in files beyond the %d whose outline is asked for", x.capped, maxIDFiles))
	}
	if x.unmatched > 0 {
		parts = append(parts, fmt.Sprintf("%d that the server's outline does not list", x.unmatched))
	}
	if len(parts) > 0 {
		out = append(out, "some results have no id: "+strings.Join(parts, ", ")+"; `outline` of the file gives its ids")
	}
	return out
}
