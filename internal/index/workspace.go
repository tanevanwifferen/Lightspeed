package index

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

func routerLanguage(rel string) string { return router.LanguageID(rel) }

// snapWorkspace is the [Workspace] of one snapshot: the files that were listed,
// and reads confined to them.
type snapWorkspace struct {
	root string
	snap *Snapshot
}

func newWorkspace(root string, snap *Snapshot) Workspace {
	return &snapWorkspace{root: root, snap: snap}
}

func (w *snapWorkspace) Has(p string) bool {
	_, ok := w.snap.State[path.Clean(p)]
	return ok
}

func (w *snapWorkspace) Files() []string { return w.snap.Files }

// Read returns a listed file's content. Only files of the snapshot can be read,
// and never through a symlink, so a resolver looking for go.mod or Cargo.toml
// cannot be led out of the workspace.
func (w *snapWorkspace) Read(p string) ([]byte, error) {
	p = path.Clean(p)
	if _, ok := w.snap.State[p]; !ok || strings.HasPrefix(p, "../") || path.IsAbs(p) {
		return nil, &fs.PathError{Op: "read", Path: p, Err: errors.Join(fs.ErrNotExist)}
	}
	if _, why := statRegular(pathJoin(w.root, p)); why != "" {
		return nil, &fs.PathError{Op: "read", Path: p, Err: fs.ErrNotExist}
	}
	return os.ReadFile(pathJoin(w.root, p))
}
