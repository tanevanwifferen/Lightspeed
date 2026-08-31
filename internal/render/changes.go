package render

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/diff"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// ChangeKind is the sort of file operation a Change represents,
// mirroring the resource operations of an LSP WorkspaceEdit.
type ChangeKind string

const (
	// ChangeModify edits an existing file.
	ChangeModify ChangeKind = "modify"
	// ChangeCreate creates a file that did not exist.
	ChangeCreate ChangeKind = "create"
	// ChangeDelete removes a file.
	ChangeDelete ChangeKind = "delete"
	// ChangeRename moves a file, possibly editing it on the way.
	ChangeRename ChangeKind = "rename"
)

// Edit is one text edit with its position resolved to byte
// coordinates, so a preview can be printed without re-deriving columns.
type Edit struct {
	Span
	NewText string `json:"newText"`
}

// Change is one file's worth of a preview: what the file looks like
// now, what it would look like afterwards, and the edits in between.
//
// Before and After carry whole file contents and are never rendered
// into output; they are what the diff is computed from, and what
// internal/edit stages when the same preview is applied. Building a
// Change with one of the New*Change constructors fills them and
// resolves every edit's position through the vendored gopls Mapper.
type Change struct {
	Kind ChangeKind           `json:"kind"`
	URI  protocol.DocumentURI `json:"uri"`
	Path string               `json:"path"`
	// NewURI and NewPath are the destination of a ChangeRename.
	NewURI  protocol.DocumentURI `json:"newUri,omitempty"`
	NewPath string               `json:"newPath,omitempty"`
	// Edits is the text edits applied to this file, in source order.
	Edits []Edit `json:"edits,omitempty"`

	Before string `json:"-"`
	After  string `json:"-"`
}

// NewChange builds a ChangeModify from a file's content and the edits a
// server proposed for it.
//
// The edits are applied through the vendored protocol.ApplyEdits, which
// rejects overlapping edits — so a hostile or buggy edit set fails here
// rather than producing a plausible-looking diff.
func NewChange(m *protocol.Mapper, edits []protocol.TextEdit) (Change, error) {
	if m == nil {
		return Change{}, Errorf(CodeInternal, "NewChange: nil mapper")
	}
	after, _, err := protocol.ApplyEdits(m, edits)
	if err != nil {
		return Change{}, Errorf(CodeEditConflict, "%s: cannot apply edits: %v", m.URI.Path(), err)
	}
	resolved, err := resolveEdits(m, edits)
	if err != nil {
		return Change{}, err
	}
	return Change{
		Kind:   ChangeModify,
		URI:    m.URI,
		Path:   m.URI.Path(),
		Edits:  resolved,
		Before: string(m.Content),
		After:  string(after),
	}, nil
}

// NewCreateChange builds a ChangeCreate for a new file.
func NewCreateChange(uri protocol.DocumentURI, content string) Change {
	return Change{
		Kind:  ChangeCreate,
		URI:   uri,
		Path:  uri.Path(),
		After: content,
	}
}

// NewDeleteChange builds a ChangeDelete. The mapper supplies the
// content that would be lost, so the diff shows what is being removed
// rather than just naming the file.
func NewDeleteChange(m *protocol.Mapper) (Change, error) {
	if m == nil {
		return Change{}, Errorf(CodeInternal, "NewDeleteChange: nil mapper")
	}
	return Change{
		Kind:   ChangeDelete,
		URI:    m.URI,
		Path:   m.URI.Path(),
		Before: string(m.Content),
	}, nil
}

// NewRenameChange builds a ChangeRename, optionally editing the file as
// it moves.
func NewRenameChange(m *protocol.Mapper, dst protocol.DocumentURI, edits []protocol.TextEdit) (Change, error) {
	c, err := NewChange(m, edits)
	if err != nil {
		return Change{}, err
	}
	c.Kind = ChangeRename
	c.NewURI = dst
	c.NewPath = dst.Path()
	return c, nil
}

// resolveEdits converts LSP text edits to Edits with byte coordinates,
// sorted into source order.
func resolveEdits(m *protocol.Mapper, edits []protocol.TextEdit) ([]Edit, error) {
	out := make([]Edit, 0, len(edits))
	for _, e := range edits {
		span, err := NewSpan(m, e.Range)
		if err != nil {
			return nil, err
		}
		out = append(out, Edit{Span: span, NewText: e.NewText})
	}
	slices.SortStableFunc(out, func(a, b Edit) int { return a.Start.Offset - b.Start.Offset })
	return out, nil
}

