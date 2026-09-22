package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	goplscmd "github.com/tanevanwifferen/Lightspeed/internal/gopls/cmd"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// The composed verdict commands (docs/DECISIONS.md D38): type_hierarchy,
// check_references, rename_check and delete_check. Each answers a question with a
// verdict *and the evidence behind it*, never a bare yes: every row says which
// query it came from (the `evidence` field), and every verdict carries the rule
// that produced it and the counts it rests on.

// The evidence a row can come from.
const (
	evidenceTypeHierarchy  = "type_hierarchy"
	evidenceImplementation = "implementation"
	evidenceReferences     = "references"
	evidenceSearchText     = "search_text"
	evidenceIndex          = "index"
	evidenceImporters      = "importers"
	evidencePrepareRename  = "prepare_rename"
	evidenceRename         = "rename"
	evidenceDefinition     = "definition"
)

// verdictCommands is the table of the four commands.
func verdictCommands() []*command {
	return []*command{
		{
			Name:    "type_hierarchy",
			Args:    "<loc>",
			Summary: "print the supertypes and subtypes of the type at a location",
			// Deliberately unguarded, like `check`: a server without
			// typeHierarchyProvider still answers the subtypes half through
			// `implementation`, and the command says which one it used.
			Run: typeHierarchyCommand,
			MCP: oneTool("type_hierarchy", "Supertypes and subtypes of the type at a location; each row names its evidence (type_hierarchy, or implementation as fallback). Bounded by depth, limit.",
				locationParams(pointDesc), []param{
					{Flag: "direction", Type: typeString, Enum: []string{directionSupertypes, directionSubtypes, directionBoth},
						Desc: "supertypes, subtypes or both (default)."},
					{Flag: "depth", Type: typeInt, Min: intp(1), Max: intp(maxTypeDepth),
						Desc: "Levels to expand (default 1: direct supertypes and subtypes)."},
				}, commonParams("limit", "timeout", "server", "absolute")),
		},
		{
			Name:    "check_references",
			Args:    "[<name|loc>]",
			Summary: "is this used anywhere: semantic references plus text mentions outside them, kept apart",
			Run:     checkReferencesCommand,
			MCP: oneTool("check_references", "Is this used anywhere? Semantic references plus text hits outside them, apart; verdict ambiguous (with counts per candidate) if the name matches several.",
				[]param{
					{Name: "name", Type: typeString,
						Desc: "A bare name or dotted symbol path (Type.Method), a location file:line:col, or a symbol id. Give this, symbol or id."},
					{Flag: "symbol", Type: typeString,
						Desc: "Dotted symbol path such as pkg.Type.Method. Alternative to name; an ambiguous name gives verdict ambiguous, per-candidate reference counts and text hits."},
					{Flag: "id", Type: typeString,
						Desc: "Symbol id (path::Container.Name#kind). Alternative to name."},
					{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true,
						Desc: "Directory whose workspace is searched and that id paths are relative to (default: workspace)."},
					{Flag: "glob", Type: typeStrings,
						Desc: "Only search these files for text hits; a leading ! excludes. No / in a glob matches at any depth."},
				}, commonParams("limit", "timeout", "server", "absolute")),
		},
		{
			Name:    "rename_check",
			Args:    "<loc> <newname>",
			Summary: "would renaming the symbol at a location work: prepareRename, the edit preview's size, and name collisions; writes nothing",
			Run:     renameCheckCommand,
			MCP: oneTool("rename_check", "Read-only rename verdict: server acceptance, files and edits touched, name collisions from the index. Never writes; use rename to apply.",
				locationParams(pointDesc),
				[]param{{Name: "new_name", Type: typeString, Required: true, Desc: "The name it would be renamed to."}},
				commonParams("limit", "timeout", "server", "absolute")),
		},
		{
			Name:    "delete_check",
			Args:    "<loc>",
			Summary: "would deleting the symbol at a location break anything: references outside its declaration, exportedness, importers if it is the last symbol",
			Method:  methodReferences,
			Run:     deleteCheckCommand,
			MCP: oneTool("delete_check", "Read-only delete verdict: references outside the declaration, exportedness, and importers if it is the last symbol of its file or package. Never writes.",
				locationParams(pointDesc), commonParams("limit", "timeout", "server", "absolute")),
		},
	}
}

// --- rows ---

