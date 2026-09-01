package edit

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

func createOp(uri protocol.DocumentURI, opts *protocol.CreateFileOptions) protocol.DocumentChange {
	return protocol.DocumentChange{CreateFile: &protocol.CreateFile{Kind: "create", URI: uri, Options: opts}}
}

func renameOp(old, nw protocol.DocumentURI, opts *protocol.RenameFileOptions) protocol.DocumentChange {
	return protocol.DocumentChange{RenameFile: &protocol.RenameFile{
		Kind: "rename", OldURI: old, NewURI: nw, Options: opts,
	}}
}

func deleteOp(uri protocol.DocumentURI, opts *protocol.DeleteFileOptions) protocol.DocumentChange {
	return protocol.DocumentChange{DeleteFile: &protocol.DeleteFile{Kind: "delete", URI: uri, Options: opts}}
}

// TestResourceOperationOrder: documentChanges is a sequence, not a
// set. Each operation sees the result of the ones before it, and the
// same operations in a different order mean something different.
func TestResourceOperationOrder(t *testing.T) {
	root := newTree(t, map[string]string{
		"old.go":  "package a\n\nfunc Old() {}\n",
		"gone.go": "package a\n",
		"keep.go": "package a\n",
	})

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			// create, then fill the file that was just created
			createOp(uriOf(root, "sub/new.go"), nil),
			docEdit(uriOf(root, "sub/new.go"), 0, te(0, 0, 0, 0, "package sub\n")),
			// rename, then edit the file at its new name
			renameOp(uriOf(root, "old.go"), uriOf(root, "moved.go"), nil),
			docEdit(uriOf(root, "moved.go"), 0, te(2, 5, 2, 8, "New")),
			deleteOp(uriOf(root, "gone.go"), nil),
		},
	}, Options{Root: root})

	res, err := tx.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got, want := readFile(t, filepath.Join(root, "sub/new.go")), "package sub\n"; got != want {
		t.Errorf("sub/new.go = %q, want %q", got, want)
	}
	if got, want := readFile(t, filepath.Join(root, "moved.go")), "package a\n\nfunc New() {}\n"; got != want {
		t.Errorf("moved.go = %q, want %q", got, want)
	}
	if exists(filepath.Join(root, "old.go")) {
		t.Error("old.go still exists after the rename")
	}
	if exists(filepath.Join(root, "gone.go")) {
		t.Error("gone.go still exists after the delete")
	}
	if got, want := readFile(t, filepath.Join(root, "keep.go")), "package a\n"; got != want {
		t.Errorf("keep.go was touched: %q", got)
	}

	want := &Result{
		Created:  []string{filepath.Join(root, "sub/new.go")},
		Deleted:  []string{filepath.Join(root, "gone.go")},
		Renamed:  []Rename{{From: filepath.Join(root, "old.go"), To: filepath.Join(root, "moved.go")}},
		Modified: nil,
	}
	if !reflect.DeepEqual(res, want) {
		t.Errorf("result = %+v, want %+v", res, want)
	}
}

// TestEditThenRenameKeepsEdits: editing a file and then moving it is
// one operation on the content, not two on the disk.
func TestEditThenRenameKeepsEdits(t *testing.T) {
	root := newTree(t, map[string]string{"old.go": "package a\n"})
	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "old.go"), 0, te(0, 8, 0, 9, "b")),
			renameOp(uriOf(root, "old.go"), uriOf(root, "new.go"), nil),
		},
	}, Options{Root: root})
	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if got, want := readFile(t, filepath.Join(root, "new.go")), "package b\n"; got != want {
		t.Errorf("new.go = %q, want %q", got, want)
	}
	if exists(filepath.Join(root, "old.go")) {
		t.Error("old.go survived the rename")
	}
}

