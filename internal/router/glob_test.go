package router

import (
	"strings"
	"testing"
)

func TestGlobMatch(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		want    bool
	}{
		// "**" spans zero or more segments, so the conventional
		// "**/*.go" also claims a file at the root.
		{"**/*.go", "main.go", true},
		{"**/*.go", "pkg/main.go", true},
		{"**/*.go", "a/b/c/main.go", true},
		{"**/*.go", "main.rs", false},
		{"**/*.go", "go/main.rs", false},
		{"**/go.mod", "go.mod", true},
		{"**/go.mod", "nested/go.mod", true},
		{"**/go.mod", "go.mod/x", false},

		// A single "*" stays inside one segment.
		{"*.go", "main.go", true},
		{"*.go", "pkg/main.go", false},
		{"src/*.rs", "src/lib.rs", true},
		{"src/*.rs", "src/a/lib.rs", false},

		// Anchored patterns do not float: this is what keeps a
		// definition from claiming files in a nested project.
		{"src/**/*.rs", "src/lib.rs", true},
		{"src/**/*.rs", "src/a/b/lib.rs", true},
		{"src/**/*.rs", "nested/src/lib.rs", false},
		{"src/**/*.rs", "lib.rs", false},

		// "**" in the middle, and runs of it.
		{"a/**/b/*.c", "a/b/x.c", true},
		{"a/**/b/*.c", "a/z/b/x.c", true},
		{"a/**/b/*.c", "a/z/y/b/x.c", true},
		{"a/**/b/*.c", "a/b/z/x.c", false},
		{"a/**/**/x", "a/b/c/x", true},
		{"a/**", "a/b/c", true},
		{"a/**", "a", true}, // a trailing "**" matches zero segments too
		{"**", "anything/at/all", true},

		{"?.go", "a.go", true},
		{"?.go", "ab.go", false},
		{"[ab].go", "a.go", true},
		{"[ab].go", "c.go", false},

		{"**/*.{ts,tsx}", "src/a.ts", true},
		{"**/*.{ts,tsx}", "src/a.tsx", true},
		{"**/*.{ts,tsx}", "src/a.js", false},
		{"{src,lib}/**/*.py", "lib/a/b.py", true},
		{"{src,lib}/**/*.py", "test/a.py", false},
		{"a{b,c{d,e}}f", "acdf", true},
		{"a{b,c{d,e}}f", "abf", true},
		{"a{b,c{d,e}}f", "axf", false},

		// Matching is case-sensitive, as the filesystems we target are.
		{"**/*.go", "MAIN.GO", false},

		// Leading "./" and "/" on the subject are noise.
		{"**/*.go", "./main.go", true},
		{"/abs/**/*.go", "/abs/pkg/x.go", true},
		{"/abs/**/*.go", "/other/pkg/x.go", false},
		{"**/*.go", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.pattern+" vs "+tt.path, func(t *testing.T) {
			g, err := CompileGlob(tt.pattern)
			if err != nil {
				t.Fatalf("CompileGlob(%q) = %v", tt.pattern, err)
			}
			if got := g.Match(tt.path); got != tt.want {
				t.Errorf("CompileGlob(%q).Match(%q) = %t, want %t", tt.pattern, tt.path, got, tt.want)
			}
		})
	}
}

func TestGlobAbsolute(t *testing.T) {
	abs, err := CompileGlob("/etc/**/*.conf")
	if err != nil {
		t.Fatalf("CompileGlob() = %v", err)
	}
	if !abs.Absolute() {
		t.Error("a pattern starting with / is not reported as absolute")
	}
	rel, err := CompileGlob("**/*.conf")
	if err != nil {
		t.Fatalf("CompileGlob() = %v", err)
	}
	if rel.Absolute() {
		t.Error("a pattern not starting with / is reported as absolute")
	}
	if got, want := rel.Pattern(), "**/*.conf"; got != want {
		t.Errorf("Pattern() = %q, want %q", got, want)
	}
}

func TestCompileGlobErrors(t *testing.T) {
	tests := []struct{ pattern, want string }{
		{"", "pattern is empty"},
		{"**/*.[go", "bad segment"},
		{"a{b", "unmatched '{'"},
		{"ab}", "unmatched '}'"},
		{strings.Repeat("{a,b}", 9), "exceeds"},
	}
	for _, tt := range tests {
		t.Run(tt.pattern, func(t *testing.T) {
			g, err := CompileGlob(tt.pattern)
			if err == nil {
				t.Fatalf("CompileGlob(%q) = %+v, want an error mentioning %q", tt.pattern, g, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("CompileGlob(%q) error = %q, want it to mention %q", tt.pattern, err, tt.want)
			}
		})
	}
}
