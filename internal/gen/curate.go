package gen

import (
	"fmt"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// The curation layer: the hand-written half of a built-in definition.
//
// A generated table is only honest if it is clear which half came from
// where, so the split is deliberate and narrow:
//
// From the corpus, always:
//
//	server.command          upstream's cmd
//	activation.languages    upstream's filetypes, mapped to LSP language
//	                        ids (they are not the same vocabulary)
//	activation.root_markers upstream's root_markers, when it has them
//	                        declaratively — clangd and pyright do
//
// Curated, because the corpus cannot say it:
//
//	activation.globs        nvim-lspconfig has no globs at all: Neovim
//	                        dispatches on filetype, which it computes
//	                        itself. lightspeed has to claim paths, so
//	                        the extension lists are ours.
//	activation.root_markers for the servers whose upstream root_dir is
//	                        a Lua function — gopls, rust-analyzer and
//	                        vtsls — transcribed from that function's
//	                        own fallback chain, and for lua-ls, whose
//	                        marker list is a computed expression.
//	install.mise            the corpus knows nothing about installing.
//	activation.priority     all equal; lightspeed's routing gives no
//	                        server precedence over another today.
//
// Deliberately not imported:
//
//	settings, capabilities  editor policy. Upstream turns on semantic
//	                        tokens, code lenses and inlay hints because
//	                        an editor draws them; PLAN §2 lists exactly
//	                        those as non-goals, and shipping them as
//	                        defaults would make every query slower for
//	                        output no CLI prints.
//	init_options            same, one step removed: upstream's only
//	                        init option in this snapshot is vtsls's
//	                        hostInfo = 'neovim', which would be a lie.
//	                        It is curated to "lightspeed" instead.
//	root_dir, on_attach,    Lua behaviour. PLAN §5.4 puts server quirks
//	on_init, before_init,   in declarative config, not in code, so what
//	get_language_id         cannot be said declaratively is dropped and
//	                        noted rather than approximated in Go.
type curation struct {
	// Name is what lightspeed calls the server. It follows PLAN §3's
	// spelling, which is the servers' own (rust-analyzer, lua-ls),
	// not the corpus's Lua-identifier spelling (rust_analyzer).
	Name string
	// Corpus is the vendored file this definition is built from.
	Corpus string
	// Globs claim paths, since the corpus has none.
	Globs []string
	// RootMarkers overrides the corpus, and must carry a Reason.
	RootMarkers []string
	// RootMarkerReason justifies overriding or supplying the markers.
	RootMarkerReason string
	// Mise is the install spec handed to `mise use -g`.
	Mise string
	// MiseNote records anything uncertain about that spec.
	MiseNote string
	// InitOptions is sent as initializationOptions.
	InitOptions map[string]any
	// Languages overrides the mapped filetypes entirely; empty means
	// map them.
	Languages []string
	// Notes are extra remarks emitted with the definition.
	Notes []string
}

// curations is the whole built-in table, in emission order.
//
// The six servers are exactly PLAN §8 M4's definition of done. gopls
// comes first because it is the reference implementation this project
// generalises (PLAN §0) and because the router's load order is its
// preference order; the rest are alphabetical.
var curations = []curation{{
	Name:   "gopls",
	Corpus: "gopls",
	Globs:  []string{"**/*.go", "**/go.mod", "**/go.work"},
	RootMarkers: []string{
		"go.work",
		"go.mod",
		".git",
	},
	RootMarkerReason: "upstream computes the root in Lua; this is that function's own chain, " +
		"`vim.fs.root(fname, 'go.work') or vim.fs.root(fname, 'go.mod') or vim.fs.root(fname, '.git')`",
	Mise: "go:golang.org/x/tools/gopls@v0.23.0",
	MiseNote: "the version PLAN §6 pins, and the one internal/gopls is vendored from; " +
		"pinning here is only meaningful because mise records a checksum for it",
}, {
	Name:   "clangd",
	Corpus: "clangd",
	Globs: []string{
		"**/*.c", "**/*.h",
		"**/*.cc", "**/*.cpp", "**/*.cxx", "**/*.c++",
		"**/*.hh", "**/*.hpp", "**/*.hxx", "**/*.h++",
		"**/*.m", "**/*.mm",
		"**/*.cu", "**/*.cuh",
	},
	Mise: "ubi:clangd/clangd",
	MiseNote: "clangd has no mise registry entry, so the backend is named explicitly. " +
		"It is the one install spec in this table that could not be checked against " +
		"a registry offline; `lightspeed doctor` reports mise's own failure verbatim if it is wrong",
}, {
	Name:   "lua-ls",
	Corpus: "lua_ls",
	Globs:  []string{"**/*.lua"},
	RootMarkers: []string{
		".emmyrc.json",
		".luarc.json",
		".luarc.jsonc",
		".luacheckrc",
		".stylua.toml",
		"stylua.toml",
		"selene.toml",
		"selene.yml",
		".git",
	},
	RootMarkerReason: "upstream's list is a version test over two marker groups; this is those " +
		"groups concatenated in order, which is what upstream's own pre-0.11.3 branch does",
	Mise: "lua-language-server",
}, {
	Name:   "pyright",
	Corpus: "pyright",
	Globs:  []string{"**/*.py", "**/*.pyi"},
	Mise:   "npm:pyright",
}, {
	Name:   "rust-analyzer",
	Corpus: "rust_analyzer",
	Globs:  []string{"**/*.rs", "**/Cargo.toml"},
	RootMarkers: []string{
		"rust-project.json",
		"Cargo.toml",
		".git",
	},
	RootMarkerReason: "upstream computes the root in Lua, via `cargo metadata`, which this " +
		"package will not run at build time and the router will not run at query time; " +
		"rust-project.json comes first because it is an explicit, deliberate project " +
		"description, and a Cargo workspace member therefore resolves to the member, not " +
		"the workspace root — a documented narrowing of upstream's behaviour",
	Mise: "rust-analyzer",
}, {
	Name:   "vtsls",
	Corpus: "vtsls",
	Globs: []string{
		"**/*.ts", "**/*.tsx", "**/*.mts", "**/*.cts",
		"**/*.js", "**/*.jsx", "**/*.mjs", "**/*.cjs",
	},
	RootMarkers: []string{
		"package-lock.json",
		"yarn.lock",
		"pnpm-lock.yaml",
		"bun.lockb",
		"bun.lock",
		"tsconfig.json",
		"jsconfig.json",
		"package.json",
		".git",
	},
	RootMarkerReason: "upstream computes the root in Lua: a package-manager lock file, else " +
		"the working directory, and it refuses to attach at all inside a Deno project. The " +
		"lock files are transcribed; tsconfig.json, jsconfig.json and package.json are added " +
		"as a fallback for a checkout with no lock file, and the Deno exclusion is not " +
		"expressible declaratively — a Deno project needs a .lightspeed.toml",
	InitOptions: map[string]any{"hostInfo": "lightspeed"},
	Mise:        "npm:@vtsls/language-server",
	Notes: []string{
		"upstream sets hostInfo = 'neovim'; ours says what is actually driving the server",
	},
}}

// filetypeLanguageID maps a Neovim filetype to the LSP language
// identifier a server matches on. Most filetypes are already the
// language id; these are the ones that are not, and getting them wrong
// means a server silently never claims a file.
//
// The mapping for objc/objcpp/cuda is upstream's own, from clangd's
// get_language_id function — the one piece of that Lua worth keeping,
// transcribed as data.
var filetypeLanguageID = map[string]string{
	"objc":   "objective-c",
	"objcpp": "objective-cpp",
	"cuda":   "cuda-cpp",
}

// dropFiletypes are Neovim compound filetypes with no LSP counterpart.
// "c.doxygen" is Neovim saying "C, and the comments are Doxygen"; no
// language server has ever been sent it as a language id.
var dropFiletypes = map[string]bool{
	"c.doxygen":   true,
	"cpp.doxygen": true,
}

// languageIDs maps a corpus filetype list to LSP language ids, dropping
// the Neovim-only ones and de-duplicating while keeping order.
func languageIDs(filetypes []string) (ids []string, dropped []string) {
	for _, ft := range filetypes {
		if dropFiletypes[ft] {
			dropped = append(dropped, ft)
			continue
		}
		id := ft
		if mapped, ok := filetypeLanguageID[ft]; ok {
			id = mapped
		}
		if !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	return ids, dropped
}

// A Builtin is one generated definition together with everything the
// emitter needs to explain it.
type Builtin struct {
	// Def is the definition itself, already validated.
	Def *serverdef.ServerDef
	// Corpus is the vendored file it was built from.
	Corpus string
	// FromCorpus lists the definition keys the corpus supplied.
	FromCorpus []string
	// Curated lists the keys the curation supplied, each with its
	// reason.
	Curated []Note
	// NotImported is what the corpus said and this table does not
	// carry, with the reason.
	NotImported []Note
}

// BuildAll turns the vendored corpus into the built-in table. It is
// deterministic: same snapshot, same table, byte for byte, on any
// machine and with no network.
func BuildAll() ([]*Builtin, error) {
	out := make([]*Builtin, 0, len(curations))
	for _, c := range curations {
		src, err := CorpusFile(c.Corpus)
		if err != nil {
			return nil, err
		}
		cfg, err := ExtractCorpus(c.Corpus, src)
		if err != nil {
			return nil, err
		}
		built, err := c.build(cfg)
		if err != nil {
			return nil, err
		}
		out = append(out, built)
	}
	return out, nil
}

func (c curation) build(cfg *CorpusConfig) (*Builtin, error) {
	b := &Builtin{Corpus: c.Corpus}

	languages := c.Languages
	fromCorpus := []string{"server.command"}
	if len(languages) == 0 {
		ids, dropped := languageIDs(cfg.Filetypes)
		languages = ids
		fromCorpus = append(fromCorpus, "activation.languages")
		if len(dropped) > 0 {
			b.NotImported = append(b.NotImported, Note{
				Key:    "filetypes",
				Reason: fmt.Sprintf("dropped the Neovim-only filetype(s) %s", strings.Join(dropped, ", ")),
			})
		}
	} else {
		b.Curated = append(b.Curated, Note{Key: "activation.languages", Reason: "overridden by curation"})
	}

	markers := cfg.RootMarkers
	switch {
	case len(c.RootMarkers) > 0:
		markers = c.RootMarkers
		reason := c.RootMarkerReason
		if reason == "" {
			return nil, fmt.Errorf("%s: curated root markers need a reason", c.Name)
		}
		b.Curated = append(b.Curated, Note{Key: "activation.root_markers", Reason: reason})
	case len(markers) > 0:
		fromCorpus = append(fromCorpus, "activation.root_markers")
	default:
		return nil, fmt.Errorf("%s: neither the corpus nor the curation supplies root markers", c.Name)
	}

	if len(c.Globs) == 0 {
		return nil, fmt.Errorf("%s: curation supplies no globs, so the server would only ever be claimed by language id", c.Name)
	}
	b.Curated = append(b.Curated, Note{
		Key:    "activation.globs",
		Reason: "the corpus has no globs: Neovim dispatches on filetype, lightspeed on paths",
	})

	if c.Mise != "" {
		reason := "the corpus knows nothing about installation"
		if c.MiseNote != "" {
			reason = c.MiseNote
		}
		b.Curated = append(b.Curated, Note{Key: "install.mise", Reason: reason})
	}
	if len(c.InitOptions) > 0 {
		b.Curated = append(b.Curated, Note{Key: "server.initialization_options", Reason: "curated; see below"})
	}
	for _, note := range c.Notes {
		b.Curated = append(b.Curated, Note{Key: "note", Reason: note})
	}

	// Everything the corpus said that this table does not carry.
	if len(cfg.Settings) > 0 {
		b.NotImported = append(b.NotImported, Note{
			Key:    "settings",
			Reason: "editor policy (semantic tokens, code lenses, inlay hints); PLAN §2 makes those non-goals",
		})
	}
	if len(cfg.InitOptions) > 0 && len(c.InitOptions) == 0 {
		b.NotImported = append(b.NotImported, Note{Key: "init_options", Reason: "editor-specific"})
	}
	b.NotImported = append(b.NotImported, cfg.Notes...)

	b.FromCorpus = fromCorpus
	b.Def = &serverdef.ServerDef{
		SchemaVersion: serverdef.SchemaVersion,
		Name:          c.Name,
		Activation: serverdef.Activation{
			Languages:   languages,
			Globs:       c.Globs,
			RootMarkers: markers,
			Priority:    serverdef.DefaultPriority,
		},
		Server: serverdef.Server{
			Command:               cfg.Cmd,
			Transport:             serverdef.TransportStdio,
			InitializationOptions: c.InitOptions,
		},
		Install: serverdef.Install{Mise: c.Mise},
	}
	if err := b.Def.Validate(); err != nil {
		return nil, fmt.Errorf("generated definition is invalid: %w", err)
	}
	return b, nil
}
