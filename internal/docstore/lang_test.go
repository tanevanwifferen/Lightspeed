package docstore

import "testing"

// TestLanguageID: the language id is what tells a server which
// grammar to use; getting it wrong makes a server answer nothing at
// all, which the readiness gate would then have to distinguish from
// indexing.
func TestLanguageID(t *testing.T) {
	cases := map[string]string{
		"/ws/main.go":          "go",
		"/ws/go.mod":           "gomod",
		"/ws/go.sum":           "gosum",
		"/ws/go.work":          "gowork",
		"/ws/src/lib.rs":       "rust",
		"/ws/app.tsx":          "typescriptreact",
		"/ws/app.ts":           "typescript",
		"/ws/main.PY":          "python",
		"/ws/header.h":         "c",
		"/ws/impl.cpp":         "cpp",
		"/ws/init.lua":         "lua",
		"/ws/Dockerfile":       "dockerfile",
		"/ws/Makefile":         "makefile",
		"/ws/CMakeLists.txt":   "cmake",
		"/ws/deploy.yml":       "yaml",
		"/ws/unknown.kts":      "kts",
		"/ws/README":           "plaintext",
		"/ws/.gitignore":       "gitignore",
		"/ws/nested/dir/x.zig": "zig",
	}
	for path, want := range cases {
		if got := LanguageID(path); got != want {
			t.Errorf("LanguageID(%q) = %q, want %q", path, got, want)
		}
	}
}
