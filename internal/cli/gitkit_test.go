package cli

import (
	"reflect"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/index"
)

func TestParseDiffReadsHunksStatusesAndRenames(t *testing.T) {
	out := strings.Join([]string{
		"diff --git a.go a.go",
		"index 111..222 100644",
		"--- a.go",
		"+++ a.go",
		"@@ -3 +3,2 @@ func Alpha() {",
		"-x", "+y", "+z",
		"@@ -10,2 +11,0 @@",
		"-p", "-q",
		"diff --git new.go new.go",
		"new file mode 100644",
		"--- /dev/null",
		"+++ new.go",
		"@@ -0,0 +1,3 @@",
		"diff --git gone.go gone.go",
		"deleted file mode 100644",
		"--- gone.go",
		"+++ /dev/null",
		"@@ -1,2 +0,0 @@",
		"diff --git old.go moved.go",
		"similarity index 90%",
		"rename from old.go",
		"rename to moved.go",
		"--- old.go",
		"+++ moved.go",
		"@@ -5 +5 @@",
		"diff --git img.png img.png",
		"Binary files img.png and img.png differ",
		"",
	}, "\n")
	got := parseDiff([]byte(out))
	if len(got) != 5 {
		t.Fatalf("files = %+v", got)
	}
	a := got[0]
	if a.NewPath != "a.go" || a.Status != "M" || len(a.Hunks) != 2 ||
		a.Hunks[0] != (hunk{3, 1, 3, 2}) || a.Hunks[1] != (hunk{10, 2, 11, 0}) {
		t.Errorf("a.go = %+v", a)
	}
	if n := got[1]; n.Status != "A" || n.OldPath != "" || n.NewPath != "new.go" || n.Hunks[0] != (hunk{0, 0, 1, 3}) {
		t.Errorf("new.go = %+v", n)
	}
	if d := got[2]; d.Status != "D" || d.NewPath != "" || d.OldPath != "gone.go" {
		t.Errorf("gone.go = %+v", d)
	}
	if r := got[3]; r.Status != "R" || r.OldPath != "old.go" || r.NewPath != "moved.go" {
		t.Errorf("rename = %+v", r)
	}
	if b := got[4]; !b.Binary || b.NewPath != "img.png" {
		t.Errorf("binary = %+v", b)
	}
}

func TestShiftLineCarriesALineThroughLaterCommits(t *testing.T) {
	// Two lines inserted after line 2; line 10 replaced by three lines; line 20
	// (which is line 24 in the new numbering by then... it is deleted).
	hs := []hunk{{2, 0, 3, 2}, {10, 1, 12, 3}, {20, 1, 23, 0}}
	for _, tc := range []struct {
		in, want int
		lost     bool
	}{
		{1, 1, false},   // before everything
		{2, 2, false},   // the line an insertion follows does not move
		{3, 5, false},   // after the insertion: +2
		{10, 12, true},  // replaced: lost, lands where the replacement starts
		{11, 15, false}, // after the insertion (+2) and the replacement (+2)
		{20, 23, true},  // deleted
		{25, 28, false}, // +2 +2 -1
	} {
		if got, lost := shiftLine(tc.in, hs); got != tc.want || lost != tc.lost {
			t.Errorf("shiftLine(%d) = %d,%v; want %d,%v", tc.in, got, lost, tc.want, tc.lost)
		}
	}
}

func TestShifterAppliesNewestLayerLast(t *testing.T) {
	s := &shifter{}
	s.push([]hunk{{1, 0, 2, 5}}) // the newest change: five lines inserted after line 1
	s.push([]hunk{{1, 0, 2, 1}}) // an older one: one line inserted after line 1
	// A line 4 as it was after the oldest commit is 4+1 after the next, 4+1+5 now.
	if got, lost := s.carry(4); got != 10 || lost {
		t.Errorf("carry(4) = %d,%v; want 10,false", got, lost)
	}
}

func sym(id string, from, to int) index.Symbol {
	return index.Symbol{ID: id, Qualified: idKey(id), Kind: "function", Line: from, EndLine: to}
}

