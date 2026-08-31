package router

import (
	"path/filepath"
	"strings"
)

// languageByBase maps a whole file name to an LSP language identifier,
// for the files that carry no distinguishing extension.
var languageByBase = map[string]string{
	"go.mod":         "gomod",
	"go.work":        "gowork",
	"go.sum":         "gosum",
	"go.work.sum":    "gosum",
	"Dockerfile":     "dockerfile",
	"Makefile":       "make",
	"GNUmakefile":    "make",
	"CMakeLists.txt": "cmake",
	"Gemfile":        "ruby",
	"Rakefile":       "ruby",
}

// languageByExt maps a file extension (with the dot) to an LSP
// language identifier. Where the LSP specification names an identifier
// ("shellscript", "typescriptreact", …) that name is used, since it is
// what servers match on.
var languageByExt = map[string]string{
	".go":         "go",
	".gotmpl":     "gotmpl",
	".tmpl":       "gotmpl",
	".tpl":        "gotmpl",
	".rs":         "rust",
	".py":         "python",
	".pyi":        "python",
	".pyw":        "python",
	".c":          "c",
	".h":          "c",
	".cc":         "cpp",
	".cpp":        "cpp",
	".cxx":        "cpp",
	".hh":         "cpp",
	".hpp":        "cpp",
	".hxx":        "cpp",
	".m":          "objective-c",
	".mm":         "objective-cpp",
	".ts":         "typescript",
	".mts":        "typescript",
	".cts":        "typescript",
	".tsx":        "typescriptreact",
	".js":         "javascript",
	".mjs":        "javascript",
	".cjs":        "javascript",
	".jsx":        "javascriptreact",
	".java":       "java",
	".kt":         "kotlin",
	".kts":        "kotlin",
	".scala":      "scala",
	".sbt":        "scala",
	".rb":         "ruby",
	".php":        "php",
	".cs":         "csharp",
	".fs":         "fsharp",
	".swift":      "swift",
	".dart":       "dart",
	".lua":        "lua",
	".zig":        "zig",
	".hs":         "haskell",
	".ml":         "ocaml",
	".mli":        "ocaml",
	".ex":         "elixir",
	".exs":        "elixir",
	".erl":        "erlang",
	".nix":        "nix",
	".tf":         "terraform",
	".tfvars":     "terraform",
	".proto":      "proto",
	".sql":        "sql",
	".sh":         "shellscript",
	".bash":       "shellscript",
	".zsh":        "shellscript",
	".fish":       "fish",
	".ps1":        "powershell",
	".vim":        "vim",
	".el":         "lisp",
	".clj":        "clojure",
	".toml":       "toml",
	".json":       "json",
	".jsonc":      "jsonc",
	".yaml":       "yaml",
	".yml":        "yaml",
	".xml":        "xml",
	".html":       "html",
	".htm":        "html",
	".css":        "css",
	".scss":       "scss",
	".less":       "less",
	".vue":        "vue",
	".svelte":     "svelte",
	".md":         "markdown",
	".mdx":        "mdx",
	".rst":        "restructuredtext",
	".tex":        "latex",
	".r":          "r",
	".jl":         "julia",
	".pl":         "perl",
	".pm":         "perl",
	".groovy":     "groovy",
	".gradle":     "groovy",
	".dockerfile": "dockerfile",
}

// LanguageID returns the LSP language identifier for a path, or "" if
// it is unknown. The whole file name is consulted first ("go.mod" is
// "gomod", not "toml"-adjacent), then the extension.
//
// The identifier serves two purposes: matching
// [serverdef.Activation.Languages], and populating languageId in
// textDocument/didOpen. Unknown is not fatal — a definition can still
// claim the file by glob — so this table is a starting corpus, to be
// replaced by the generated one of PLAN §6 item 3 in M4.
func LanguageID(path string) string {
	base := filepath.Base(path)
	if lang, ok := languageByBase[base]; ok {
		return lang
	}
	// A dotfile such as ".gitignore" has no extension, it has a name.
	if strings.HasPrefix(base, ".") && strings.Count(base, ".") == 1 {
		return ""
	}
	if lang, ok := languageByExt[strings.ToLower(filepath.Ext(base))]; ok {
		return lang
	}
	return ""
}
