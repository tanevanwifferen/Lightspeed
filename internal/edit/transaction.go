package edit

import (
	"bytes"
	"io/fs"
	"os"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// defaultFileMode is the mode a file created by a WorkspaceEdit gets.
// Modified files keep the mode they already had.
const defaultFileMode fs.FileMode = 0o644

// Document is the pre-edit state of one file, as the caller sees it.
type Document struct {
	// Content is the exact bytes the edit's ranges are relative to.
	Content []byte
	// Version is the LSP document version the content corresponds to.
	// Zero means unversioned — the file was read from disk and never
	// opened, so there is no version to check an edit against.
	Version int32
	// Mode is the file's permission bits, preserved across the write.
	// Zero means defaultFileMode.
	Mode fs.FileMode
}

// Source supplies the pre-edit content of the files an edit touches.
//
// It exists so that the applier can be driven from the docstore's open
// documents (which carry versions) as easily as from the disk, and so
// that tests need no filesystem. A Source that reports content
// differing from the disk does not weaken the transaction: Apply
// re-reads every file it is about to touch and refuses to write if the
// bytes it staged from are no longer the bytes on disk.
type Source interface {
	// ReadDocument returns the document at an absolute path. A file
	// that does not exist must be reported with an error satisfying
	// errors.Is(err, fs.ErrNotExist).
	ReadDocument(path string) (Document, error)
}

// DiskSource reads documents from the filesystem. It is the default
// Source, and reports every document as unversioned.
type DiskSource struct{}

// ReadDocument implements [Source].
func (DiskSource) ReadDocument(path string) (Document, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return Document{}, err
	}
	mode := defaultFileMode
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	return Document{Content: content, Mode: mode}, nil
}

// Options configures staging.
type Options struct {
	// Root is the workspace root. Every path the edit touches must
	// resolve inside it, symlinks included. Required.
	Root string

	// Source supplies pre-edit file content. nil means DiskSource.
	Source Source

	// RequireVersions makes an edit that names a document version for
	// a document whose version is unknown an error, rather than an
	// unverifiable claim to be taken on trust. Off by default: the
	// CLI reads most files straight off the disk, where there is no
	// version to compare against.
	RequireVersions bool
}

func (o Options) source() Source {
	if o.Source != nil {
		return o.Source
	}
	return DiskSource{}
}

// fileState is one file's staged state: where it came from, what it
// will contain, and what it contained when we read it.
type fileState struct {
	path string
	uri  protocol.DocumentURI

	// exists reports whether the file is present in the staged tree.
	exists bool
	// content is the staged content; meaningful only when exists.
	content []byte
	mode    fs.FileMode
	eol     lineEnding

	// onDisk reports whether the file existed when staging read it,
	// and original is what it contained then. Apply re-checks both.
	onDisk   bool
	original []byte
	version  int32

	// unverified is a version an edit claimed that could not be
	// checked, because the content was read from a source that has no
	// versions. Reported as a warning rather than silently trusted.
	unverified int32

	// from is the path this file's content was renamed from, if any.
	from string
	// edits accumulates the resolved edits for the preview.
	edits []render.Edit
}

// Rename is one file move performed by a transaction.
type Rename struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// Result reports what a committed transaction did. Paths are absolute
// and each list is sorted.
type Result struct {
	Created   []string `json:"created,omitempty"`
	Modified  []string `json:"modified,omitempty"`
	Deleted   []string `json:"deleted,omitempty"`
	Renamed   []Rename `json:"renamed,omitempty"`
	Unchanged []string `json:"unchanged,omitempty"`
}

// Changed reports whether anything was written.
func (r *Result) Changed() bool {
	return r != nil && (len(r.Created) > 0 || len(r.Modified) > 0 || len(r.Deleted) > 0 || len(r.Renamed) > 0)
}

// Transaction is a validated, staged edit set that has not been
// written. It is safe to render, to inspect, and to throw away; it is
// not safe for concurrent use, and Apply may be called only once.
type Transaction struct {
	ws       *workspace
	opts     Options
	states   map[string]*fileState
	order    []string
	warnings []string
	applied  bool

	// renameFn replaces os.Rename in the commit's forward path. Tests
	// set it to inject a failure part-way through a multi-file write;
	// it is nil everywhere else.
	renameFn func(oldpath, newpath string) error
}

// Root is the workspace root the transaction is confined to.
func (t *Transaction) Root() string { return t.ws.root }