// ChangeSet is a renderable multi-file preview.
type ChangeSet struct {
	Changes []Change `json:"changes"`
	// Total is how many changes existed before truncation the caller
	// performed. Zero means len(Changes).
	Total int `json:"total,omitempty"`
	// Truncated records truncation performed upstream of the renderer.
	Truncated bool `json:"truncated,omitempty"`
}

// Sort orders changes by path, so a multi-file diff is reproducible.
func (cs *ChangeSet) Sort() {
	slices.SortStableFunc(cs.Changes, func(a, b Change) int {
		return strings.Compare(a.Path, b.Path)
	})
}

func (cs ChangeSet) total() int {
	if cs.Total > len(cs.Changes) {
		return cs.Total
	}
	return len(cs.Changes)
}

// Changes renders an edit preview. json, text and diff are supported;
// sarif is for diagnostics only.
func Changes(w io.Writer, f Format, cs ChangeSet, opts Options) error {
	if err := opts.validate(); err != nil {
		return err
	}
	switch f {
	case FormatJSON:
		return changesJSON(w, cs, opts)
	case FormatText:
		return changesText(w, cs, opts)
	case FormatDiff:
		return changesDiff(w, cs, opts)
	case FormatSARIF:
		return unsupported(f, "changes", FormatJSON, FormatText, FormatDiff)
	default:
		_, err := ParseFormat(string(f))
		return err
	}
}

// changeView is a Change as rendered: no file contents, but the unified
// diff for this file, so that a JSON consumer can pipe
// data.changes[].diff straight into `git apply` without asking for
// --format diff separately.
type changeView struct {
	Kind    ChangeKind           `json:"kind"`
	URI     protocol.DocumentURI `json:"uri"`
	Path    string               `json:"path"`
	NewURI  protocol.DocumentURI `json:"newUri,omitempty"`
	NewPath string               `json:"newPath,omitempty"`
	Edits   []Edit               `json:"edits,omitempty"`
	Diff    string               `json:"diff,omitempty"`
}

type changesData struct {
	Changes   []changeView `json:"changes"`
	Count     int          `json:"count"`
	Total     int          `json:"total"`
	Truncated bool         `json:"truncated"`
	Limit     int          `json:"limit,omitempty"`
	// Edits is the total number of text edits across all rendered
	// changes: the number an agent should expect to see applied.
	Edits int `json:"edits"`
}

func changesJSON(w io.Writer, cs ChangeSet, opts Options) error {
	kept, cut := truncate(cs.Changes, opts.Limit)

	views := make([]changeView, 0, len(kept))
	edits := 0
	for _, c := range kept {
		patch, err := c.unified(opts)
		if err != nil {
			return err
		}
		edits += len(c.Edits)
		views = append(views, changeView{
			Kind:    c.Kind,
			URI:     c.URI,
			Path:    opts.displayPath(c.Path),
			NewURI:  c.NewURI,
			NewPath: opts.displayPath(c.NewPath),
			Edits:   displayEdits(c.Edits, opts),
			Diff:    patch,
		})
	}

	data := changesData{
		Changes:   views,
		Count:     len(views),
		Total:     cs.total(),
		Truncated: cut || cs.Truncated,
		Edits:     edits,
	}
	warnings := slices.Clone(opts.Warnings)
	if cut {
		data.Limit = opts.Limit
		warnings = append(warnings, truncationWarning("changes", len(views), cs.total(), opts.Limit))
	}
	return WriteEnvelope(w, Envelope{
		Version:  EnvelopeVersion,
		OK:       true,
		Data:     data,
		Warnings: warnings,
	}, opts)
}

// displayEdits rewrites edit paths for output without touching the
// caller's slice.
func displayEdits(edits []Edit, opts Options) []Edit {
	if len(edits) == 0 || opts.Root == "" {
		return edits
	}
	out := slices.Clone(edits)
	for i := range out {
		out[i].Path = opts.displayPath(out[i].Path)
	}
	return out
}

// changesText writes one line per edit in `file:line:col: text` form,
// where the text is the replacement. File-level operations get a single
// line at 1:1, the position of the whole-file change.
func changesText(w io.Writer, cs ChangeSet, opts Options) error {
	kept, cut := truncate(cs.Changes, opts.Limit)

	var buf bytes.Buffer
	for _, c := range kept {
		path := opts.displayPath(c.Path)
		switch c.Kind {
		case ChangeCreate:
			fmt.Fprintf(&buf, "%s:1:1: create file\n", path)
		case ChangeDelete:
			fmt.Fprintf(&buf, "%s:1:1: delete file\n", path)
		case ChangeRename:
			fmt.Fprintf(&buf, "%s:1:1: rename to %s\n", path, opts.displayPath(c.NewPath))
		}
		for _, e := range c.Edits {
			fmt.Fprintf(&buf, "%s:%d:%d: %s\n", path, e.Start.Line, e.Start.Column, oneLine(e.NewText))
		}
	}
	writeNotices(&buf, noticesFor("changes", len(kept), cs.total(), opts, cut))
	return writeAll(w, buf.Bytes())
}

