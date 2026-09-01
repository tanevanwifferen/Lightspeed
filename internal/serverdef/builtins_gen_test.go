package serverdef

import (
	"slices"
	"strings"
	"testing"
)

// TestBuiltinsCoverM4 pins PLAN §8 M4's definition of done: these six
// servers, and only these six, answer on a clean machine with no
// hand-written configuration. The command, the markers and the globs
// have to be plausible here, because the milestone's real test —
// running all six — cannot be hermetic.
func TestBuiltinsCoverM4(t *testing.T) {
	want := []string{"clangd", "gopls", "lua-ls", "pyright", "rust-analyzer", "vtsls"}

	got := BuiltinNames()
	sorted := slices.Clone(got)
	slices.Sort(sorted)
	if !slices.Equal(sorted, want) {
		t.Fatalf("built-in servers = %v, want exactly %v", sorted, want)
	}
	// The router treats load order as preference order, and gopls is
	// the reference server, so it comes first.
	if got[0] != "gopls" {
		t.Errorf("BuiltinNames()[0] = %q, want gopls first", got[0])
	}
}

// TestBuiltinsArePlausible checks each generated definition against
// what a working server actually needs. A generator can only be trusted
// if its output is checked against something other than itself.
func TestBuiltinsArePlausible(t *testing.T) {
	wantBinary := map[string]string{
		"gopls":         "gopls",
		"clangd":        "clangd",
		"lua-ls":        "lua-language-server",
		"pyright":       "pyright-langserver",
		"rust-analyzer": "rust-analyzer",
		"vtsls":         "vtsls",
	}
	// One marker per server that must be in the list, and its position
	// relative to .git: a project marker that loses to .git would route
	// every file to the top of the repository.
	wantMarker := map[string]string{
		"gopls":         "go.mod",
		"clangd":        "compile_commands.json",
		"lua-ls":        ".luarc.json",
		"pyright":       "pyproject.toml",
		"rust-analyzer": "Cargo.toml",
		"vtsls":         "package.json",
	}
	wantLanguage := map[string]string{
		"gopls":         "go",
		"clangd":        "cpp",
		"lua-ls":        "lua",
		"pyright":       "python",
		"rust-analyzer": "rust",
		"vtsls":         "typescript",
	}
	wantGlob := map[string]string{
		"gopls":         "**/*.go",
		"clangd":        "**/*.cpp",
		"lua-ls":        "**/*.lua",
		"pyright":       "**/*.py",
		"rust-analyzer": "**/*.rs",
		"vtsls":         "**/*.ts",
	}

	for _, def := range Builtins() {
		t.Run(def.Name, func(t *testing.T) {
			if err := def.Validate(); err != nil {
				t.Fatalf("does not validate: %v", err)
			}
			if def.SchemaVersion != SchemaVersion {
				t.Errorf("SchemaVersion = %d, want %d", def.SchemaVersion, SchemaVersion)
			}
			if got := def.Server.Command[0]; got != wantBinary[def.Name] {
				t.Errorf("command[0] = %q, want %q", got, wantBinary[def.Name])
			}
			if def.Server.Transport != TransportStdio {
				t.Errorf("transport = %q, want %q", def.Server.Transport, TransportStdio)
			}
			if def.Activation.Priority != DefaultPriority {
				t.Errorf("priority = %d, want the default %d: nothing in M4 ranks one server above another",
					def.Activation.Priority, DefaultPriority)
			}
			if def.Install.Mise == "" {
				t.Error("no install.mise: PLAN §6 needs an exact command to print on exit 3")
			}

			markers := def.Activation.RootMarkers
			if !slices.Contains(markers, wantMarker[def.Name]) {
				t.Errorf("root markers %v do not include %q", markers, wantMarker[def.Name])
			}
			git := slices.Index(markers, ".git")
			if git < 0 {
				t.Error("root markers do not include .git, so a loose file has no root at all")
			} else if git != len(markers)-1 {
				t.Errorf("root markers %v put .git at %d; it must be last, or it would outrank every project marker",
					markers, git)
			}

			if !slices.Contains(def.Activation.Languages, wantLanguage[def.Name]) {
				t.Errorf("languages %v do not include %q", def.Activation.Languages, wantLanguage[def.Name])
			}
			for _, lang := range def.Activation.Languages {
				if lang != strings.ToLower(lang) || strings.Contains(lang, ".") {
					t.Errorf("language id %q is not an LSP language id: the corpus's Neovim filetypes must be mapped", lang)
				}
			}

			if !slices.Contains(def.Activation.Globs, wantGlob[def.Name]) {
				t.Errorf("globs %v do not include %q", def.Activation.Globs, wantGlob[def.Name])
			}
			for _, glob := range def.Activation.Globs {
				if !strings.HasPrefix(glob, "**/") {
					t.Errorf("glob %q is not anchored with **/, so it would only match at the workspace root", glob)
				}
			}
		})
	}
}

// TestBuiltinProvenance keeps the generated table attributable: an
// embedded corpus with no provenance is an unattributed copy.
func TestBuiltinProvenance(t *testing.T) {
	p := BuiltinProvenance()
	if p.Corpus == "" || p.Upstream == "" || p.Commit == "" || p.License == "" || p.Snapshot == "" {
		t.Fatalf("provenance is incomplete: %+v", p)
	}
	if len(p.Commit) != 40 {
		t.Errorf("commit %q is not a full sha1; a snapshot pinned to an abbreviation is not pinned", p.Commit)
	}
	if p.License != "Apache-2.0" {
		t.Errorf("license = %q; PLAN §9.3 rules out a copyleft corpus, so this must stay Apache-2.0", p.License)
	}
}

// TestBuiltinsAreIndependentCopies is the guard the layering depends on:
// folding an override into a built-in must not reach the next caller.
func TestBuiltinsAreIndependentCopies(t *testing.T) {
	first := Builtins()[0]
	first.Activation.Languages[0] = "corrupted"
	first.Server.Command[0] = "corrupted"

	second := Builtins()[0]
	if second.Activation.Languages[0] == "corrupted" || second.Server.Command[0] == "corrupted" {
		t.Fatal("Builtins() shares state between calls")
	}
}
