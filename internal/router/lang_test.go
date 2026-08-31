package router

import "testing"

func TestLanguageID(t *testing.T) {
	tests := []struct{ path, want string }{
		{"main.go", "go"},
		{"/abs/path/main.go", "go"},
		{"go.mod", "gomod"},
		{"/repo/go.mod", "gomod"},
		{"go.work", "gowork"},
		{"lib.rs", "rust"},
		{"app.py", "python"},
		{"types.pyi", "python"},
		{"Cargo.toml", "toml"},
		{"pyproject.toml", "toml"},
		{"index.ts", "typescript"},
		{"App.tsx", "typescriptreact"},
		{"index.js", "javascript"},
		{"build.sh", "shellscript"},
		{"README.md", "markdown"},
		{"Makefile", "make"},
		{"Dockerfile", "dockerfile"},
		{"CMakeLists.txt", "cmake"},
		{"main.C", "c"}, // extension matching is case-insensitive
		{"noextension", ""},
		{".gitignore", ""},
		{"archive.tar.gz", ""},
		{"", ""},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := LanguageID(tt.path); got != tt.want {
				t.Errorf("LanguageID(%q) = %q, want %q", tt.path, got, tt.want)
			}
		})
	}
}