// changesDiff writes a unified diff of the whole change set.
//
// Any notices go in front of the first patch: `git apply` skips
// preamble it does not recognise (that is how it tolerates a mail
// header), so a truncation notice cannot be silent and cannot break
// the patch either.
func changesDiff(w io.Writer, cs ChangeSet, opts Options) error {
	kept, cut := truncate(cs.Changes, opts.Limit)

	var buf bytes.Buffer
	writeNotices(&buf, noticesFor("changes", len(kept), cs.total(), opts, cut))
	for _, c := range kept {
		patch, err := c.unified(opts)
		if err != nil {
			return err
		}
		buf.WriteString(patch)
	}
	return writeAll(w, buf.Bytes())
}

// unified renders one change as a git-style unified diff.
//
// A ChangeRename is emitted as a delete of the old path plus a create
// of the new one. git's own `rename from`/`rename to` headers would be
// terser, but delete+create is unambiguous, needs no similarity index,
// and applies under plain `patch` as well as `git apply`.
func (c Change) unified(opts Options) (string, error) {
	switch c.Kind {
	case ChangeCreate:
		return c.filePatch(opts, "", c.After, opts.displayPath(c.Path), createPatch)
	case ChangeDelete:
		return c.filePatch(opts, c.Before, "", opts.displayPath(c.Path), deletePatch)
	case ChangeRename:
		del, err := c.filePatch(opts, c.Before, "", opts.displayPath(c.Path), deletePatch)
		if err != nil {
			return "", err
		}
		add, err := c.filePatch(opts, "", c.After, opts.displayPath(c.NewPath), createPatch)
		if err != nil {
			return "", err
		}
		return del + add, nil
	case ChangeModify, "":
		return c.filePatch(opts, c.Before, c.After, opts.displayPath(c.Path), modifyPatch)
	default:
		return "", Errorf(CodeInternal, "unknown change kind %q", c.Kind)
	}
}

// patchShape distinguishes the three git file-patch headers.
type patchShape int

const (
	modifyPatch patchShape = iota
	createPatch
	deletePatch
)

// gitFileMode is the mode git records for a new or deleted regular
// file. lightspeed never previews a mode change, so this is the only
// mode that appears.
const gitFileMode = "100644"

// filePatch renders one file's before/after as a unified diff.
//
// The `a/`…`b/` label convention and the `diff --git` header are used
// only when path is relative, i.e. when Options.Root placed the file
// inside a workspace: that is the form `git apply` consumes with its
// default -p1. With no root the labels are the absolute paths and no
// git header is emitted, which is what plain `diff -u` produces and
// what `patch -p0` consumes.
func (c Change) filePatch(opts Options, before, after, path string, shape patchShape) (string, error) {
	path = filepath.ToSlash(path)
	git := !filepath.IsAbs(path) && path != ""

	oldLabel, newLabel := path, path
	if git {
		oldLabel, newLabel = "a/"+path, "b/"+path
	}
	var header string
	switch shape {
	case createPatch:
		oldLabel = "/dev/null"
		if git {
			header = fmt.Sprintf("diff --git a/%s b/%s\nnew file mode %s\n", path, path, gitFileMode)
		}
	case deletePatch:
		newLabel = "/dev/null"
		if git {
			header = fmt.Sprintf("diff --git a/%s b/%s\ndeleted file mode %s\n", path, path, gitFileMode)
		}
	default:
		if git {
			header = fmt.Sprintf("diff --git a/%s b/%s\n", path, path)
		}
	}

	body, err := diff.ToUnified(oldLabel, newLabel, before, diff.Lines(before, after), opts.diffContext())
	if err != nil {
		// Cannot happen: diff.Lines produces edits consistent with
		// before by construction. Reported rather than ignored so a
		// future change to the vendored differ cannot pass silently.
		return "", Errorf(CodeInternal, "%s: generating diff: %v", path, err)
	}
	if body == "" && shape == modifyPatch {
		// A change that changes nothing contributes nothing: emitting a
		// header with no hunks would produce a patch git rejects.
		return "", nil
	}
	return header + body, nil
}