// Warnings are non-fatal notes about the edit set — a malformed part
// that was ignorable, an operation that turned out to be a no-op.
// They belong in the output envelope, not in a log nobody reads.
func (t *Transaction) Warnings() []string { return slices.Clone(t.warnings) }

// Empty reports whether the transaction would change nothing.
func (t *Transaction) Empty() bool {
	for _, st := range t.states {
		if st.dirty() {
			return false
		}
	}
	return true
}

// dirty reports whether committing this state would touch the disk.
func (st *fileState) dirty() bool {
	switch {
	case st.exists && !st.onDisk:
		return true
	case !st.exists && st.onDisk:
		return true
	case st.exists && st.onDisk:
		return !bytes.Equal(st.content, st.original)
	default:
		// Staged as absent and absent on disk: an edit that created a
		// file and then deleted it again, which is nothing.
		return false
	}
}

// Files lists every path the transaction would touch, sorted. A file
// the edit mentions but does not actually change is included: the
// caller asked what was touched, not what differs.
func (t *Transaction) Files() []string {
	out := make([]string, 0, len(t.states))
	for path := range t.states {
		out = append(out, path)
	}
	slices.Sort(out)
	return out
}

// ChangeSet is the staged result as a renderable preview, built from
// exactly the bytes Apply would write.
//
// A rename is reported as a rename only when its destination did not
// already exist; a rename that overwrites an existing file is reported
// as a delete of the source plus a modification of the destination,
// because that is what happens on disk and what a patch of it has to
// say to be appliable.
func (t *Transaction) ChangeSet() render.ChangeSet {
	var cs render.ChangeSet
	renamed := t.renameSources()

	for _, path := range t.order {
		st := t.states[path]
		switch {
		case st.exists && !st.onDisk:
			if src, ok := t.renameOf(st); ok {
				cs.Changes = append(cs.Changes, render.Change{
					Kind:    render.ChangeRename,
					URI:     src.uri,
					Path:    src.path,
					NewURI:  st.uri,
					NewPath: st.path,
					Edits:   st.edits,
					Before:  string(src.original),
					After:   string(st.content),
				})
				continue
			}
			cs.Changes = append(cs.Changes, render.Change{
				Kind:   render.ChangeCreate,
				URI:    st.uri,
				Path:   st.path,
				Edits:  st.edits,
				After:  string(st.content),
				Before: "",
			})
		case !st.exists && st.onDisk:
			if renamed[st.path] {
				continue // reported as part of the rename
			}
			cs.Changes = append(cs.Changes, render.Change{
				Kind:   render.ChangeDelete,
				URI:    st.uri,
				Path:   st.path,
				Before: string(st.original),
			})
		case st.exists && st.onDisk:
			if bytes.Equal(st.content, st.original) && len(st.edits) == 0 {
				continue
			}
			cs.Changes = append(cs.Changes, render.Change{
				Kind:   render.ChangeModify,
				URI:    st.uri,
				Path:   st.path,
				Edits:  st.edits,
				Before: string(st.original),
				After:  string(st.content),
			})
		}
	}
	cs.Sort()
	return cs
}

// renameOf reports the state a file's content was renamed from, if the
// move is a true rename: the source has to have existed on disk and to
// be gone from the staged tree, and the destination has to be new.
func (t *Transaction) renameOf(st *fileState) (*fileState, bool) {
	if st.from == "" || st.onDisk {
		// A destination that already existed is being overwritten, not
		// moved into: on disk that is a delete plus a modification,
		// and reporting it as a rename would produce a patch that
		// tries to create a file that is already there.
		return nil, false
	}
	src, ok := t.states[st.from]
	if !ok || src.exists || !src.onDisk {
		return nil, false
	}
	return src, true
}

// renameSources is the set of paths reported as the source half of a
// rename, and therefore not separately as a delete.
func (t *Transaction) renameSources() map[string]bool {
	out := make(map[string]bool)
	for _, st := range t.states {
		if src, ok := t.renameOf(st); ok {
			out[src.path] = true
		}
	}
	return out
}

// Diff renders the staged result as a unified diff. Paths are made
// relative to the workspace root unless the caller says otherwise, so
// the patch feeds `git apply` with its default -p1.
func (t *Transaction) Diff(opts render.Options) (string, error) {
	if opts.Root == "" {
		opts.Root = t.ws.root
	}
	if len(opts.Warnings) == 0 {
		opts.Warnings = t.Warnings()
	}
	var buf strings.Builder
	if err := render.Changes(&buf, render.FormatDiff, t.ChangeSet(), opts); err != nil {
		return "", err
	}
	return buf.String(), nil
}
