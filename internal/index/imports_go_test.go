package index

import (
	"os"
	"path/filepath"
	"testing"
)

func TestExtractGo(t *testing.T) {
	src := `// Package p mentions "not/an/import" in a comment.
package p

import "fmt"
import (
	"os"
	str "strings"
	_ "embed"
	. "net/http"
	"github.com/x/y/z"
)

/* import "block/comment" */
const s = "import \"in/string\""

import "C"
`
	refs := impExtract(t, "go", "p.go", src)
	impEqual(t, "go", impSpecs(refs), []string{"fmt", "os", "strings", "embed", "net/http", "github.com/x/y/z"})
	if refs[0].Line != 4 || refs[1].Line != 6 {
		t.Errorf("lines = %d, %d", refs[0].Line, refs[1].Line)
	}
}

func TestExtractGoToleratesSyntaxErrorAfterImports(t *testing.T) {
	refs := impExtract(t, "go", "p.go", "package p\nimport \"a\"\nfunc {{{ broken")
	impEqual(t, "go", impSpecs(refs), []string{"a"})
}

func TestExtractUncoveredLanguage(t *testing.T) {
	if refs, covered := ExtractImports("zig", "a.zig", []byte(`@import("std")`)); covered || refs != nil {
		t.Errorf("zig: covered=%v refs=%v; an unknown language must be uncovered, not import-free", covered, refs)
	}
	if ExtractorFor("zig") != nil {
		t.Error("zig has an extractor")
	}
	langs := ExtractedLanguages()
	want := []string{"c", "cpp", "go", "javascript", "javascriptreact", "lua", "python", "rust", "typescript", "typescriptreact"}
	impEqual(t, "languages", langs, want)
}

// The one test that goes through real files: go.mod on disk, read through the
// filesystem workspace, resolving a real import.
func TestGoModuleOnDisk(t *testing.T) {
	root := t.TempDir()
	write := func(rel, s string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/app\n\ngo 1.22\n")
	write("main.go", "package main\nimport (\"fmt\"; \"example.com/app/lib\"; \"golang.org/x/tools/go/ast\")\n")
	write("lib/lib.go", "package lib\n")

	ws := NewFSWorkspace(root, []string{"main.go", "lib/lib.go", "go.mod"})
	main, err := os.ReadFile(filepath.Join(root, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	refs, _ := ExtractImports("go", "main.go", main)
	f := &File{Path: "main.go", Language: "go", HasImports: true, Imports: refs}
	lib := &File{Path: "lib/lib.go", Language: "go", HasImports: true}
	g := BuildGraph([]*File{f, lib}, ws)

	got := g.ImportsOf("main.go")
	var summary []string
	for _, ri := range got.Imports {
		summary = append(summary, ri.Spec+"="+ri.Category)
	}
	impEqual(t, "resolved", summary, []string{"fmt=stdlib", "example.com/app/lib=workspace", "golang.org/x/tools/go/ast=third-party"})
	if got.Imports[1].Targets[0] != "lib" {
		t.Errorf("targets = %v, want the package directory lib", got.Imports[1].Targets)
	}
}