func TestMapFileChange(t *testing.T) {
	newFS := &index.FileSymbols{File: "a.go", Symbols: []index.Symbol{
		sym("a.go::Alpha#function", 3, 5), sym("a.go::Beta#function", 7, 9), sym("a.go::Delta#function", 11, 13),
	}}
	oldSyms := []index.Symbol{
		sym("a.go::Alpha#function", 3, 5), sym("a.go::Beta#function", 7, 9), sym("a.go::Gamma#function", 11, 13),
	}
	f := fileDiff{OldPath: "a.go", NewPath: "a.go", Status: "M", Hunks: []hunk{
		{OldStart: 1, OldCount: 0, NewStart: 2, NewCount: 1}, // a line after the package clause
		{OldStart: 4, OldCount: 1, NewStart: 4, NewCount: 1}, // inside Alpha
		{OldStart: 11, OldCount: 3, NewStart: 11, NewCount: 3},
	}}
	// With the old outline: Delta is new, Gamma is gone, Alpha is modified, and the
	// first hunk belongs to no symbol.
	rows := mapFileChange(f, newFS, oldSyms, true)
	got := map[string]string{}
	for _, r := range rows {
		key := r.ID
		if key == "" {
			key = "(file)"
		}
		got[key] = r.Status
	}
	want := map[string]string{
		"a.go::Alpha#function": "modified", "a.go::Delta#function": "added",
		"a.go::Gamma#function": "removed", "(file)": "modified",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows = %v, want %v\n%+v", got, want, rows)
	}
	for _, r := range rows {
		if r.Evidence != "index+old_outline" {
			t.Errorf("evidence = %q", r.Evidence)
		}
	}

	// Without it, hunk-only: a replaced Delta is "modified" (it is not provably
	// new), Gamma cannot be named, and the answer says how it was made.
	rows = mapFileChange(f, newFS, nil, false)
	for _, r := range rows {
		if r.Status == "removed" {
			t.Errorf("a removal invented without an old outline: %+v", r)
		}
		if r.Evidence != "index+hunks" {
			t.Errorf("evidence = %q", r.Evidence)
		}
	}
}

func TestMapFileChangeAttributesToTheInnermostSymbol(t *testing.T) {
	newFS := &index.FileSymbols{File: "t.go", Symbols: []index.Symbol{
		sym("t.go::T#struct", 1, 10), sym("t.go::T.Method#method", 4, 6),
	}}
	f := fileDiff{OldPath: "t.go", NewPath: "t.go", Status: "M", Hunks: []hunk{{5, 1, 5, 1}}}
	rows := mapFileChange(f, newFS, []index.Symbol{sym("t.go::T#struct", 1, 10), sym("t.go::T.Method#method", 4, 6)}, true)
	if len(rows) != 1 || rows[0].ID != "t.go::T.Method#method" {
		t.Errorf("a hunk inside a method is the method's, not its type's: %+v", rows)
	}
	// A hunk in the struct body outside the method is the struct's.
	f.Hunks = []hunk{{2, 1, 2, 1}}
	rows = mapFileChange(f, newFS, newFS.Symbols, true)
	if len(rows) != 1 || rows[0].ID != "t.go::T#struct" {
		t.Errorf("rows = %+v", rows)
	}
}

func TestMapFileChangeRenamedAndDeletedFiles(t *testing.T) {
	oldSyms := []index.Symbol{sym("old.go::Keep#function", 1, 3), sym("old.go::Drop#function", 5, 7)}
	newFS := &index.FileSymbols{File: "new.go", Symbols: []index.Symbol{sym("new.go::Keep#function", 1, 3)}}
	f := fileDiff{OldPath: "old.go", NewPath: "new.go", Status: "R", Hunks: []hunk{{5, 3, 4, 0}}}
	rows := mapFileChange(f, newFS, oldSyms, true)
	if len(rows) != 1 || rows[0].Status != "removed" || idKey(rows[0].ID) != "Drop#function" || rows[0].OldFile != "old.go" {
		t.Errorf("a rename compares symbols by name, not by path: %+v", rows)
	}
	del := mapFileChange(fileDiff{OldPath: "old.go", Status: "D"}, nil, oldSyms, true)
	if len(del) != 2 || del[0].Status != "removed" {
		t.Errorf("deleted file = %+v", del)
	}
	blind := mapFileChange(fileDiff{OldPath: "old.go", Status: "D"}, nil, nil, false)
	if len(blind) != 1 || blind[0].Kind != "file" || blind[0].Status != "removed" {
		t.Errorf("a deleted file with no old outline is one file-level row: %+v", blind)
	}
	pure := mapFileChange(fileDiff{OldPath: "old.go", NewPath: "new.go", Status: "R"}, newFS, nil, false)
	if len(pure) != 1 || pure[0].Status != "renamed" {
		t.Errorf("pure rename = %+v", pure)
	}
}
