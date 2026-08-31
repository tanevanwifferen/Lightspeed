package serverdef

// Built-in definitions for the three servers M1 targets. These are
// hand-written Go literals, in the shape of PLAN §6 and modelled on
// the nvim-lspconfig corpus: filetypes become language ids, root
// patterns become root markers in the same order (most significant
// first, because internal/router honours that order).
//
// PLAN §6 item 3 replaces this with a table generated at build time
// from an upstream corpus, and PLAN §6 item 4 adds PATH sniffing;
// both are M4. Until then this is the whole default layer, so it is
// kept small and uncontroversial rather than broad: no opinionated
// server settings, and the only pinned install spec is the gopls
// version PLAN §6 itself names. Version pinning for the rest is an
// M4 job, together with the mise lockfile that makes a pin meaningful.
var builtins = []*ServerDef{
	{
		SchemaVersion: SchemaVersion,
		Name:          "gopls",
		Activation: Activation{
			Languages:   []string{"go", "gomod", "gowork", "gotmpl"},
			Globs:       []string{"**/*.go", "**/go.mod", "**/go.work"},
			RootMarkers: []string{"go.work", "go.mod", ".git"},
			Priority:    DefaultPriority,
		},
		Server: Server{
			Command:   []string{"gopls", "serve"},
			Transport: TransportStdio,
		},
		Install: Install{Mise: "go:golang.org/x/tools/gopls@v0.23.0"},
	},
	{
		SchemaVersion: SchemaVersion,
		Name:          "rust-analyzer",
		Activation: Activation{
			Languages:   []string{"rust"},
			Globs:       []string{"**/*.rs", "**/Cargo.toml"},
			RootMarkers: []string{"rust-project.json", "Cargo.toml", ".git"},
			Priority:    DefaultPriority,
		},
		Server: Server{
			Command:   []string{"rust-analyzer"},
			Transport: TransportStdio,
		},
		Install: Install{Mise: "rust-analyzer"},
	},
	{
		SchemaVersion: SchemaVersion,
		Name:          "pyright",
		Activation: Activation{
			Languages: []string{"python"},
			Globs:     []string{"**/*.py", "**/*.pyi"},
			RootMarkers: []string{
				"pyrightconfig.json",
				"pyproject.toml",
				"setup.py",
				"setup.cfg",
				"requirements.txt",
				"Pipfile",
				".git",
			},
			Priority: DefaultPriority,
		},
		Server: Server{
			Command:   []string{"pyright-langserver", "--stdio"},
			Transport: TransportStdio,
		},
		Install: Install{Mise: "npm:pyright"},
	},
}

// Builtins returns the built-in server definitions, in a stable
// order. The result is freshly copied on every call, so a caller that
// layers overrides on top cannot corrupt the defaults for the next
// caller.
func Builtins() []*ServerDef {
	out := make([]*ServerDef, len(builtins))
	for i, d := range builtins {
		out[i] = d.Clone()
	}
	return out
}

// Builtin returns a copy of the built-in definition with the given
// name.
func Builtin(name string) (*ServerDef, bool) {
	for _, d := range builtins {
		if d.Name == name {
			return d.Clone(), true
		}
	}
	return nil, false
}
