package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `outline`: the symbols of one or more files as a tree, with ids
// (docs/DECISIONS.md D22). It is the call an agent makes instead of reading a
// file: what is declared here, how, and where, in a few tokens per symbol.

// A sessionSet is the sessions a command with several files needs: one per
// (server, workspace root), started when the first file that needs it comes
// up, so that a batch of Go files is one gopls session and a batch of Go and
// Python files is two. The outline of each file is asked for once.
type sessionSet struct {
	e      *env
	common *commonFlags
	root   string

	sessions map[string]*session
	order    []*session
	files    map[string]*fileSymbols
	warns    []string
}

func newSessionSet(e *env, common *commonFlags, root string) *sessionSet {
	return &sessionSet{e: e, common: common, root: root,
		sessions: map[string]*session{}, files: map[string]*fileSymbols{}}
}

// load opens a file in the session of the server that handles it and asks for
// its symbols.
func (ss *sessionSet) load(file string) (*fileSymbols, error) {
	key := canonPath(file)
	if ff, ok := ss.files[key]; ok {
		return ff, nil
	}
	match, err := ss.e.resolveTarget(file, "", ss.common.server)
	if err != nil {
		return nil, err
	}
	sk := match.Server.Name + "\x00" + match.Root
	s, ok := ss.sessions[sk]
	if !ok {
		connectCtx, cancel := context.WithTimeout(ss.e.base(), ss.common.timeout)
		defer cancel()
		s, err = startSession(connectCtx, ss.e, match, ss.common.gateOptions())
		if err != nil {
			return nil, err
		}
		ss.sessions[sk] = s
		ss.order = append(ss.order, s)
	}
	ff, err := s.loadFile(ss.root, file)
	if err != nil {
		return nil, err
	}
	ss.files[key] = ff
	ss.warns = append(ss.warns, ff.warnings...)
	return ff, nil
}

// warnings are what loading the files said, once each.
func (ss *sessionSet) warnings() []string {
	seen := map[string]bool{}
	var out []string
	for _, w := range ss.warns {
		if !seen[w] {
			seen[w] = true
			out = append(out, w)
		}
	}
	return out
}

// close ends every session, closing the documents the command opened.
func (ss *sessionSet) close() {
	for _, s := range ss.order {
		s.close()
	}
}

// An outlineNode is one symbol of `outline`.
type outlineNode struct {
	ID   string `json:"id,omitempty"`
	Kind string `json:"kind"`
	Name string `json:"name"`
	// Signature is the server's own detail when it gives one, and the
	// declaration line(s) sliced from the file when it does not.
	Signature string `json:"signature,omitempty"`
	// Line and EndLine are 1-based and inclusive: the declaration itself,
	// not its doc comment (`source` includes that).
	Line     int            `json:"line"`
	EndLine  int            `json:"end_line"`
	Children []*outlineNode `json:"children,omitempty"`
}

// An outlineFile is one file of `outline`.
type outlineFile struct {
	// File is the path relative to the workspace, or absolute for a file
	// outside it (which has no ids).
	File     string         `json:"file"`
	Language string         `json:"language,omitempty"`
	Symbols  []*outlineNode `json:"symbols"`
	// Count is the number of symbols listed for the file, nested ones
	// included.
	Count int `json:"count"`
	// Error is why this file has no outline, when it has none because of the
	// file itself — it does not exist, or no server handles it — while the
	// others in the call do.
	Error *itemError `json:"error,omitempty"`
}

// outlineData is the payload of `outline`.
type outlineData struct {
	Files []outlineFile `json:"files"`
	// Count is how many symbols this output lists, Total how many there
	// were before --limit. They differ exactly when Truncated is set.
	Count     int  `json:"count"`
	Total     int  `json:"total"`
	Truncated bool `json:"truncated"`
	Limit     int  `json:"limit,omitempty"`
}

// buildOutline nests a file's symbols by their parents and spends budget on
// them in document order. budget is decremented for each symbol listed; when
// it runs out, later symbols are counted and left out.
func buildOutline(ff *fileSymbols, budget *int, limited bool) (nodes []*outlineNode, listed, total int) {
	x := newLineIndex(ff.doc.Content)
	order := make([]int, len(ff.syms))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		return posBefore(ff.syms[order[a]].Range.Start, ff.syms[order[b]].Range.Start)
	})

	byIndex := map[int]*outlineNode{}
	for _, i := range order {
		total++
		if limited && *budget <= 0 {
			continue
		}
		sym := ff.syms[i]
		node := &outlineNode{
			Kind:      idKind(sym.Kind),
			Name:      sym.Name,
			Signature: sym.Signature,
			Line:      int(sym.Full.Start.Line) + 1,
			EndLine:   endLine(sym) + 1,
		}
		if ff.ids != nil {
			node.ID = ff.ids[i]
		}
		if node.Signature == "" {
			node.Signature = x.DeclSignature(int(sym.Full.Start.Line), endLine(sym))
		}
		// A symbol whose parent was left out by the limit cannot be:
		// parents precede their children in position order.
		if parent, ok := byIndex[sym.Parent-1]; ok && sym.Parent > 0 {
			parent.Children = append(parent.Children, node)
		} else {
			nodes = append(nodes, node)
		}
		byIndex[i] = node
		listed++
		*budget--
	}
	return nodes, listed, total
}

