package edit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// TestSymlinkEscapeRefused is the containment test: a path that is
// lexically inside the workspace but that the filesystem would take
// outside it.
func TestSymlinkEscapeRefused(t *testing.T) {
	outside := newTree(t, map[string]string{
		"secret.txt": "do not touch\n",
		"sub/x.go":   "package sub\n",
	})
	root := newTree(t, map[string]string{
		"a.go":     "package a\n",
		"real.go":  "package real\n",
		"inner/":   "",
		"keep.txt": "keep\n",
	})
	// A directory that leaves the workspace, a file that leaves the
	// workspace, and a link that stays inside it.
	mustSymlink(t, outside, filepath.Join(root, "escape"))
	mustSymlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "secret.go"))
	mustSymlink(t, filepath.Join(root, "real.go"), filepath.Join(root, "alias.go"))

	outsideBefore := snapshot(t, outside)
	rootBefore := snapshot(t, root)

	for _, tc := range []struct {
		name string
		uri  protocol.DocumentURI
		want string
	}{
		{"through a symlinked directory", uriOf(root, "escape/sub/x.go"), "outside the workspace root"},
		{"a file that is a symlink out", uriOf(root, "secret.go"), "refusing to write through a symlink"},
		{"a file that is a symlink in", uriOf(root, "alias.go"), "refusing to write through a symlink"},
		{"a new file under a symlinked directory", uriOf(root, "escape/new.go"), "outside the workspace root"},
		{"an absolute path elsewhere", uriOf(outside, "secret.txt"), "outside the workspace root"},
		{"a traversal out and back", protocol.URIFromPath(filepath.Join(root, "..", filepath.Base(outside), "secret.txt")), "outside the workspace root"},
		{"the workspace root itself", protocol.URIFromPath(root), "outside the workspace root"},
		{"a directory", uriOf(root, "inner"), "is a directory"},
		{"a non-file URI", protocol.DocumentURI("jdt://contents/java.base/java.lang/String.class"), "not a usable file URI"},
		{"an empty URI", protocol.DocumentURI(""), "empty document URI"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Once as an edit, once as a create: both entry points
			// have to reach the same check.
			msg := stageError(t, &protocol.WorkspaceEdit{
				DocumentChanges: []protocol.DocumentChange{
					docEdit(tc.uri, 0, te(0, 0, 0, 1, "x")),
				},
			}, Options{Root: root})
			wantContains(t, msg, tc.want)

			msg = stageError(t, &protocol.WorkspaceEdit{
				DocumentChanges: []protocol.DocumentChange{
					{CreateFile: &protocol.CreateFile{Kind: "create", URI: tc.uri}},
				},
			}, Options{Root: root})
			wantContains(t, msg, tc.want)

			assertUnchanged(t, root, rootBefore)
			assertUnchanged(t, outside, outsideBefore)
		})
	}
}

// TestSymlinkInsideWorkspaceAllowed: a symlinked directory that stays
// inside the workspace is not an escape, and refusing it would break
// repositories that legitimately use one.
func TestSymlinkInsideWorkspaceAllowed(t *testing.T) {
	root := newTree(t, map[string]string{"pkg/x.go": "package pkg\n"})
	mustSymlink(t, filepath.Join(root, "pkg"), filepath.Join(root, "link"))

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "link/x.go"), 0, te(0, 8, 0, 11, "sub")),
		},
	}, Options{Root: root})
	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := readFile(t, filepath.Join(root, "pkg", "x.go")), "package sub\n"; got != want {
		t.Errorf("content = %q, want %q", got, want)
	}
	if fi, err := os.Lstat(filepath.Join(root, "link")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the symlink was replaced by the write")
	}
}

// TestSymlinkPlantedAfterStaging: the boundary check is repeated at
// commit time, because the window between staging and writing is
// exactly when an attacker would swap a file for a link.
func TestSymlinkPlantedAfterStaging(t *testing.T) {
	outside := newTree(t, map[string]string{"secret.txt": "do not touch\n"})
	root := newTree(t, map[string]string{"a.go": "package a\n"})

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), 0, te(0, 8, 0, 9, "b")),
		},
	}, Options{Root: root})

	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, filepath.Join(outside, "secret.txt"), filepath.Join(root, "a.go"))
	outsideBefore := snapshot(t, outside)

	if _, err := tx.Apply(); err == nil {
		t.Fatal("Apply: expected the planted symlink to be refused")
	} else {
		wantContains(t, err.Error(), "symlink")
	}
	assertUnchanged(t, outside, outsideBefore)
}

func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}
