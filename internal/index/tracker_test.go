package index

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A file created in a directory that held no listed file changes only that
// directory's mtime. The tracker must watch such directories too, or the new
// file is never listed (and so never indexed, nor its server told about it).
// Every mtime here is aged past the racy window, so the young-directory rule
// that forces a relist cannot be what makes the test pass.
func testFileAddedUnderAnEmptyDirectory(t *testing.T, root string) {
	t.Helper()
	ctx := context.Background()
	put(t, root, "top.go", "package top\n")
	for _, d := range []string{"old/sub", "old/other", "brandnew/x/y"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	age := func() {
		for _, d := range []string{"old/sub", "old/other", "old", "brandnew/x/y", "brandnew/x", "brandnew", "."} {
			touchDir(t, root, d)
		}
	}
	age()

	tr := NewTracker(root)
	s1, err := tr.Scan(ctx)
	if err != nil || len(s1.Files) != 1 {
		t.Fatalf("first scan: %v %v", s1.Files, err)
	}
	s2, _ := tr.Scan(ctx)
	if s2.Listed {
		t.Fatal("an unchanged tree was listed again: the test would not exercise the fast path")
	}

	// The first file of a directory that was empty when it was last scanned:
	// the directory's mtime moves to now, which differs from the aged one the
	// snapshot recorded, and nothing else in the workspace moves.
	put(t, root, "old/sub/f.go", "package sub\n")
	s3, _ := tr.Scan(ctx)
	if d := Diff(s2, s3); len(d) != 1 || d[0].Path != "old/sub/f.go" || d[0].Kind != Created {
		t.Fatalf("a file created in an empty directory was missed: %v", d)
	}

	// The same, three levels down, where neither the root nor the parents move.
	age()
	if s, _ := tr.Scan(ctx); !s.Listed {
		t.Fatal("aging the directories should have been noticed")
	}
	if s, _ := tr.Scan(ctx); s.Listed {
		t.Fatal("an unchanged tree was listed again")
	}
	put(t, root, "brandnew/x/y/g.go", "package y\n")
	s4, _ := tr.Scan(ctx)
	if d := Diff(s3, s4); len(d) != 1 || d[0].Path != "brandnew/x/y/g.go" || d[0].Kind != Created {
		t.Fatalf("a file created in a nested empty directory was missed: %v", d)
	}

	// A directory made after the first scan, left alone, then filled.
	if err := os.MkdirAll(filepath.Join(root, "later", "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	s5, _ := tr.Scan(ctx)
	if len(s5.Files) != len(s4.Files) {
		t.Fatalf("an empty new directory changed the file list: %v", s5.Files)
	}
	age()
	touchDir(t, root, "later/deep")
	touchDir(t, root, "later")
	tr.Scan(ctx) // records the aged mtimes
	s5, _ = tr.Scan(ctx)
	if s5.Listed {
		t.Fatal("an unchanged tree was listed again")
	}
	put(t, root, "later/deep/h.go", "package deep\n")
	s6, _ := tr.Scan(ctx)
	if d := Diff(s5, s6); len(d) != 1 || d[0].Path != "later/deep/h.go" || d[0].Kind != Created {
		t.Fatalf("a file created in a directory made after the first scan was missed: %v", d)
	}
}

func TestTrackerSeesAFileAddedUnderAnEmptyDirectory(t *testing.T) {
	testFileAddedUnderAnEmptyDirectory(t, newRoot(t))
}

func TestTrackerSeesAFileAddedUnderAnEmptyDirectoryInARepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := newRoot(t)
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("git init: %v: %s", err, out)
	}
	testFileAddedUnderAnEmptyDirectory(t, root)
}

func TestTrackerDoesNotWatchGitOrBuildDirectories(t *testing.T) {
	root := newRoot(t)
	put(t, root, "a.go", "package a\n")
	put(t, root, ".github/workflows/ci.yml", "on: push\n")
	for _, d := range []string{".git/objects", "node_modules/pkg", "docs"} {
		if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(d)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dirs, capped := watchDirs(context.Background(), root, []string{"a.go", ".github/workflows/ci.yml"})
	if capped {
		t.Fatal("capped")
	}
	got := map[string]bool{}
	for _, d := range dirs {
		got[d] = true
	}
	for _, want := range []string{".", "docs", ".github", ".github/workflows"} {
		if !got[want] {
			t.Errorf("%q is not watched: %v", want, dirs)
		}
	}
	for _, not := range []string{".git", ".git/objects", "node_modules", "node_modules/pkg"} {
		if got[not] {
			t.Errorf("%q is watched: %v", not, dirs)
		}
	}
}
