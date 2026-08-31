package docstore

import (
	"path/filepath"
	"strings"
)

// languageIDByExt maps a file extension to an LSP language
// identifier. The list covers the servers PLAN §8 M4 targets plus the
// common companions; anything missing falls back to the extension
// itself, which is what most editors do and what most servers accept.
var languageIDByExt = map[string]string{
	".c":      "c",
	".cc":     "cpp",
	".clj":    "clojure",
	".cpp":    "cpp",
	".cs":     "csharp",
	".css":    "css",
	".cxx":    "cpp",
	".dart":   "dart",
	".ex":     "elixir",
	".exs":    "elixir",
	".go":     "go",
	".h":      "c",
	".hpp":    "cpp",
	".hs":     "haskell",
	".html":   "html",
	".java":   "java",
	".js":     "javascript",
	".json":   "json",
	".jsx":    "javascriptreact",
	".kt":     "kotlin",
	".lua":    "lua",
	".md":     "markdown",
	".ml":     "ocaml",
	".mjs":    "javascript",
	".nix":    "nix",
	".php":    "php",
	".proto":  "proto",
	".py":     "python",
	".pyi":    "python",
	".rb":     "ruby",
	".rs":     "rust",
	".scala":  "scala",
	".scss":   "scss",
	".sh":     "shellscript",
	".sql":    "sql",
	".svelte": "svelte",
	".swift":  "swift",
	".tf":     "terraform",
	".toml":   "toml",
	".ts":     "typescript",
	".tsx":    "typescriptreact",
	".vue":    "vue",
	".yaml":   "yaml",
	".yml":    "yaml",
	".zig":    "zig",
}

// languageIDByBase maps a whole file name to a language identifier,
// for the files whose extension says nothing useful.
var languageIDByBase = map[string]string{
	"BUILD":          "starlark",
	"BUILD.bazel":    "starlark",
	"CMakeLists.txt": "cmake",
	"Dockerfile":     "dockerfile",
	"Makefile":       "makefile",
	"go.mod":         "gomod",
	"go.sum":         "gosum",
	"go.work":        "gowork",
}

// LanguageID reports the LSP language identifier for a path. Unknown
// extensions become the extension itself (".kts" → "kts"), and a file
// with no extension at all becomes "plaintext".
func LanguageID(path string) string {
	base := filepath.Base(path)
	if id, ok := languageIDByBase[base]; ok {
		return id
	}
	ext := filepath.Ext(base)
	if id, ok := languageIDByExt[strings.ToLower(ext)]; ok {
		return id
	}
	if ext == "" || ext == "." {
		return "plaintext"
	}
	return strings.ToLower(strings.TrimPrefix(ext, "."))
}