// TestRenameChainCollapses: A→B→C touches the disk once, from A to C.
func TestRenameChainCollapses(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			renameOp(uriOf(root, "a.go"), uriOf(root, "b.go"), nil),
			renameOp(uriOf(root, "b.go"), uriOf(root, "c.go"), nil),
		},
	}, Options{Root: root})

	res, err := tx.Apply()
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := []Rename{{From: filepath.Join(root, "a.go"), To: filepath.Join(root, "c.go")}}
	if !reflect.DeepEqual(res.Renamed, want) {
		t.Errorf("renamed = %+v, want %+v", res.Renamed, want)
	}
	if exists(filepath.Join(root, "b.go")) {
		t.Error("the waypoint b.go was written to disk")
	}
	if got, want := readFile(t, filepath.Join(root, "c.go")), "package a\n"; got != want {
		t.Errorf("c.go = %q, want %q", got, want)
	}
}

// TestClobberRefused: overwriting a file that the edit did not say to
// overwrite is how a refactor eats an unrelated file.
func TestClobberRefused(t *testing.T) {
	files := map[string]string{"a.go": "package a\n", "b.go": "package b\n"}

	t.Run("create over an existing file", func(t *testing.T) {
		root := newTree(t, files)
		before := snapshot(t, root)
		msg := stageError(t, &protocol.WorkspaceEdit{
			DocumentChanges: []protocol.DocumentChange{createOp(uriOf(root, "a.go"), nil)},
		}, Options{Root: root})
		wantContains(t, msg, "a.go", "would overwrite", "overwrite nor ignoreIfExists")
		assertUnchanged(t, root, before)
	})

	t.Run("rename over an existing file", func(t *testing.T) {
		root := newTree(t, files)
		before := snapshot(t, root)
		msg := stageError(t, &protocol.WorkspaceEdit{
			DocumentChanges: []protocol.DocumentChange{
				renameOp(uriOf(root, "a.go"), uriOf(root, "b.go"), nil),
			},
		}, Options{Root: root})
		wantContains(t, msg, "b.go", "would overwrite")
		assertUnchanged(t, root, before)
	})

	t.Run("create with overwrite truncates", func(t *testing.T) {
		root := newTree(t, files)
		tx := mustStage(t, &protocol.WorkspaceEdit{
			DocumentChanges: []protocol.DocumentChange{
				createOp(uriOf(root, "a.go"), &protocol.CreateFileOptions{Overwrite: true}),
			},
		}, Options{Root: root})
		if _, err := tx.Apply(); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readFile(t, filepath.Join(root, "a.go")); got != "" {
			t.Errorf("a.go = %q, want empty", got)
		}
	})

	t.Run("create with ignoreIfExists is a no-op", func(t *testing.T) {
		root := newTree(t, files)
		before := snapshot(t, root)
		tx := mustStage(t, &protocol.WorkspaceEdit{
			DocumentChanges: []protocol.DocumentChange{
				createOp(uriOf(root, "a.go"), &protocol.CreateFileOptions{IgnoreIfExists: true}),
			},
		}, Options{Root: root})
		if _, err := tx.Apply(); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		assertUnchanged(t, root, before)
		if len(tx.Warnings()) == 0 {
			t.Error("a skipped operation was not reported")
		}
	})

	t.Run("overwrite wins over ignoreIfExists", func(t *testing.T) {
		root := newTree(t, files)
		tx := mustStage(t, &protocol.WorkspaceEdit{
			DocumentChanges: []protocol.DocumentChange{
				createOp(uriOf(root, "a.go"), &protocol.CreateFileOptions{Overwrite: true, IgnoreIfExists: true}),
			},
		}, Options{Root: root})
		if _, err := tx.Apply(); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got := readFile(t, filepath.Join(root, "a.go")); got != "" {
			t.Errorf("a.go = %q, want empty", got)
		}
	})

	t.Run("rename with overwrite reports a delete and a modify", func(t *testing.T) {
		root := newTree(t, files)
		tx := mustStage(t, &protocol.WorkspaceEdit{
			DocumentChanges: []protocol.DocumentChange{
				renameOp(uriOf(root, "a.go"), uriOf(root, "b.go"), &protocol.RenameFileOptions{Overwrite: true}),
			},
		}, Options{Root: root})
		res, err := tx.Apply()
		if err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got, want := readFile(t, filepath.Join(root, "b.go")), "package a\n"; got != want {
			t.Errorf("b.go = %q, want %q", got, want)
		}
		if exists(filepath.Join(root, "a.go")) {
			t.Error("a.go survived")
		}
		// Not reported as a rename: the destination already existed,
		// so on disk this is a delete plus an overwrite, and a patch
		// of it has to say so to apply.
		if len(res.Renamed) != 0 {
			t.Errorf("renamed = %+v, want none", res.Renamed)
		}
		if !reflect.DeepEqual(res.Deleted, []string{filepath.Join(root, "a.go")}) {
			t.Errorf("deleted = %v", res.Deleted)
		}
		if !reflect.DeepEqual(res.Modified, []string{filepath.Join(root, "b.go")}) {
			t.Errorf("modified = %v", res.Modified)
		}
	})
}

