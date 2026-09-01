package edit

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/diff"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// Stage validates a WorkspaceEdit and computes the resulting file
// contents in memory. It writes nothing; [Transaction.Apply] does that.
//
// Everything below is rejected, with an error naming the file and the
// range at fault, before any file is touched:
//
//   - a URI that is not a usable file URI, or a path outside the
//     workspace root, or one that reaches outside it through a
//     symlinked directory, or a path whose final component is a
//     symlink (an atomic rename would replace the link, not the file);
//   - an edit range that does not fit the document it applies to, or
//     one whose column falls inside a character that UTF-16 encodes
//     as two code units;
//   - two edits within one file whose ranges overlap;
//   - a document version that does not match the version the content
//     was read at;
//   - a create or rename that would clobber an existing file without
//     the operation's overwrite option;
//   - a rename or edit of a file that does not exist, or a delete of
//     one that does not, absent the corresponding ignore option;
//   - an edit element that is not a plain or annotated TextEdit.
//
// If both `changes` and `documentChanges` are present, documentChanges
// wins and a warning records that `changes` was ignored — that is what
// the LSP specification asks for, and silently applying both would
// double-apply the overlap between them.
func Stage(we *protocol.WorkspaceEdit, opts Options) (*Transaction, error) {
	ws, err := newWorkspace(opts.Root)
	if err != nil {
		return nil, err
	}
	t := &Transaction{
		ws:     ws,
		opts:   opts,
		states: make(map[string]*fileState),
	}
	if we == nil {
		return t, nil
	}

	switch {
	case len(we.DocumentChanges) > 0:
		if len(we.Changes) > 0 {
			t.warn("workspace edit carries both changes and documentChanges; " +
				"documentChanges applied, changes ignored")
		}
		for i, ch := range we.DocumentChanges {
			if err := t.documentChange(i, ch); err != nil {
				return nil, err
			}
		}
	case len(we.Changes) > 0:
		// A map has no order, so impose one: the same edit set must
		// stage identically on every run, or a preview cannot be
		// trusted to describe the apply.
		for _, uri := range slices.Sorted(maps.Keys(we.Changes)) {
			st, err := t.stateFor(uri)
			if err != nil {
				return nil, err
			}
			if !st.exists {
				return nil, render.Errorf(render.CodeEditConflict,
					"edit: %s: cannot edit a file that does not exist", t.ws.rel(st.path))
			}
			if err := t.editFile(st, we.Changes[uri]); err != nil {
				return nil, err
			}
		}
	}

	t.noteUnversioned()
	return t, nil
}

// Apply stages a WorkspaceEdit and commits it in one step, for callers
// that do not need the preview.
func Apply(we *protocol.WorkspaceEdit, opts Options) (*Result, error) {
	t, err := Stage(we, opts)
	if err != nil {
		return nil, err
	}
	return t.Apply()
}

// documentChange dispatches one element of documentChanges. The index
// travels into the error, because "documentChanges[3]" is something an
// agent can look up in the edit set it was handed.
func (t *Transaction) documentChange(i int, ch protocol.DocumentChange) error {
	if !ch.Valid() {
		return render.Errorf(render.CodeEditConflict,
			"edit: documentChanges[%d]: not exactly one of edit, create, rename or delete", i)
	}
	switch {
	case ch.TextDocumentEdit != nil:
		return atOperation(i, t.textDocumentEdit(ch.TextDocumentEdit))
	case ch.CreateFile != nil:
		return atOperation(i, t.createFile(ch.CreateFile))
	case ch.RenameFile != nil:
		return atOperation(i, t.renameFile(ch.RenameFile))
	case ch.DeleteFile != nil:
		return atOperation(i, t.deleteFile(ch.DeleteFile))
	}
	return nil
}

