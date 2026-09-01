package gen

import (
	"fmt"
	"slices"
)

// A Note records something the corpus said that was not imported, and
// why. Notes are not diagnostics to be silenced: they are emitted as
// comments into the generated table, so that anyone reading a built-in
// definition can see what upstream expresses that lightspeed does not.
type Note struct {
	// Key is the corpus key, "cmd", "root_dir", "settings".
	Key string
	// Reason is why it was left out.
	Reason string
}

func (n Note) String() string { return n.Key + ": " + n.Reason }

// A CorpusConfig is one nvim-lspconfig server module, reduced to the
// declarative facts a server definition can hold.
type CorpusConfig struct {
	// Corpus is the upstream file's base name, "rust_analyzer".
	Corpus string
	// Cmd is the argv upstream starts the server with.
	Cmd []string
	// Filetypes are Neovim filetypes, which are close to LSP language
	// ids but not the same thing; [curation] maps them.
	Filetypes []string
	// RootMarkers are upstream's declarative root markers, nil when
	// upstream computes them in Lua instead.
	RootMarkers []string
	// InitOptions and Settings are upstream's, imported for
	// completeness. The generator does not use them — see the policy
	// note in curate.go — but reading them is how their absence from
	// the generated table becomes a documented choice rather than an
	// omission.
	InitOptions map[string]any
	Settings    map[string]any
	// Notes are everything not imported, in key order.
	Notes []Note
}

// corpusKeys are the keys the reader understands; anything else in an
// upstream module is recorded as a note under its own name.
var corpusKeys = []string{"cmd", "filetypes", "root_markers", "init_options", "settings"}

// ExtractCorpus reads one vendored server module. It fails only when
// what upstream says cannot be believed — no command, no filetypes, a
// malformed list — because a built-in that cannot start or can never
// activate is worse than no built-in at all.
func ExtractCorpus(name string, src []byte) (*CorpusConfig, error) {
	tbl, err := ReturnedTable(src)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	cfg := &CorpusConfig{Corpus: name}

	if cfg.Cmd, err = stringList(tbl, "cmd"); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(cfg.Cmd) == 0 {
		return nil, fmt.Errorf("%s: cmd is missing or not a literal list, so there is no way to start the server", name)
	}
	if cfg.Filetypes, err = stringList(tbl, "filetypes"); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	if len(cfg.Filetypes) == 0 {
		return nil, fmt.Errorf("%s: filetypes is missing or not a literal list, so the server could never be selected", name)
	}

	if v, ok := tbl.Get("root_markers"); ok {
		markers, err := valueStrings(v)
		if err != nil {
			cfg.note("root_markers", err.Error())
		} else {
			cfg.RootMarkers = markers
		}
	}

	for _, key := range []string{"init_options", "settings"} {
		v, ok := tbl.Get(key)
		if !ok {
			continue
		}
		sub, ok := v.(*Table)
		if !ok {
			cfg.note(key, describeValue(v))
			continue
		}
		table, dropped := sub.Map()
		for _, d := range dropped {
			cfg.note(key+"."+d, "not a literal")
		}
		if key == "init_options" {
			cfg.InitOptions = table
		} else {
			cfg.Settings = table
		}
	}

	for _, key := range tbl.Keys() {
		if slices.Contains(corpusKeys, key) {
			continue
		}
		v, _ := tbl.Get(key)
		cfg.note(key, describeValue(v))
	}
	slices.SortStableFunc(cfg.Notes, func(a, b Note) int {
		if a.Key == b.Key {
			return 0
		}
		if a.Key < b.Key {
			return -1
		}
		return 1
	})
	return cfg, nil
}

func (c *CorpusConfig) note(key, reason string) {
	c.Notes = append(c.Notes, Note{Key: key, Reason: reason})
}

// Note reports the recorded reason for a key, if there is one.
func (c *CorpusConfig) Note(key string) (string, bool) {
	for _, n := range c.Notes {
		if n.Key == key {
			return n.Reason, true
		}
	}
	return "", false
}

func stringList(tbl *Table, key string) ([]string, error) {
	v, ok := tbl.Get(key)
	if !ok {
		return nil, nil
	}
	list, err := valueStrings(v)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", key, err)
	}
	return list, nil
}

func valueStrings(v Value) ([]string, error) {
	sub, ok := v.(*Table)
	if !ok {
		return nil, fmt.Errorf("%s", describeValue(v))
	}
	return sub.Strings()
}

func describeValue(v Value) string {
	switch v := v.(type) {
	case Unsupported:
		return v.Reason
	case *Table:
		return "a table"
	case string:
		return "a string, want a list"
	default:
		return fmt.Sprintf("%T", v)
	}
}
