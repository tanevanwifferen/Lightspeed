package edit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// threeFileEdit is the canonical multi-file refactor: rename a symbol
// declared in one file and used in two others.
func threeFileEdit(t *testing.T) (root string, we *protocol.WorkspaceEdit) {
	t.Helper()
	root = newTree(t, map[string]string{
		"a.go":     "package a\n\nfunc Old() {}\n",
		"b.go":     "package a\n\nfunc useB() { Old() }\n",
		"sub/c.go": "package sub\n\nvar _ = a.Old\n",
	})
	return root, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), 0, te(2, 5, 2, 8, "New")),
			docEdit(uriOf(root, "b.go"), 0, te(2, 14, 2, 17, "New")),
			docEdit(uriOf(root, "sub/c.go"), 0, te(2, 10, 2, 13, "New")),
		},
	}
}

// TestThreeFileEditApplies is the happy path the guarantees are about.
func TestThreeFileEditApplies(t *testing.T) {
	root, we := threeFileEdit(t)
	res, err := Apply(we, Options{Root: root})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	for name, want := range map[string]string{
		"a.go":     "package a\n\nfunc New() {}\n",
		"b.go":     "package a\n\nfunc useB() { New() }\n",
		"sub/c.go": "package sub\n\nvar _ = a.New\n",
	} {
		if got := readFile(t, filepath.Join(root, filepath.FromSlash(name))); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if len(res.Modified) != 3 {
		t.Errorf("modified = %v, want three files", res.Modified)
	}
	assertNoTempFiles(t, root)
}

// TestRollbackWhenTheLastWriteFails is the other half of the M2
// criterion: a failure part-way through the commit leaves the tree
// byte-identical, hashes and all.
func TestRollbackWhenTheLastWriteFails(t *testing.T) {
	root, we := threeFileEdit(t)
	before := snapshot(t, root)

	tx := mustStage(t, we, Options{Root: root})
	failing := filepath.Join(root, "sub", "c.go")
	tx.renameFn = func(oldpath, newpath string) error {
		if newpath == failing {
			return errInjected
		}
		return os.Rename(oldpath, newpath)
	}

	if _, err := tx.Apply(); err == nil {
		t.Fatal("Apply: expected the injected failure")
	} else if !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("Apply: got %v, want the injected failure", err)
	}

	assertUnchanged(t, root, before)
	assertNoTempFiles(t, root)
}

// TestRollbackRestoresDeletesAndCreates: a failed commit has to undo
// every kind of change it had already made, not just the overwrites.
func TestRollbackRestoresDeletesAndCreates(t *testing.T) {
	root := newTree(t, map[string]string{
		"keep.go":   "package a\n",
		"gone.go":   "package gone\n",
		"moved.go":  "package moved\n",
		"other.txt": "unrelated\n",
	})
	before := snapshot(t, root)

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "keep.go"), 0, te(0, 8, 0, 9, "b")),
			createOp(uriOf(root, "deep/nested/new.go"), nil),
			docEdit(uriOf(root, "deep/nested/new.go"), 0, te(0, 0, 0, 0, "package nested\n")),
			renameOp(uriOf(root, "moved.go"), uriOf(root, "elsewhere.go"), nil),
			deleteOp(uriOf(root, "gone.go"), nil),
		},
	}, Options{Root: root})

	// Fail the very last thing the commit does: the move-aside of the
	// deleted file, after every write has already landed.
	tx.renameFn = func(oldpath, newpath string) error {
		if oldpath == filepath.Join(root, "gone.go") {
			return errInjected
		}
		return os.Rename(oldpath, newpath)
	}

	if _, err := tx.Apply(); err == nil {
		t.Fatal("Apply: expected the injected failure")
	}
	assertUnchanged(t, root, before)
	assertNoTempFiles(t, root)
	if exists(filepath.Join(root, "deep")) {
		t.Error("a directory created for the commit survived the rollback")
	}
}

// TestFailureBeforeAnyWrite: an unwritable directory is discovered
// while staging temp files, before a single rename has happened.
func TestFailureBeforeAnyWrite(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are advisory")
	}
	root, we := threeFileEdit(t)
	before := snapshot(t, root)

	sub := filepath.Join(root, "sub")
	if err := os.Chmod(sub, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o755) })

	tx := mustStage(t, we, Options{Root: root})
	if _, err := tx.Apply(); err == nil {
		t.Fatal("Apply: expected a permission failure")
	} else {
		wantContains(t, err.Error(), "c.go")
	}
	assertUnchanged(t, root, before)
	assertNoTempFiles(t, root)
}

