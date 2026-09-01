package edit

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/pathutil"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// workspace is the boundary every path in a WorkspaceEdit must stay
// inside.
//
// Two roots are kept. root is the lexical one, used to reject the
// cheap attacks (a URI containing ../.. , an absolute path somewhere
// else entirely) without touching the filesystem. real is root with
// symlinks resolved, used to reject the interesting one: a path that
// is lexically inside the workspace but whose directory chain leaves
// it, so that writing "inside" the workspace lands in /etc or in
// another checkout.
type workspace struct {
	root string
	real string
}

// newWorkspace resolves a workspace root. The root must exist: a
// containment check against a directory that is not there cannot mean
// anything.
func newWorkspace(root string) (*workspace, error) {
	if root == "" {
		return nil, render.Errorf(render.CodeUsage, "edit: workspace root is required")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, render.Errorf(render.CodeUsage, "edit: workspace root %q: %v", root, err)
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil {
		return nil, render.Errorf(render.CodeUsage, "edit: workspace root %s: %v", abs, err)
	}
	if !info.IsDir() {
		return nil, render.Errorf(render.CodeUsage, "edit: workspace root %s is not a directory", abs)
	}
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, render.Errorf(render.CodeIOError, "edit: resolving workspace root %s: %v", abs, err)
	}
	return &workspace{root: abs, real: filepath.Clean(real)}, nil
}

// resolve turns a URI from a language server into an absolute path
// that is safe to write, or explains why it is not.
//
// The URI is canonicalised through the vendored ParseDocumentURI
// first, because DocumentURI.Path panics on anything that is not a
// file URI — and a server answering `jdt://contents/...` or plain
// nonsense is exactly the input this package exists to survive.
func (w *workspace) resolve(uri protocol.DocumentURI) (string, error) {
	if uri == "" {
		return "", render.Errorf(render.CodeEditConflict, "edit: empty document URI")
	}
	clean, err := protocol.ParseDocumentURI(string(uri))
	if err != nil {
		return "", render.Errorf(render.CodeEditConflict, "edit: %s: not a usable file URI: %v", uri, err)
	}
	path := filepath.Clean(clean.Path())
	if !filepath.IsAbs(path) {
		return "", render.Errorf(render.CodeEditConflict, "edit: %s: path is not absolute", path)
	}
	if path == w.root || !pathutil.InDir(w.root, path) {
		return "", render.Errorf(render.CodeEditConflict,
			"edit: %s: path is outside the workspace root %s", w.rel(path), w.root)
	}
	if err := w.checkSymlinks(path); err != nil {
		return "", err
	}
	return path, nil
}

// checkSymlinks rejects a lexically-contained path that the
// filesystem would take out of the workspace.
//
// The deepest existing ancestor is resolved with EvalSymlinks and the
// still-missing tail is appended: that catches a symlinked directory
// anywhere in the chain, including one created after the root was
// resolved. The final component is then checked separately, because a
// symlink there is a different problem — an atomic rename over a
// symlink replaces the link rather than following it, so even a link
// that stays inside the workspace would silently do the wrong thing.
func (w *workspace) checkSymlinks(path string) error {
	// The target itself first, so that a symlink there is reported as
	// what it is rather than as wherever it happens to point.
	switch info, err := os.Lstat(path); {
	case errors.Is(err, fs.ErrNotExist):
		// Nothing there yet; only the chain above it matters.
	case err != nil:
		return render.Errorf(render.CodeIOError, "edit: %s: %v", w.rel(path), err)
	case info.Mode()&fs.ModeSymlink != 0:
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: refusing to write through a symlink", w.rel(path))
	case info.IsDir():
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: is a directory, not a file", w.rel(path))
	case !info.Mode().IsRegular():
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: is not a regular file (mode %s)", w.rel(path), info.Mode())
	}

	anc, tail, err := deepestExisting(path)
	if err != nil {
		return render.Errorf(render.CodeIOError, "edit: %s: %v", w.rel(path), err)
	}
	realAnc, err := filepath.EvalSymlinks(anc)
	if err != nil {
		return render.Errorf(render.CodeIOError, "edit: %s: resolving %s: %v", w.rel(path), anc, err)
	}
	resolved := filepath.Join(append([]string{realAnc}, tail...)...)
	if resolved != w.real && !pathutil.InDir(w.real, resolved) {
		return render.Errorf(render.CodeEditConflict,
			"edit: %s: resolves through a symlink to %s, outside the workspace root %s",
			w.rel(path), resolved, w.real)
	}

	return nil
}

// deepestExisting splits path into the deepest ancestor that exists
// and the components below it, in root-to-leaf order.
func deepestExisting(path string) (anc string, tail []string, err error) {
	anc = path
	for {
		if _, err := os.Lstat(anc); err == nil {
			slices.Reverse(tail) // collected leaf-first
			return anc, tail, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return "", nil, err
		}
		parent := filepath.Dir(anc)
		if parent == anc {
			// Reached the filesystem root without finding anything
			// that exists. Cannot happen for an absolute path on a
			// mounted filesystem, but do not loop if it does.
			return "", nil, fmt.Errorf("no existing ancestor")
		}
		tail = append(tail, filepath.Base(anc))
		anc = parent
	}
}

// rel renders a path relative to the workspace root for error
// messages: absolute paths in an error an agent has to read are noise,
// and the root is already known to the caller.
func (w *workspace) rel(path string) string {
	if r, err := filepath.Rel(w.root, path); err == nil && !strings.HasPrefix(r, "..") {
		return filepath.ToSlash(r)
	}
	return path
}