// atOperation re-labels an error with the position of the operation
// that caused it, keeping its code so that the exit-code mapping and
// errors.Is both still work.
func atOperation(i int, err error) error {
	if err == nil {
		return nil
	}
	var coded *render.CodedError
	if errors.As(err, &coded) {
		return &render.CodedError{
			Code:    coded.Code,
			Message: fmt.Sprintf("edit: documentChanges[%d]: %s", i, strings.TrimPrefix(coded.Message, "edit: ")),
			Details: coded.Details,
			Err:     err,
		}
	}
	return fmt.Errorf("documentChanges[%d]: %w", i, err)
}

func (t *Transaction) textDocumentEdit(tde *protocol.TextDocumentEdit) error {
	st, err := t.stateFor(tde.TextDocument.URI)
	if err != nil {
		return err
	}
	if !st.exists {
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: cannot edit a file that does not exist", t.ws.rel(st.path))
	}
	if err := t.checkVersion(st, tde.TextDocument.Version); err != nil {
		return err
	}
	edits, err := textEdits(t.ws.rel(st.path), tde.Edits)
	if err != nil {
		return err
	}
	if err := t.editFile(st, edits); err != nil {
		return err
	}
	// "A TextDocumentEdit describes all changes on a document version
	// Si and after they are applied move the document to version
	// Si+1" — LSP 3.17. Tracking that is not bookkeeping for its own
	// sake: a second edit for this document naming Si again would
	// have ranges relative to the text we just replaced, and applying
	// it to the new text would corrupt the file at offsets that look
	// entirely reasonable. Advancing the version turns that into the
	// mismatch it is.
	if st.version > 0 {
		st.version++
	}
	return nil
}

// textEdits converts the edits union to plain TextEdits.
//
// The vendored protocol.AsTextEdits panics on anything that is neither
// a TextEdit nor an AnnotatedTextEdit, which is correct inside gopls —
// it produced the value — and unacceptable here, where the value came
// off the wire from someone else's server. Same conversion, error
// instead of panic.
func textEdits(name string, elems []protocol.Or_TextDocumentEdit_edits_Elem) ([]protocol.TextEdit, error) {
	out := make([]protocol.TextEdit, 0, len(elems))
	for i, e := range elems {
		switch v := e.Value.(type) {
		case protocol.TextEdit:
			out = append(out, v)
		case protocol.AnnotatedTextEdit:
			out = append(out, v.TextEdit)
		case protocol.SnippetTextEdit:
			return nil, render.Errorf(render.CodeEditConflict,
				"edit: %s: edit %d is a snippet edit, which has no meaning outside an editor", name, i)
		case nil:
			return nil, render.Errorf(render.CodeEditConflict,
				"edit: %s: edit %d is null", name, i)
		default:
			return nil, render.Errorf(render.CodeEditConflict,
				"edit: %s: edit %d has unexpected type %T", name, i, v)
		}
	}
	return out, nil
}