// TestMissingOperandRefused: renaming or deleting something that is
// not there means the server is describing a workspace we do not have.
func TestMissingOperandRefused(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	before := snapshot(t, root)

	t.Run("rename", func(t *testing.T) {
		msg := stageError(t, &protocol.WorkspaceEdit{
			DocumentChanges: []protocol.DocumentChange{
				renameOp(uriOf(root, "nope.go"), uriOf(root, "b.go"), nil),
			},
		}, Options{Root: root})
		wantContains(t, msg, "nope.go", "does not exist")
	})

	t.Run("delete", func(t *testing.T) {
		msg := stageError(t, &protocol.WorkspaceEdit{
			DocumentChanges: []protocol.DocumentChange{deleteOp(uriOf(root, "nope.go"), nil)},
		}, Options{Root: root})
		wantContains(t, msg, "nope.go", "does not exist")
	})

	t.Run("delete with ignoreIfNotExists", func(t *testing.T) {
		tx := mustStage(t, &protocol.WorkspaceEdit{
			DocumentChanges: []protocol.DocumentChange{
				deleteOp(uriOf(root, "nope.go"), &protocol.DeleteFileOptions{IgnoreIfNotExists: true}),
			},
		}, Options{Root: root})
		if !tx.Empty() {
			t.Error("a skipped delete is not empty")
		}
	})

	assertUnchanged(t, root, before)
}

// TestCreateThenDeleteIsNothing: an edit set that undoes itself must
// not leave a file behind, and must not touch the disk at all.
func TestCreateThenDeleteIsNothing(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	before := snapshot(t, root)

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			createOp(uriOf(root, "tmp.go"), nil),
			docEdit(uriOf(root, "tmp.go"), 0, te(0, 0, 0, 0, "package tmp\n")),
			deleteOp(uriOf(root, "tmp.go"), nil),
		},
	}, Options{Root: root})
	if !tx.Empty() {
		t.Error("create-then-delete is not empty")
	}
	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	assertUnchanged(t, root, before)
}

// TestEditOfDeletedFileRefused: order matters, and an edit set that
// contradicts itself is a bug in the server, not a decision for us.
func TestEditOfDeletedFileRefused(t *testing.T) {
	root := newTree(t, map[string]string{"a.go": "package a\n"})
	before := snapshot(t, root)
	msg := stageError(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			deleteOp(uriOf(root, "a.go"), nil),
			docEdit(uriOf(root, "a.go"), 0, te(0, 8, 0, 9, "b")),
		},
	}, Options{Root: root})
	wantContains(t, msg, "a.go", "does not exist")
	assertUnchanged(t, root, before)
}

// TestFileModePreserved: an executable file that gets edited is still
// executable, and a new file is not.
func TestFileModePreserved(t *testing.T) {
	root := newTree(t, map[string]string{"run.sh": "#!/bin/sh\necho old\n"})
	script := filepath.Join(root, "run.sh")
	if err := chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	tx := mustStage(t, &protocol.WorkspaceEdit{
		DocumentChanges: []protocol.DocumentChange{
			docEdit(uriOf(root, "run.sh"), 0, te(1, 5, 1, 8, "new")),
			createOp(uriOf(root, "plain.txt"), nil),
		},
	}, Options{Root: root})
	if _, err := tx.Apply(); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := mode(t, script); got != 0o755 {
		t.Errorf("run.sh mode = %04o, want 0755", got)
	}
	if got := mode(t, filepath.Join(root, "plain.txt")); got != defaultFileMode {
		t.Errorf("plain.txt mode = %04o, want %04o", got, defaultFileMode)
	}
}