// outlineCommand implements `lightspeed outline <file>...`.
func outlineCommand(e *env, c *command, args []string) int {
	var anchor string
	common, files, err := parseFlagsRange(e, c, args, 1, -1, func(fs *flag.FlagSet) {
		fs.StringVar(&anchor, "path", ".", "directory whose workspace the symbol ids are relative to")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "outline", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	root, err := commandWorkspace(anchor)
	if err != nil {
		return e.fail(err)
	}

	// Every file is checked before a server is asked anything: a typo in the
	// fifth should not cost the first four a server start. A file that is not
	// there is that file's error, though, and not the batch's: the others are
	// outlined and the exit status says there was a problem (D31).
	type target struct {
		arg, path string
		// name is what the file is called in the answer: its path relative to
		// the workspace, as its id would spell it, or as typed for a file
		// outside it (D31).
		name string
		err  error
	}
	targets := make([]target, 0, len(files))
	seen := map[string]bool{}
	for _, f := range files {
		p, err := filepath.Abs(f)
		if err != nil {
			return e.usagef("resolving %s: %v", f, err)
		}
		if seen[canonPath(p)] {
			continue
		}
		seen[canonPath(p)] = true
		name := f
		if rel, in := relToRoot(root, p); in {
			name = rel
		}
		targets = append(targets, target{arg: f, path: p, name: name, err: mustBeFile(p)})
	}

	ss := newSessionSet(e, common, root)
	defer ss.close()
	budget := common.limit
	data := outlineData{Files: make([]outlineFile, 0, len(targets)), Limit: common.limit}
	var (
		warnings []string
		failed   []error
	)
	for _, t := range targets {
		var ff *fileSymbols
		if t.err == nil {
			ff, t.err = ss.load(t.path)
			// A file that no server handles, or that a server cannot
			// outline, is that file's problem. A server that is not ready or
			// has crashed is everyone's: nothing after it could be believed.
			if t.err != nil && !fileLevelError(t.err) {
				return e.fail(t.err)
			}
		}
		if t.err != nil {
			t.err = namedError(t.err, t.name, t.path)
			ie := newItemError(t.name, t.err)
			failed = append(failed, t.err)
			warnings = append(warnings, ie.warning())
			data.Files = append(data.Files, outlineFile{File: t.name, Symbols: []*outlineNode{}, Error: &ie})
			continue
		}
		nodes, listed, total := buildOutline(ff, &budget, common.limit > 0)
		if nodes == nil {
			nodes = []*outlineNode{}
		}
		data.Files = append(data.Files, outlineFile{
			File: ff.displayPath(), Language: ff.doc.LanguageID, Symbols: nodes, Count: listed,
		})
		data.Count += listed
		data.Total += total
	}
	data.Truncated = data.Count < data.Total

	if len(failed) == len(data.Files) {
		// Nothing was outlined: that is the error itself, as it was when the
		// command took one file, and the code tells a caller what to do.
		err := failed[0]
		if len(failed) > 1 {
			errs := make([]itemError, 0, len(data.Files))
			for _, f := range data.Files {
				errs = append(errs, *f.Error)
			}
			err = &render.CodedError{Code: render.CodeForError(err), Message: fmt.Sprintf("none of the %d files could be outlined; the first: %v", len(failed), err),
				Details: map[string]any{"errors": errs}}
		}
		return e.fail(err)
	}
	warnings = append(ss.warnings(), warnings...)
	if data.Truncated {
		warnings = append(warnings, fmt.Sprintf("listed %d of %d symbols; raise --limit or outline fewer files", data.Count, data.Total))
	}
	exit := ExitOK
	if len(failed) > 0 {
		exit = ExitProblems
	}
	if format == render.FormatText {
		writeOutlineText(e.stdout, data)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// namedError is err with the absolute path of a file spelled as the file is
// called in the answer (its workspace-relative name), so that the message, like
// the target beside it, does not leak the machine's layout.
func namedError(err error, name string, abs ...string) error {
	rewrite := func(msg string) string {
		for _, a := range abs {
			msg = strings.ReplaceAll(msg, a, name)
			if c := canonPath(a); c != a {
				msg = strings.ReplaceAll(msg, c, name)
			}
		}
		return msg
	}
	if coded, ok := err.(*render.CodedError); ok {
		if msg := rewrite(coded.Message); msg != coded.Message {
			cp := *coded
			cp.Message = msg
			return &cp
		}
		return err
	}
	if msg := rewrite(err.Error()); msg != err.Error() {
		return errors.New(msg)
	}
	return err
}

// fileLevelError reports whether err says something about one file, and not
// about the server or the workspace: the file is not there, is not a file, or
// nothing handles it.
func fileLevelError(err error) bool {
	switch render.CodeForError(err) {
	case render.CodeNoSuchFile, render.CodeNoServer, render.CodeServerNotInstalled, render.CodeUnsupportedMethod:
		return true
	}
	return false
}

// writeOutlineText prints an outline one symbol per line, indented by depth:
// `file:line: kind name  signature`.
func writeOutlineText(w io.Writer, d outlineData) {
	var walk func(file string, nodes []*outlineNode, depth int)
	walk = func(file string, nodes []*outlineNode, depth int) {
		for _, n := range nodes {
			fmt.Fprintf(w, "%s:%d: %s%s %s", file, n.Line, strings.Repeat("  ", depth), n.Kind, n.Name)
			if n.Signature != "" {
				fmt.Fprintf(w, "  %s", n.Signature)
			}
			fmt.Fprintln(w)
			walk(file, n.Children, depth+1)
		}
	}
	for _, f := range d.Files {
		if f.Error != nil {
			fmt.Fprintf(w, "# %s\n", f.Error.warning())
			continue
		}
		walk(f.File, f.Symbols, 0)
	}
	if d.Truncated {
		fmt.Fprintf(w, "# %d of %d symbols; --limit raises the cap\n", d.Count, d.Total)
	}
}