// editFile validates a batch of edits against a file's staged content
// and folds them in. Several batches may land on one file — an edit,
// a rename, another edit — and each is relative to the content the
// ones before it produced.
//
// Conversion and ordering are the vendored gopls code's job
// (EditsToDiffEdits, SortEdits, diff.Apply): every offset is relative
// to the content as it was before this batch, and diff.Apply
// materialises the result from those original offsets in one pass —
// the same invariant that applying in reverse order preserves, without
// the in-place mutation.
func (t *Transaction) editFile(st *fileState, edits []protocol.TextEdit) error {
	if len(edits) == 0 {
		return nil
	}
	name := t.ws.rel(st.path)
	m := protocol.NewMapper(st.uri, st.content)

	edits = normalizeEdits(edits, st.eol)
	// Validate each range first: the batch conversion below reports
	// the first failure without saying which edit caused it, and an
	// error that does not name the offending range is not much of an
	// error.
	for _, e := range edits {
		if err := checkRange(name, m, e.Range); err != nil {
			return err
		}
	}
	converted, err := protocol.EditsToDiffEdits(m, edits)
	if err != nil {
		return render.Errorf(render.CodeInternal,
			"edit: %s: converting validated edits: %v", name, err)
	}

	sorted := slices.Clone(converted)
	diff.SortEdits(sorted)
	if err := checkOverlap(name, m, sorted); err != nil {
		return err
	}

	out, err := diff.ApplyBytes(st.content, sorted)
	if err != nil {
		// Unreachable: the checks above are strictly stronger than
		// diff.Apply's. Reported rather than ignored so that a change
		// to either side cannot pass silently.
		return render.Errorf(render.CodeInternal, "edit: %s: applying staged edits: %v", name, err)
	}

	for _, e := range edits {
		span, err := render.NewSpan(m, e.Range)
		if err != nil {
			return render.Errorf(render.CodeInternal,
				"edit: %s: resolving a validated range %s: %v", name, rangeText(e.Range), err)
		}
		st.edits = append(st.edits, render.Edit{Span: span, NewText: e.NewText})
	}
	slices.SortStableFunc(st.edits, func(a, b render.Edit) int { return a.Start.Offset - b.Start.Offset })

	st.content = out
	return nil
}

// checkRange rejects a range the document cannot represent.
func checkRange(name string, m *protocol.Mapper, r protocol.Range) error {
	if _, _, err := m.RangeOffsets(r); err != nil {
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: range %s is outside the document: %v", name, rangeText(r), err)
	}
	if err := checkPosition(name, m, r.Start); err != nil {
		return err
	}
	return checkPosition(name, m, r.End)
}

// checkPosition rejects a UTF-16 character offset that falls inside a
// character made of two code units — the second half of the surrogate
// pair encoding an emoji, say.
//
// The vendored Mapper resolves such a position by snapping to the
// start of the character it landed in. That is the forgiving thing to
// do for a query, and the wrong thing to do for a write: an edit that
// names half a character would silently consume the whole one. The
// snap is detected by converting the offset back — a position that
// resolves to a *smaller* column on the same line was moved.
//
// A position past the last line with column 0 is left alone: that is
// the ordinary way to name end-of-file, and it round-trips to a
// different line by construction.
func checkPosition(name string, m *protocol.Mapper, p protocol.Position) error {
	offset, err := m.PositionOffset(p)
	if err != nil {
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: position %d:%d is outside the document: %v",
			name, p.Line+1, p.Character+1, err)
	}
	got, err := m.OffsetPosition(offset)
	if err != nil {
		return render.Errorf(render.CodeInternal,
			"edit: %s: converting offset %d back: %v", name, offset, err)
	}
	if got.Line == p.Line && got.Character < p.Character {
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: position %d:%d falls inside a multi-unit character and resolves to %d:%d; "+
				"the edit does not name the text it would change",
			name, p.Line+1, p.Character+1, got.Line+1, got.Character+1)
	}
	return nil
}

// checkOverlap rejects edits that fight over the same bytes.
//
// Edits sharing a start offset are not an overlap as long as at most
// one of them consumes anything: several insertions at one point are
// applied in the order the server listed them, which is what the
// vendored SortEdits is stable to preserve. An edit that starts before
// the previous one ends is a genuine conflict — the two disagree about
// what the resulting text is, and the answer would depend on which one
// happened to be applied second.
func checkOverlap(name string, m *protocol.Mapper, sorted []diff.Edit) error {
	for i := 1; i < len(sorted); i++ {
		prev, cur := sorted[i-1], sorted[i]
		if cur.Start < prev.End {
			return render.Errorf(render.CodeEditConflict,
				"edit: %s: overlapping edits at %s and %s; nothing was written",
				name, offsetsText(m, prev.Start, prev.End), offsetsText(m, cur.Start, cur.End))
		}
	}
	return nil
}

