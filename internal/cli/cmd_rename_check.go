package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/index"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed rename_check <loc> <newname>` — would this rename work?
//
// A read-only verdict assembled from the same three sources `rename` uses and
// one it does not: the server's own answer (prepareRename, then a rename
// request whose edits are staged but never written), the workspace index (is
// there already a symbol of that name where the renamed one lives?), and a text
// scan (what mentions the old name and will not be touched). Nothing is written
// and no file is opened for writing (docs/DECISIONS.md D38).

// The verdicts of rename_check.
const (
	renameOK         = "ok"
	renameCollision  = "collision"
	renameRefused    = "refused"
	renameNoop       = "noop"
	renameUnverified = "unverified"
)

// defaultRenameLimit is how many rows of each list rename_check prints.
const defaultRenameLimit = 20

// A renameStep is what one query said.
type renameStep struct {
	// Evidence is the query: prepare_rename or rename.
	Evidence string `json:"evidence"`
	// Status is accepted, refused or not_supported.
	Status string `json:"status"`
	// Detail is the server's reason or the reason the query was not made.
	Detail      string `json:"detail,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
}

// A renameFileEdits is one file of the rename preview.
type renameFileEdits struct {
	File  string `json:"file"`
	Edits int    `json:"edits"`
	// Kind is modify, create, delete or rename.
	Kind string `json:"kind,omitempty"`
}

// renameEdits is the size of the rename preview.
type renameEdits struct {
	renameStep
	Files     int               `json:"files"`
	Edits     int               `json:"edits"`
	PerFile   []renameFileEdits `json:"per_file,omitempty"`
	Truncated bool              `json:"truncated"`
	Total     int               `json:"total"`
	Limit     int               `json:"limit,omitempty"`
}

// A collision is an existing symbol the new name would clash with.
type renameClash struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
	File string `json:"file"`
	Line int    `json:"line"`
	// Scope is where it is: same_file, or same_directory for the languages whose
	// unit of scope is the directory (Go, Java, Kotlin).
	Scope    string `json:"scope"`
	Evidence string `json:"evidence"`
}

// collisionSection is the index's answer about names.
type collisionSection struct {
	Evidence string `json:"evidence"`
	// Checked is false when the index could not be consulted or the declaration
	// could not be identified; Detail says why.
	Checked bool          `json:"checked"`
	Detail  string        `json:"detail,omitempty"`
	Count   int           `json:"count"`
	Rows    []renameClash `json:"rows"`
}

// renameCheckData is the payload of `rename_check`.
type renameCheckData struct {
	Root    string  `json:"root"`
	Symbol  subject `json:"symbol"`
	OldName string  `json:"old_name"`
	NewName string  `json:"new_name"`
	// Verdict is ok, collision, refused, noop or unverified, and Rule is the
	// reasoning, with the counts it rests on.
	Verdict    string           `json:"verdict"`
	Rule       string           `json:"rule"`
	Prepare    renameStep       `json:"prepare"`
	Edits      renameEdits      `json:"edits"`
	Collisions collisionSection `json:"collisions"`
	// Unrenamed are the whole-word mentions of the old name on lines the rename
	// does not edit: strings, comments, configuration, other languages.
	Unrenamed refSection `json:"unrenamed_mentions"`
	// Truncated and Total summarize the lists: Truncated is set when --limit
	// cut any of them, Total is the rows they had before it.
	Truncated bool `json:"truncated"`
	Total     int  `json:"total"`
}

func renameCheckCommand(e *env, c *command, args []string) int {
	var fset *flag.FlagSet
	common, sf, locArg, rest, err := parseLocationFlags(e, c, args, 1, func(fs *flag.FlagSet) { fset = fs })
	if err != nil {
		return e.flagError(err)
	}
	newName := rest[0]
	if strings.TrimSpace(newName) == "" || strings.ContainsAny(newName, " \t\r\n") {
		return e.usagef("rename_check: the new name must not be empty or contain whitespace (got %q)", newName)
	}
	format, err := managementFormat(common, "rename_check", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	limit := effectiveLimit(fset, common, defaultRenameLimit)

	cs, err := openSubject(e, common, sf, locArg)
	if err != nil {
		return e.fail(err)
	}
	defer cs.Close()
	sess := cs.Q.session
	warnings := slices.Clone(cs.Warnings)

	var run *indexRun
	if r, rerr := startIndex(e, common, cs.Root); rerr == nil {
		run = r
		defer run.close()
	} else {
		warnings = append(warnings, "the index is unavailable: "+rerr.Error())
	}
	decl, w := cs.declaration(run)
	warnings = append(warnings, w...)

	data := renameCheckData{Root: cs.Root, Symbol: cs.Subject, OldName: decl.Word, NewName: newName}
	if decl.ID != "" {
		data.Symbol = subject{ID: decl.ID, Name: decl.Name, Kind: decl.Kind, File: decl.File, Line: decl.Line, EndLine: decl.EndLine}
	}
	data.Prepare = renameStep{Evidence: evidencePrepareRename, Status: "not_supported", Detail: "the server does not advertise renameProvider.prepareProvider"}
	data.Edits = renameEdits{renameStep: renameStep{Evidence: evidenceRename, Status: "not_supported", Detail: "the server does not advertise renameProvider"}, PerFile: []renameFileEdits{}}
	data.Collisions = collisionSection{Evidence: evidenceIndex, Rows: []renameClash{}}
	data.Unrenamed = refSection{Evidence: evidenceSearchText, Rows: []refRow{}}

	if decl.Word == newName {
		data.Verdict, data.Rule = renameNoop, fmt.Sprintf("noop: %q is already the name of the symbol, so a rename changes nothing", newName)
		data.Prepare.Status, data.Prepare.Detail = "not_asked", "the name is unchanged"
		data.Edits.Status, data.Edits.Detail = "not_asked", "the name is unchanged"
		data.Collisions.Detail = "not checked: the name is unchanged"
		return finishRenameCheck(e, common, format, data, warnings)
	}

	position := textDocumentPosition(cs.Q.doc.URI, cs.Q.position)
	refusal := ""

	// 1. prepareRename: can this position be renamed at all.
	if sess.lsp.Supports(methodPrepareRename) {
		ctx, cancel := cs.Q.queryContext()
		res, qerr := sess.query(ctx, methodPrepareRename, position)
		cancel()
		switch {
		case qerr == nil:
			warnings = append(warnings, res.Warnings...)
			if perr := checkPrepareRename(res.Result, cs.Subject.Location); perr != nil {
				data.Prepare = renameStep{Evidence: evidencePrepareRename, Status: "refused", Detail: "the server will not rename the symbol at this position"}
				refusal = "prepareRename refused the position"
				data.Edits.Status, data.Edits.Detail = "not_asked", "prepareRename refused the position"
			} else {
				data.Prepare = renameStep{Evidence: evidencePrepareRename, Status: "accepted", Placeholder: prepareRenamePlaceholder(res.Result)}
			}
		default:
			if ce, ok := errCodedAs(qerr, render.CodeServerError); ok {
				data.Prepare = renameStep{Evidence: evidencePrepareRename, Status: "refused", Detail: oneLine(ce.Message)}
				refusal = "prepareRename failed: " + oneLine(ce.Message)
				data.Edits.Status, data.Edits.Detail = "not_asked", "prepareRename failed"
			} else {
				return e.fail(qerr)
			}
		}
	}

	// 2. The rename itself, staged and never written.
	var changes render.ChangeSet
	if refusal == "" && sess.lsp.Supports(methodRename) {
		ctx, cancel := cs.Q.queryContext()
		res, qerr := sess.query(ctx, methodRename, map[string]any{
			"textDocument": position["textDocument"], "position": position["position"], "newName": newName})
		cancel()
		if qerr != nil {
			ce, ok := errCodedAs(qerr, render.CodeServerError)
			if !ok {
				return e.fail(qerr)
			}
			data.Edits.Status, data.Edits.Detail = "refused", oneLine(ce.Message)
			refusal = "the server refused the rename: " + oneLine(ce.Message)
		} else {
			warnings = append(warnings, res.Warnings...)
			tx, serr := stageEdit(sess, res.Result)
			switch {
			case serr == nil:
				changes = tx.ChangeSet()
				warnings = append(warnings, tx.Warnings()...)
				data.Edits.Status, data.Edits.Detail = "accepted", ""
				summarizeEdits(&data.Edits, changes, cs.Root, limit)
				if data.Edits.Edits == 0 && data.Edits.Files == 0 {
					data.Edits.Status, data.Edits.Detail = "refused", "the server answered with no edits, so nothing would be renamed"
					refusal = "the rename produces no edits"
				}
			default:
				if ce, ok := errCodedAs(serr, render.CodeEditConflict); ok || errHasCode(serr) {
					msg := serr.Error()
					if ok {
						msg = ce.Message
					}
					data.Edits.Status, data.Edits.Detail = "refused", "the server's edit set cannot be applied: "+msg
					refusal = "the server's edit set cannot be applied"
				} else {
					return e.fail(serr)
				}
			}
		}
	}

	// 3. Collisions, from the index.
	if run != nil {
		data.Collisions = checkCollisions(run, decl, newName)
	} else {
		data.Collisions.Detail = "not checked: the workspace index could not be consulted"
	}

	// 4. What the rename will leave alone.
	if refusal == "" && decl.Word != "" {
		ctx, cancel := scanContext(e, common)
		scan, serr := scanIdentifier(ctx, cs.Root, decl.Word, nil)
		cancel()
		if serr != nil {
			warnings = append(warnings, "the mentions a rename would leave alone were not searched: "+serr.Error())
		} else {
			warnings = append(warnings, scan.Warnings...)
			edited := map[string]bool{}
			for _, ch := range changes.Changes {
				rel, _ := relToRoot(cs.Root, ch.Path)
				for _, ed := range ch.Edits {
					edited[fmt.Sprintf("%s:%d", rel, ed.Start.Line)] = true
				}
			}
			var rows []refRow
			for _, m := range scan.Matches {
				if edited[fmt.Sprintf("%s:%d", m.File, m.Line)] {
					continue
				}
				rows = append(rows, refRow{File: m.File, Line: m.Line, Column: m.Col, Text: clipRowText(m.Text),
					Test: isTestPath(m.File), Evidence: evidenceSearchText})
			}
			sortRefRows(rows)
			shown, truncated, total := capRows(rows, limit)
			data.Unrenamed.Rows = append(data.Unrenamed.Rows, shown...)
			data.Unrenamed.Total, data.Unrenamed.Truncated, data.Unrenamed.Limit = total, truncated, limit
			data.Unrenamed.Count, data.Unrenamed.Files = len(data.Unrenamed.Rows), distinctRowFiles(data.Unrenamed.Rows)
			for _, r := range data.Unrenamed.Rows {
				if r.Test {
					data.Unrenamed.InTests++
				}
			}
			if truncated {
				warnings = append(warnings, truncationWarning("unrenamed mentions", data.Unrenamed.Count, total, "raise --limit"))
			}
		}
	}

	// 5. The verdict.
	switch {
	case refusal != "" && data.Prepare.Status == "refused":
		data.Verdict, data.Rule = renameRefused, "refused: "+refusal
	case data.Collisions.Count > 0:
		data.Verdict = renameCollision
		data.Rule = fmt.Sprintf("collision: the index lists %d existing symbol(s) called %q in the scope of the renamed one", data.Collisions.Count, newName)
		if refusal != "" {
			data.Rule += "; " + refusal
		}
	case refusal != "":
		data.Verdict, data.Rule = renameRefused, "refused: "+refusal
	case data.Edits.Status != "accepted":
		data.Verdict = renameUnverified
		data.Rule = "unverified: the server cannot preview a rename, so only the index's collision check stands; use rename to see the edits"
	default:
		data.Verdict = renameOK
		data.Rule = fmt.Sprintf("ok: the server accepts the rename and it edits %d place(s) in %d file(s)", data.Edits.Edits, data.Edits.Files)
		if data.Collisions.Checked {
			data.Rule += fmt.Sprintf("; the index shows no symbol called %q in the same scope", newName)
		} else {
			data.Rule += "; name collisions were NOT checked (" + data.Collisions.Detail + ")"
		}
	}
	return finishRenameCheck(e, common, format, data, warnings)
}

func finishRenameCheck(e *env, common *commonFlags, format render.Format, data renameCheckData, warnings []string) int {
	data.Truncated = data.Edits.Truncated || data.Unrenamed.Truncated
	data.Total = data.Edits.Total + data.Unrenamed.Total
	if data.Edits.Truncated {
		warnings = append(warnings, truncationWarning("edited files", len(data.Edits.PerFile), data.Edits.Total, "raise --limit"))
	}
	exit := ExitOK
	if data.Verdict != renameOK {
		exit = ExitProblems
	}
	if format == render.FormatText {
		writeRenameCheckText(e.stdout, data, warnings)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// errHasCode reports whether err carries a machine code at all.
func errHasCode(err error) bool {
	_, ok := err.(render.Coder)
	return ok
}

// prepareRenamePlaceholder is the placeholder a prepareRename answer offers, if
// it has one.
func prepareRenamePlaceholder(raw json.RawMessage) string {
	var p struct {
		Placeholder string `json:"placeholder"`
	}
	if json.Unmarshal(raw, &p) != nil {
		return ""
	}
	return p.Placeholder
}

// summarizeEdits counts the rename preview: files, edits, and resource
// operations, with the per-file list bounded by limit.
func summarizeEdits(out *renameEdits, cs render.ChangeSet, root string, limit int) {
	var per []renameFileEdits
	for _, ch := range cs.Changes {
		rel, in := relToRoot(root, ch.Path)
		if !in {
			rel = ch.Path
		}
		per = append(per, renameFileEdits{File: rel, Edits: len(ch.Edits), Kind: string(ch.Kind)})
		out.Edits += len(ch.Edits)
	}
	out.Files = len(per)
	slices.SortStableFunc(per, func(a, b renameFileEdits) int { return strings.Compare(a.File, b.File) })
	out.PerFile, out.Truncated, out.Total = capRows(per, limit)
	out.Limit = limit
}

// dirScopedLangs are the languages whose unit of scope is the directory: two files
// of one directory are one package, so a name declared in either clashes.
var dirScopedLangs = map[string]bool{"go": true, "java": true, "kotlin": true}

// checkCollisions looks in the index for symbols already called newName in the
// scope of the declaration: the same container of the same file, and, for the
// languages scoped by directory, of the same directory.
func checkCollisions(run *indexRun, decl symbolDecl, newName string) collisionSection {
	sec := collisionSection{Evidence: evidenceIndex, Rows: []renameClash{}}
	if decl.File == "" || decl.Word == "" {
		sec.Detail = "not checked: the declaration could not be located"
		return sec
	}
	if decl.ID == "" {
		sec.Detail = "not checked: the declaration could not be identified in its file's outline, so its scope is unknown"
		return sec
	}
	dir := path.Dir(decl.File)
	q := index.SymbolsQuery{Files: []string{decl.File}}
	scope := "same_file"
	if dirScopedLangs[decl.Language] {
		q = index.SymbolsQuery{}
		if dir != "." {
			q.Path = dir
		}
		scope = "same_directory"
	}
	out, err := run.Symbols(q)
	if err != nil {
		sec.Detail = "not checked: " + err.Error()
		return sec
	}
	sec.Checked = true
	for _, f := range out.Files {
		if scope == "same_directory" && path.Dir(f.File) != dir {
			continue
		}
		for _, s := range f.Symbols {
			if s.Name != newName || s.Container != decl.Container || s.ID == decl.ID {
				continue
			}
			sc := "same_directory"
			if f.File == decl.File {
				sc = "same_file"
			}
			sec.Rows = append(sec.Rows, renameClash{ID: s.ID, Name: s.Qualified, Kind: s.Kind, File: f.File, Line: s.Line, Scope: sc, Evidence: evidenceIndex})
		}
	}
	slices.SortStableFunc(sec.Rows, func(a, b renameClash) int {
		if a.File != b.File {
			return strings.Compare(a.File, b.File)
		}
		return a.Line - b.Line
	})
	sec.Count = len(sec.Rows)
	if scope == "same_file" {
		sec.Detail = "checked the declaring file only; " + decl.Language + " has no directory-wide scope rule here"
	} else {
		sec.Detail = "checked the declaring file and the other files of its directory"
	}
	return sec
}

func writeRenameCheckText(w io.Writer, d renameCheckData, warnings []string) {
	fmt.Fprintf(w, "# rename_check %s -> %s: %s\n", d.OldName, d.NewName, d.Rule)
	step := func(s renameStep) string {
		out := fmt.Sprintf("%s (evidence: %s)", s.Status, s.Evidence)
		if s.Placeholder != "" {
			out += fmt.Sprintf(", placeholder %q", s.Placeholder)
		}
		if s.Detail != "" {
			out += ": " + s.Detail
		}
		return out
	}
	fmt.Fprintf(w, "# prepareRename: %s\n", step(d.Prepare))
	fmt.Fprintf(w, "# rename preview: %s; %d file(s), %d edit(s)\n", step(d.Edits.renameStep), d.Edits.Files, d.Edits.Edits)
	for _, f := range d.Edits.PerFile {
		fmt.Fprintf(w, "%s: %d edit(s) [%s]\n", f.File, f.Edits, f.Kind)
	}
	fmt.Fprintf(w, "# collisions (evidence: index): %d", d.Collisions.Count)
	if d.Collisions.Detail != "" {
		fmt.Fprintf(w, " — %s", d.Collisions.Detail)
	}
	fmt.Fprintln(w)
	for _, c := range d.Collisions.Rows {
		fmt.Fprintf(w, "%s:%d: %s %s  [%s]\n", c.File, c.Line, c.Kind, c.Name, c.Scope)
	}
	fmt.Fprintf(w, "# mentions the rename leaves alone (evidence: search_text): %d in %d file(s)\n", d.Unrenamed.Total, d.Unrenamed.Files)
	writeRefRows(w, d.Unrenamed.Rows)
	writeWarningLines(w, warnings)
}
