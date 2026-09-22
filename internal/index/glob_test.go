package index

import (
	"reflect"
	"testing"
)

// The cases are those of `search_text --glob` (internal/cli/search_test.go):
// the same rules over the same files.
func TestGlobSemantics(t *testing.T) {
	files := []string{"README.md", "go.mod", "internal/a.go", "internal/a_test.go", "internal/gen/g.go", "main.go", "web/app.ts"}
	cases := map[string]struct {
		globs []string
		want  []string
	}{
		"none":                {nil, files},
		"extension any depth": {[]string{"*.go"}, []string{"internal/a.go", "internal/a_test.go", "internal/gen/g.go", "main.go"}},
		"two includes":        {[]string{"*.md", "*.ts"}, []string{"README.md", "web/app.ts"}},
		"include and exclude": {[]string{"*.go", "!*_test.go", "!internal/gen"}, []string{"internal/a.go", "main.go"}},
		"only excludes":       {[]string{"!**/*.go", "!go.mod"}, []string{"README.md", "web/app.ts"}},
		"a directory":         {[]string{"internal"}, []string{"internal/a.go", "internal/a_test.go", "internal/gen/g.go"}},
		"anchored":            {[]string{"internal/*.go"}, []string{"internal/a.go", "internal/a_test.go"}},
		"braces":              {[]string{"**/*.{md,ts}"}, []string{"README.md", "web/app.ts"}},
		"dot slash":           {[]string{"./internal/gen"}, []string{"internal/gen/g.go"}},
	}
	for name, c := range cases {
		gs, err := CompileGlobs("/ws", c.globs)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var got []string
		for _, f := range files {
			if gs.Allows(f) {
				got = append(got, f)
			}
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %v, want %v", name, got, c.want)
		}
	}
}

func TestGlobErrorsAndNil(t *testing.T) {
	if _, err := CompileGlobs("/ws", []string{"["}); err == nil {
		t.Error("a malformed glob compiled")
	}
	if _, err := CompileGlobs("/ws", []string{"!"}); err == nil {
		t.Error("an empty exclude compiled")
	}
	if _, err := CompileGlobs("/ws", []string{""}); err == nil {
		t.Error("an empty pattern compiled")
	}
	var gs *GlobSet
	if !gs.Allows("anything") {
		t.Error("a nil GlobSet must allow everything")
	}
}

func TestGlobAbsolutePattern(t *testing.T) {
	gs, err := CompileGlobs("/ws", []string{"/ws/internal/**"})
	if err != nil {
		t.Fatal(err)
	}
	if !gs.Allows("internal/a.go") || gs.Allows("web/app.ts") {
		t.Errorf("absolute pattern matched against the root-joined path: internal=%v web=%v",
			gs.Allows("internal/a.go"), gs.Allows("web/app.ts"))
	}
}