// normalizeEdits rewrites inserted line terminators to match the file.
func normalizeEdits(edits []protocol.TextEdit, eol lineEnding) []protocol.TextEdit {
	if eol != eolCRLF {
		return edits
	}
	out := slices.Clone(edits)
	for i := range out {
		out[i].NewText = normalizeEOL(out[i].NewText, eol)
	}
	return out
}

func (t *Transaction) createFile(op *protocol.CreateFile) error {
	st, err := t.stateFor(op.URI)
	if err != nil {
		return err
	}
	overwrite, ignoreIfExists := false, false
	if op.Options != nil {
		// "Overwrite wins over ignoreIfExists" — LSP 3.17.
		overwrite, ignoreIfExists = op.Options.Overwrite, op.Options.IgnoreIfExists
	}
	if st.exists {
		switch {
		case overwrite:
			st.content = nil
			st.eol = eolUnknown
			st.edits = nil
		case ignoreIfExists:
			t.warn(fmt.Sprintf("create %s: file exists and ignoreIfExists was set; skipped",
				t.ws.rel(st.path)))
		default:
			return render.Errorf(render.CodeEditConflict,
				"edit: %s: create would overwrite an existing file "+
					"and the operation sets neither overwrite nor ignoreIfExists",
				t.ws.rel(st.path))
		}
		return nil
	}
	st.exists = true
	st.content = nil
	st.eol = eolUnknown
	if st.mode == 0 {
		st.mode = defaultFileMode
	}
	return nil
}

func (t *Transaction) renameFile(op *protocol.RenameFile) error {
	src, err := t.stateFor(op.OldURI)
	if err != nil {
		return err
	}
	dst, err := t.stateFor(op.NewURI)
	if err != nil {
		return err
	}
	if src.path == dst.path {
		t.warn(fmt.Sprintf("rename %s: source and destination are the same file; skipped",
			t.ws.rel(src.path)))
		return nil
	}
	if !src.exists {
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: cannot rename a file that does not exist", t.ws.rel(src.path))
	}
	overwrite, ignoreIfExists := false, false
	if op.Options != nil {
		overwrite, ignoreIfExists = op.Options.Overwrite, op.Options.IgnoreIfExists
	}
	if dst.exists {
		switch {
		case overwrite:
		case ignoreIfExists:
			t.warn(fmt.Sprintf("rename %s to %s: destination exists and ignoreIfExists was set; skipped",
				t.ws.rel(src.path), t.ws.rel(dst.path)))
			return nil
		default:
			return render.Errorf(render.CodeEditConflict,
				"edit: %s: rename would overwrite an existing file "+
					"and the operation sets neither overwrite nor ignoreIfExists",
				t.ws.rel(dst.path))
		}
	}

	// A rename whose source was itself renamed here (A→B→C) is a move
	// from the file that is actually on disk, not from the waypoint
	// that never existed.
	from := src.path
	if !src.onDisk && src.from != "" {
		from = src.from
	}

	dst.exists = true
	dst.content = src.content
	dst.mode = src.mode
	dst.eol = src.eol
	dst.edits = src.edits
	dst.from = from

	src.exists = false
	src.content = nil
	src.edits = nil
	return nil
}

func (t *Transaction) deleteFile(op *protocol.DeleteFile) error {
	st, err := t.stateFor(op.URI)
	if err != nil {
		return err
	}
	ignoreIfNotExists := op.Options != nil && op.Options.IgnoreIfNotExists
	if !st.exists {
		if ignoreIfNotExists {
			t.warn(fmt.Sprintf("delete %s: file does not exist and ignoreIfNotExists was set; skipped",
				t.ws.rel(st.path)))
			return nil
		}
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: cannot delete a file that does not exist", t.ws.rel(st.path))
	}
	// Directories never get this far: stateFor refuses to resolve one,
	// so options.recursive has nothing to act on. Deleting a directory
	// tree is not something a language server should be asking a
	// refactoring CLI to do, and it is not undoable from memory.
	st.exists = false
	st.content = nil
	st.edits = nil
	return nil
}