// TestFileChangedAfterStaging: the edit's offsets describe the bytes
// staging read. If the file has moved on, applying them would corrupt
// it at positions that look entirely plausible.
func TestFileChangedAfterStaging(t *testing.T) {
	root, we := threeFileEdit(t)
	tx := mustStage(t, we, Options{Root: root})

	if err := os.WriteFile(filepath.Join(root, "b.go"), []byte("package a\n// rewritten\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)

	if _, err := tx.Apply(); err == nil {
		t.Fatal("Apply: expected the concurrent change to be refused")
	} else {
		wantContains(t, err.Error(), "b.go", "changed on disk", "nothing was written")
	}
	assertUnchanged(t, root, before)
}

// TestFileDeletedAfterStaging and its inverse: the disk has to look
// the way the transaction expects, in both directions.
func TestFileDeletedAfterStaging(t *testing.T) {
	root, we := threeFileEdit(t)
	tx := mustStage(t, we, Options{Root: root})
	if err := os.Remove(filepath.Join(root, "a.go")); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)

	if _, err := tx.Apply(); err == nil {
		t.Fatal("Apply: expected the deletion to be refused")
	} else {
		wantContains(t, err.Error(), "a.go", "deleted after the edit was computed")
	}
	assertUnchanged(t, root, before)
}

func TestFileCreatedAfterStaging(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{createOp(uriOf(root, "new.go"), nil)},
	}, Options{Root: root})

	if err := os.WriteFile(filepath.Join(root, "new.go"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, root)

	if _, err := tx.Apply(); err == nil {
		t.Fatal("Apply: expected the surprise file to be refused")
	} else {
		wantContains(t, err.Error(), "new.go", "created after the edit was computed")
	}
	assertUnchanged(t, root, before)
}

// TestApplyIsOnce: the staged originals describe a disk state that no
// longer holds once they have been written.
func TestApplyIsOnce(t *testing.T) {
	root, we := threeFileEdit(t)
	tx := mustStage(t, we, Options{Root: root})
	if _, err := tx.Apply(); err != nil {
		t.Fatalf("first Apply: %v", err)
	}
	if _, err := tx.Apply(); err == nil {
		t.Fatal("second Apply: expected an error")
	}
}

// TestOverlayContentMustMatchDisk: a Source may report content the
// disk does not have — an editor buffer, say — and committing that
// would silently discard whatever is really in the file.
func TestOverlayContentMustMatchDisk(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	path := filepath.Join(root, "a.go")
	src := memSource{docs: map[string]Document{
		path: {Content: []byte("package unsaved\n"), Version: 2},
	}}
	before := snapshot(t, root)

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "a.go"), 2, te(0, 8, 0, 15, "b")),
		},
	}, Options{Root: root, Source: src})

	if _, err := tx.Apply(); err == nil {
		t.Fatal("Apply: expected the disk mismatch to be refused")
	} else {
		wantContains(t, err.Error(), "a.go", "changed on disk")
	}
	assertUnchanged(t, root, before)
}

// assertNoTempFiles fails if a commit left its scaffolding behind.
func assertNoTempFiles(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(d.Name(), ".lightspeed-") {
			t.Errorf("temp file left behind: %s", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
}

// TestRollbackFailureIsLoud: the one case where the tree is left in a
// state nobody asked for must not be reported as the original error
// alone. The journal is exercised directly because a rollback that
// fails needs a filesystem that fails, and the point being tested is
// the reporting, not the syscall.
func TestRollbackFailureIsLoud(t *testing.T) {
	j := &journal{rename: os.Rename}
	j.undo = append(j.undo,
		func() error { return errInjected },
		func() error { return nil },
	)

	err := j.rollback(errCause)
	if err == nil {
		t.Fatal("rollback reported success")
	}
	wantContains(t, err.Error(), "the original problem", "left the workspace inconsistent", "injected failure")
}

// TestRollbackReportsTheOriginalCause: when the undo works, the
// caller hears about what actually went wrong and nothing else.
func TestRollbackReportsTheOriginalCause(t *testing.T) {
	j := &journal{rename: os.Rename}
	var undone int
	j.undo = append(j.undo,
		func() error { undone++; return nil },
		func() error { undone++; return nil },
	)
	if err := j.rollback(errCause); err != errCause {
		t.Errorf("rollback returned %v, want the cause unchanged", err)
	}
	if undone != 2 {
		t.Errorf("ran %d undo steps, want 2", undone)
	}
}

// TestWriteAtomicRestoresContent covers the rollback's write path,
// which never runs in a passing commit.
func TestWriteAtomicRestoresContent(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "new\n"})
	path := filepath.Join(root, "a.go")
	if err := writeAtomic(path, []byte("original\n"), 0o600); err != nil {
		t.Fatalf("writeAtomic: %v", err)
	}
	if got := readFile(t, path); got != "original\n" {
		t.Errorf("content = %q", got)
	}
	if got := mode(t, path); got != 0o600 {
		t.Errorf("mode = %04o, want 0600", got)
	}
	assertNoTempFiles(t, root)
}

// TestRootIsReported: the boundary a transaction was staged against is
// part of what a caller renders.
func TestRootIsReported(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	if got := mustStage(t, nil, Options{Root: root}).Root(); got != root {
		t.Errorf("Root() = %q, want %q", got, root)
	}
}