// A refRow is one place an identifier occurs, and the evidence it comes from.
type refRow struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column,omitempty"`
	// Text is the matched line, trimmed and clipped.
	Text string `json:"text,omitempty"`
	// In is the id of the symbol that encloses the row, when the index could say.
	In string `json:"in,omitempty"`
	// Test marks a file that looks like a test (isTestPath).
	Test     bool   `json:"test,omitempty"`
	Evidence string `json:"evidence"`
}

// clipRowText is a line as a row prints it: trimmed, and clipped on a rune boundary.
func clipRowText(s string) string {
	s = strings.TrimSpace(s)
	const max = 160
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// rowFor renders a language-server location as a reference row: the file
// relative to the workspace, the line and byte column, and the matched line.
func (c *composed) rowFor(loc protocol.Location, evidence string) (refRow, bool) {
	rel, in := relToRoot(c.Root, loc.URI.Path())
	if !in {
		return refRow{}, false
	}
	m, err := c.Q.session.docs.MapperForURI(loc.URI)
	if err != nil {
		return refRow{}, false
	}
	span, err := render.NewSpanFromLocation(m, loc)
	if err != nil {
		return refRow{}, false
	}
	return refRow{File: rel, Line: span.Start.Line, Column: span.Start.Column,
		Text: clipRowText(span.Text), Test: isTestPath(rel), Evidence: evidence}, true
}

// sortRefRows orders rows by file and position, so that repeated runs print the
// same bytes.
func sortRefRows(rows []refRow) {
	slices.SortStableFunc(rows, func(a, b refRow) int {
		if a.File != b.File {
			return strings.Compare(a.File, b.File)
		}
		if a.Line != b.Line {
			return a.Line - b.Line
		}
		return a.Column - b.Column
	})
}

// maxEnclosingFiles bounds how many files' outlines are asked of the index to
// name the symbols enclosing a list of rows.
const maxEnclosingFiles = 200

// attachEnclosing sets the In field of each row to the id of its innermost
// enclosing symbol, from the workspace index (revalidated against the disk like
// every index answer). It is best effort and says so: a failure leaves the rows
// without ids and the warning names why.
func attachEnclosing(run *indexRun, rows []refRow) []string {
	var files []string
	for _, r := range rows {
		if !slices.Contains(files, r.File) {
			files = append(files, r.File)
		}
	}
	if len(files) == 0 {
		return nil
	}
	var warnings []string
	if len(files) > maxEnclosingFiles {
		warnings = append(warnings, fmt.Sprintf("enclosing symbol ids are given for the first %d of %d files", maxEnclosingFiles, len(files)))
		files = files[:maxEnclosingFiles]
	}
	out, err := run.Symbols(index.SymbolsQuery{Files: files})
	if err != nil {
		return append(warnings, "no enclosing symbol ids: "+err.Error())
	}
	bySymbols := map[string][]index.Symbol{}
	for _, f := range out.Files {
		bySymbols[f.File] = f.Symbols
	}
	for i := range rows {
		if s := enclosingIndexSymbol(bySymbols[rows[i].File], rows[i].Line); s != nil {
			rows[i].In = s.ID
		}
	}
	return append(warnings, syncWarnings(out.Report)...)
}

// --- the identifier at a position, and the declaration it belongs to ---

// identAt is the identifier (letters, digits and _ of any script) the position
// touches, "" if it is on none.
func identAt(doc *docstore.Document, pos protocol.Position) string {
	off, err := doc.Mapper.PositionOffset(pos)
	if err != nil {
		return ""
	}
	content := doc.Mapper.Content
	start, end := off, off
	for start > 0 {
		r, size := utf8.DecodeLastRune(content[:start])
		if !isWordRune(r) {
			break
		}
		start -= size
	}
	for end < len(content) {
		r, size := utf8.DecodeRune(content[end:])
		if !isWordRune(r) {
			break
		}
		end += size
	}
	return string(content[start:end])
}

// identAtColumn is the identifier that starts at (or contains) the 1-based byte
// column of a line.
func identAtColumn(line string, col int) string {
	i := col - 1
	if i < 0 || i > len(line) {
		return ""
	}
	start, end := i, i
	for start > 0 {
		r, size := utf8.DecodeLastRuneInString(line[:start])
		if !isWordRune(r) {
			break
		}
		start -= size
	}
	for end < len(line) {
		r, size := utf8.DecodeRuneInString(line[end:])
		if !isWordRune(r) {
			break
		}
		end += size
	}
	return line[start:end]
}

// A declaration is where the symbol a command is about is declared, which is
// not always where the caller pointed: a location on a use of `Foo` asks about
// Foo, whose declaration is elsewhere.
type symbolDecl struct {
	// Word is the identifier at the position.
	Word string
	// OnDeclaration is true when the position was on the declaration's name.
	OnDeclaration bool
	File          string
	Line, EndLine int
	// Symbol is the identified symbol; ID, Name, Kind and Container are its
	// stable id, qualified name, kind and enclosing symbol's qualified name.
	ID, Name, Kind, Container string
	// Language is the router's id for the declaring file.
	Language string
	// Exported is "exported", "unexported" or "unknown", and ExportBasis says how
	// that was decided.
	Exported, ExportBasis string
}

// declaration resolves the declaration of the subject. When the position is on
// a declaration's name it is the subject the outline identified; otherwise the
// server's `definition` answer is followed, and the declaring file's symbols are
// read from the index to name it. run may be nil, in which case a definition is
// located but not named.
func (c *composed) declaration(run *indexRun) (symbolDecl, []string) {
	d := symbolDecl{Word: identAt(c.Q.doc, c.Q.position)}
	var warnings []string
	sub := c.Subject
	if sub.ID != "" && d.Word != "" && lastNameSegment(sub.Name) == d.Word && sub.Line == int(c.Q.position.Line)+1 {
		d.OnDeclaration = true
		d.File, d.Line, d.EndLine = sub.File, sub.Line, sub.EndLine
		d.ID, d.Name, d.Kind = sub.ID, sub.Name, sub.Kind
		d.Container = strings.TrimSuffix(strings.TrimSuffix(sub.Name, d.Word), ".")
	} else {
		d.File, d.Line = sub.File, sub.Line
		if c.Q.session.lsp.Supports(methodDefinition) {
			ctx, cancel := c.Q.queryContext()
			res, err := c.Q.session.query(ctx, methodDefinition, textDocumentPosition(c.Q.doc.URI, c.Q.position))
			cancel()
			if err == nil {
				warnings = append(warnings, res.Warnings...)
				locs, derr := decodeLocations(res.Result)
				for _, loc := range locs {
					if l, ok := c.Locus(loc); ok {
						d.File, d.Line = l.File, l.Line
						break
					}
				}
				if derr != nil {
					warnings = append(warnings, "definition: "+derr.Error())
				}
			} else {
				warnings = append(warnings, "could not follow the position to its declaration: "+err.Error())
			}
		}
		if run != nil && d.Word != "" {
			out, err := run.Symbols(index.SymbolsQuery{Files: []string{d.File}})
			if err == nil {
				for _, f := range out.Files {
					if s := innermostNamed(f.Symbols, d.Word, d.Line); s != nil {
						d.Line, d.EndLine = s.Line, s.EndLine
						d.ID, d.Name, d.Kind, d.Container = s.ID, s.Qualified, s.Kind, s.Container
					}
				}
			} else {
				warnings = append(warnings, "could not read the declaring file's symbols: "+err.Error())
			}
		}
	}
	d.Language = router.LanguageID(d.File)
	if d.Word != "" {
		d.Exported, d.ExportBasis = c.exportedness(d)
	} else {
		d.Exported, d.ExportBasis = "unknown", "no identifier at the position"
	}
	return d, warnings
}

// innermostNamed is the smallest symbol called name whose lines contain line.
func innermostNamed(syms []index.Symbol, name string, line int) *index.Symbol {
	var best *index.Symbol
	for i := range syms {
		s := &syms[i]
		if s.Name != name || line < s.Line || line > s.EndLine {
			continue
		}
		if best == nil || s.EndLine-s.Line <= best.EndLine-best.Line {
			best = s
		}
	}
	return best
}

// exportedness reads the declaring file for the language's own visibility
// marker. It never guesses: a language it has no rule for, or a declaration it
// cannot read, is "unknown" and says why.
func (c *composed) exportedness(d symbolDecl) (state, basis string) {
	abs := filepath.Join(c.Root, filepath.FromSlash(d.File))
	content, err := os.ReadFile(abs)
	if err != nil {
		return "unknown", "the declaring file could not be read"
	}
	lines := strings.Split(string(content), "\n")
	decl := ""
	if d.Line >= 1 && d.Line <= len(lines) {
		decl = strings.TrimSpace(lines[d.Line-1])
	}
	return languageExportState(d.Language, d.Word, d.Container != "", decl, string(content), d.File)
}

// languageExportState is the per-language rule of exportedness. decl is the trimmed
// line of the declaration, file the whole file, rel its path.
func languageExportState(lang, name string, nested bool, decl, file, rel string) (string, string) {
	first, _ := utf8.DecodeRuneInString(name)
	switch lang {
	case "go":
		if unicode.IsUpper(first) {
			return "exported", "Go: an identifier is exported when it starts with an upper-case letter"
		}
		return "unexported", "Go: an identifier is unexported when it does not start with an upper-case letter"
	case "python":
		if strings.HasPrefix(name, "_") && !(strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__")) {
			return "unexported", "Python: a leading underscore is the private convention"
		}
		return "exported", "Python: no leading underscore, so public by convention (__all__ is not read)"
	case "javascript", "typescript", "javascriptreact", "typescriptreact", "tsx", "jsx":
		if strings.HasPrefix(decl, "export ") || strings.HasPrefix(decl, "export{") {
			return "exported", "JS/TS: the declaration is written with `export`"
		}
		if nested {
			return "unknown", "JS/TS: the visibility of a class member is not read (public, private and # names are not distinguished)"
		}
		quoted := regexp.QuoteMeta(name)
		if regexp.MustCompile(`export\s*\{[^}]*\b`+quoted+`\b`).MatchString(file) ||
			regexp.MustCompile(`export\s+default\s+`+quoted+`\b`).MatchString(file) ||
			regexp.MustCompile(`(module\.)?exports\.`+quoted+`\b`).MatchString(file) ||
			regexp.MustCompile(`module\.exports\s*=\s*\{[^}]*\b`+quoted+`\b`).MatchString(file) {
			return "exported", "JS/TS: the name appears in an `export {…}`, `export default` or `module.exports` of its file"
		}
		return "unexported", "JS/TS: no `export`, `export {…}`, `export default` or `module.exports` names it in its file"
	case "rust":
		switch {
		case strings.HasPrefix(decl, "pub("):
			return "unknown", "Rust: `pub(…)` is restricted visibility; whether it reaches outside the crate is not read"
		case strings.HasPrefix(decl, "pub "):
			return "exported", "Rust: the declaration is written `pub`"
		}
		return "unexported", "Rust: no `pub` on the declaration"
	case "java", "csharp":
		switch {
		case declHasModifier(decl, "public"):
			return "exported", lang + ": the declaration is written `public`"
		case declHasModifier(decl, "private"):
			return "unexported", lang + ": the declaration is written `private`"
		}
		return "unknown", lang + ": no public/private modifier on the declaration line (package/internal/protected visibility is not decided)"
	case "kotlin":
		switch {
		case declHasModifier(decl, "private"):
			return "unexported", "Kotlin: the declaration is written `private`"
		case declHasModifier(decl, "internal"):
			return "unknown", "Kotlin: `internal` is visible to the whole module, which is not read"
		}
		return "exported", "Kotlin: public is the default"
	case "lua":
		if strings.HasPrefix(decl, "local ") {
			return "unexported", "Lua: the declaration is written `local`"
		}
		return "exported", "Lua: a declaration without `local` is global or a module field"
	case "c", "cpp":
		if declHasModifier(decl, "static") {
			return "unexported", lang + ": the declaration is `static` (internal linkage)"
		}
		if strings.HasSuffix(rel, ".h") || strings.HasSuffix(rel, ".hpp") || strings.HasSuffix(rel, ".hh") {
			return "exported", lang + ": declared in a header, without `static`"
		}
		return "unknown", lang + ": external linkage in a source file; whether a header exposes it is not read"
	}
	return "unknown", "no visibility rule for language " + fmt.Sprintf("%q", lang)
}

// declHasModifier reports whether a declaration line carries a keyword as a word.
func declHasModifier(decl, kw string) bool {
	for _, f := range strings.FieldsFunc(decl, func(r rune) bool { return !isWordRune(r) }) {
		if f == kw {
			return true
		}
	}
	return false
}

// --- text hits for an identifier ---

// A textHit is a whole-word occurrence of an identifier.
type identScan struct {
	Matches       []searchMatch
	Total         int
	FilesSearched int
	Capped        bool
	Warnings      []string
}

// perFileKeep is how many matching lines of one file a text scan keeps; every
// line is counted, so the total stays exact.
const perFileKeep = 200

// scanIdentifier searches the workspace, live and without an index, for the
// identifier as a whole word, case-sensitively — the same engine as
// `search_text`, and the same rules for what is skipped and why.
func scanIdentifier(ctx context.Context, root, word string, globs []string) (*identScan, error) {
	m, err := newMatcher(word, false, true, true)
	if err != nil {
		return nil, err
	}
	gs, err := compileGlobs(root, globs)
	if err != nil {
		return nil, err
	}
	listing, err := searchFileSet(ctx, root, root)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range listing.Files {
		if gs.allows(f) {
			files = append(files, f)
		}
	}
	scans := make([]fileScan, len(files))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range min(runtime.GOMAXPROCS(0), max(len(files), 1)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= len(files) {
					return
				}
				scans[i] = scanFile(root, files[i], m, perFileKeep, 0, defaultMaxFileBytes, false)
			}
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, render.Errorf(render.CodeTimeout, "the text scan did not finish within --timeout; narrow it with --glob")
		}
		return nil, render.Errorf(render.CodeCancelled, "the text scan was cancelled")
	}
	out := &identScan{Warnings: slices.Clone(listing.Warnings)}
	skipped := 0
	for _, s := range scans {
		if s.skipped > 0 {
			skipped++
			continue
		}
		out.FilesSearched++
		out.Total += s.total
		if s.total > len(s.matches) {
			out.Capped = true
		}
		out.Matches = append(out.Matches, s.matches...)
	}
	if skipped > 0 {
		out.Warnings = append(out.Warnings, fmt.Sprintf(
			"%d file(s) were not searched for text (binary, over %d bytes, unreadable or outside the workspace); `search_text` names them", skipped, defaultMaxFileBytes))
	}
	return out, nil
}