// stateFor resolves a URI and returns its staged state, reading the
// file the first time it is mentioned. Reading once per file is what
// makes the whole edit set consistent: later operations see the
// earlier ones' results, not the disk.
func (t *Transaction) stateFor(uri protocol.DocumentURI) (*fileState, error) {
	path, err := t.ws.resolve(uri)
	if err != nil {
		return nil, err
	}
	if st, ok := t.states[path]; ok {
		return st, nil
	}
	st := &fileState{path: path, uri: protocol.URIFromPath(path), mode: defaultFileMode}
	doc, err := t.opts.source().ReadDocument(path)
	switch {
	case err == nil:
		st.onDisk = true
		st.exists = true
		st.original = slices.Clone(doc.Content)
		st.content = st.original
		st.version = doc.Version
		st.eol = detectEOL(st.original)
		if doc.Mode != 0 {
			st.mode = doc.Mode.Perm()
		}
	case errors.Is(err, fs.ErrNotExist):
		// Not an error yet: a create operation is allowed to name it.
	default:
		return nil, render.Errorf(render.CodeIOError, "edit: %s: %v", t.ws.rel(path), err)
	}
	t.states[path] = st
	t.order = append(t.order, path)
	return st, nil
}

// checkVersion compares the version an edit was computed against with
// the version of the content it is about to be applied to.
func (t *Transaction) checkVersion(st *fileState, want int32) error {
	if want <= 0 {
		// null, or absent: the LSP spec's "the content on disk is the
		// truth". Nothing to check.
		return nil
	}
	if st.version == 0 {
		if t.opts.RequireVersions {
			return render.Errorf(render.CodeEditConflict,
				"edit: %s: edit was computed against version %d but the content is unversioned",
				t.ws.rel(st.path), want)
		}
		st.unverified = want
		return nil
	}
	if st.version != want {
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: edit was computed against version %d but the document is at version %d; "+
				"the file changed under the server",
			t.ws.rel(st.path), want, st.version)
	}
	return nil
}

// noteUnversioned reports, once, that some version claims could not be
// checked. Silence would imply they had been.
func (t *Transaction) noteUnversioned() {
	var names []string
	for _, path := range t.order {
		if t.states[path].unverified > 0 {
			names = append(names, t.ws.rel(path))
		}
	}
	if len(names) == 0 {
		return
	}
	slices.Sort(names)
	t.warn(fmt.Sprintf("document version not checked for %s: the content is unversioned",
		joinNames(names)))
}

func joinNames(names []string) string {
	const max = 3
	if len(names) <= max {
		out := names[0]
		for _, n := range names[1:] {
			out += ", " + n
		}
		return out
	}
	return fmt.Sprintf("%s and %d more", joinNames(names[:max]), len(names)-max)
}

// warn records a non-fatal note, ignoring exact duplicates.
func (t *Transaction) warn(msg string) {
	if slices.Contains(t.warnings, msg) {
		return
	}
	t.warnings = append(t.warnings, msg)
}

// rangeText formats an LSP range for an error message. It is used only
// where the range could not be resolved to byte coordinates, so the
// UTF-16 numbers the server sent are all there is to report.
func rangeText(r protocol.Range) string {
	return fmt.Sprintf("%d:%d-%d:%d",
		r.Start.Line+1, r.Start.Character+1, r.End.Line+1, r.End.Character+1)
}

// offsetsText formats a byte range as 1-based line:col, the same
// coordinates lightspeed accepts as input.
func offsetsText(m *protocol.Mapper, start, end int) string {
	sl, sc := m.OffsetLineCol8(start)
	el, ec := m.OffsetLineCol8(end)
	if sl == el && sc == ec {
		return fmt.Sprintf("%d:%d", sl, sc)
	}
	return fmt.Sprintf("%d:%d-%d:%d", sl, sc, el, ec)
}