// --- shared output helpers ---

// errCodedAs reports whether err carries the machine code.
func errCodedAs(err error, code render.Code) (*render.CodedError, bool) {
	var ce *render.CodedError
	if errors.As(err, &ce) && ce.Code == code {
		return ce, true
	}
	return nil, false
}

// looksLikeLocation reports whether a target names a position in an existing
// file (file:line, file:line:col, file:line:col-line:col or file:#offset) and
// returns it in the absolute form the rest of the package speaks. A relative
// path is tried against the working directory and then against root, so that a
// call made for another workspace (an MCP `path`) still finds its file.
func looksLikeLocation(arg, root string) (string, bool) {
	try := func(s string) (string, bool) {
		loc := goplscmd.ParseLocation(s)
		if !loc.IsValid() || !(loc.Start.HasPosition() || loc.Start.HasOffset()) {
			return "", false
		}
		st, err := os.Stat(loc.URI.Path())
		if err != nil || st.IsDir() {
			return "", false
		}
		return s, true
	}
	if s, ok := try(arg); ok {
		return s, true
	}
	if !filepath.IsAbs(arg) && root != "" {
		if s, ok := try(filepath.Join(root, arg)); ok {
			return s, true
		}
	}
	return "", false
}

// writeRefRows prints rows as `file:line:col: text  [in id]  (evidence)`, the
// grep-compatible shape of every other listing.
func writeRefRows(w io.Writer, rows []refRow) {
	for _, r := range rows {
		line := fmt.Sprintf("%s:%d", r.File, r.Line)
		if r.Column > 0 {
			line += fmt.Sprintf(":%d", r.Column)
		}
		line += ": " + r.Text
		if r.In != "" {
			line += "  [in " + r.In + "]"
		}
		if r.Test {
			line += "  [test]"
		}
		fmt.Fprintln(w, line)
	}
}

// writeWarningLines prints warnings as `# …` lines, as the other text listings do.
func writeWarningLines(w io.Writer, warnings []string) {
	for _, msg := range warnings {
		fmt.Fprintf(w, "# %s\n", msg)
	}
}

// distinctRowFiles counts the distinct files of rows.
func distinctRowFiles(rows []refRow) int {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.File] = true
	}
	return len(seen)
}

// lastNameSegment is the last dotted segment of a qualified name: `Server.Handle` →
// `Handle`.
func lastNameSegment(name string) string {
	if i := strings.LastIndex(name, "."); i >= 0 {
		return name[i+1:]
	}
	return name
}

// scanContext bounds a live scan by --timeout, like search_text's.
func scanContext(e *env, common *commonFlags) (context.Context, context.CancelFunc) {
	return context.WithTimeout(e.base(), common.timeout)
}
