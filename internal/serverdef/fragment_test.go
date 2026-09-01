package serverdef

import (
	"slices"
	"strings"
	"testing"
)

func TestParseFragmentsPresence(t *testing.T) {
	src := `schema_version = 1
name = "gopls"

[activation]
priority = 60
`
	frags, err := ParseFragments([]byte(src))
	if err != nil {
		t.Fatalf("ParseFragments() = %v", err)
	}
	if len(frags) != 1 {
		t.Fatalf("got %d fragments, want 1", len(frags))
	}
	f := frags[0]
	if f.Name != "gopls" {
		t.Errorf("Name = %q, want gopls", f.Name)
	}
	if !f.Has(KeyPriority) {
		t.Error("priority was set in the file but Has(KeyPriority) is false")
	}
	// Everything else must read as absent, not as an empty value: this
	// is the whole reason fragments exist.
	for _, key := range []string{KeyLanguages, KeyGlobs, KeyRootMarkers, KeyCommand, KeyTransport, KeyInstallMise} {
		if f.Has(key) {
			t.Errorf("Has(%q) = true, want false: the file never mentions it", key)
		}
	}
	if got, want := f.Keys(), []string{KeyPriority}; !slices.Equal(got, want) {
		t.Errorf("Keys() = %v, want %v", got, want)
	}
}

func TestParseFragmentsMultiForm(t *testing.T) {
	src := `schema_version = 1

[servers.vtsls.activation]
priority = 10

[servers.gopls.server]
command = ["gopls", "-rpc.trace"]

[servers.gopls.install]
mise = "go:golang.org/x/tools/gopls@v0.24.0"
`
	frags, err := ParseFragments([]byte(src))
	if err != nil {
		t.Fatalf("ParseFragments() = %v", err)
	}
	if len(frags) != 2 {
		t.Fatalf("got %d fragments, want 2", len(frags))
	}
	// Fragments come back sorted, so a report over them is stable.
	if frags[0].Name != "gopls" || frags[1].Name != "vtsls" {
		t.Fatalf("names = %q, %q, want gopls, vtsls (sorted)", frags[0].Name, frags[1].Name)
	}
	if got, want := frags[0].Keys(), []string{KeyCommand, KeyInstallMise}; !slices.Equal(got, want) {
		t.Errorf("gopls keys = %v, want %v", got, want)
	}
	if got, want := frags[1].Keys(), []string{KeyPriority}; !slices.Equal(got, want) {
		t.Errorf("vtsls keys = %v, want %v", got, want)
	}
}

func TestParseFragmentsErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{{
		name: "mixed forms",
		src:  "schema_version = 1\nname = \"x\"\n[servers.y.server]\ncommand = [\"y\"]\n",
		want: "cannot also set \"name\" at the top level",
	}, {
		name: "name inside a servers table",
		src:  "schema_version = 1\n[servers.y]\nname = \"z\"\n",
		want: "named by the table key",
	}, {
		name: "bad name in the table key",
		src:  "schema_version = 1\n[servers.\"../etc\".server]\ncommand = [\"x\"]\n",
		want: `name "../etc"`,
	}, {
		name: "unknown key in a servers table",
		src:  "schema_version = 1\n[servers.y]\nnope = 1\n",
		want: `unknown key "nope" in [servers.y]`,
	}, {
		name: "schema_version inside a servers table",
		src:  "schema_version = 1\n[servers.y]\nschema_version = 1\n",
		want: `unknown key "schema_version" in [servers.y]`,
	}, {
		name: "key path names the nested table",
		src:  "schema_version = 1\n[servers.y.activation]\nlanguages = \"go\"\n",
		want: "servers.y.activation.languages: expected an array of strings",
	}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			frags, err := ParseFragments([]byte(tt.src))
			if err == nil {
				t.Fatalf("ParseFragments() = %+v, want an error mentioning %q", frags, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestParseRejectsMultiDefinitionFile keeps Parse's contract: it returns
// one whole definition, so a file the layered loader accepts is not
// silently half-read by it.
func TestParseRejectsMultiDefinitionFile(t *testing.T) {
	src := "schema_version = 1\n[servers.a.server]\ncommand = [\"a\"]\n[servers.b.server]\ncommand = [\"b\"]\n"
	if def, err := Parse([]byte(src)); err == nil {
		t.Fatalf("Parse() = %+v, want an error", def)
	} else if !strings.Contains(err.Error(), "found 2") {
		t.Errorf("error = %q, want it to say how many definitions it found", err)
	}
}

func TestFragmentApplyToNil(t *testing.T) {
	frags, err := ParseFragments([]byte("schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\"]\n[server]\ncommand = [\"x\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	def := frags[0].ApplyTo(nil)
	if def.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", def.SchemaVersion, SchemaVersion)
	}
	if def.Activation.Priority != DefaultPriority {
		t.Errorf("Priority = %d, want the default %d", def.Activation.Priority, DefaultPriority)
	}
	if def.Server.Transport != TransportStdio {
		t.Errorf("Transport = %q, want %q", def.Server.Transport, TransportStdio)
	}
	if err := def.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

// TestFragmentApplyToMergeRules is the documented contract of the
// layering: absent inherits, arrays and scalars replace, the two
// free-form tables merge.
func TestFragmentApplyToMergeRules(t *testing.T) {
	base, err := Parse([]byte(`schema_version = 1
name = "gopls"

[activation]
languages    = ["go", "gomod"]
globs        = ["**/*.go"]
root_markers = ["go.work", "go.mod", ".git"]
priority     = 50

[server]
command = ["gopls"]
settings = { gopls = { staticcheck = true, hints = { assignVariableTypes = true } } }
initialization_options = { usePlaceholders = false }

[install]
mise = "go:golang.org/x/tools/gopls@v0.23.0"
`))
	if err != nil {
		t.Fatal(err)
	}

	frags, err := ParseFragments([]byte(`schema_version = 1
name = "gopls"

[activation]
languages = ["go"]
priority  = 90

[server]
settings = { gopls = { staticcheck = false, buildFlags = ["-tags=integration"] } }
`))
	if err != nil {
		t.Fatal(err)
	}
	merged := frags[0].ApplyTo(base)

	if got, want := merged.Activation.Languages, []string{"go"}; !slices.Equal(got, want) {
		t.Errorf("languages = %v, want %v: an array replaces, so a shorter list is how you remove one", got, want)
	}
	if got, want := merged.Activation.Globs, []string{"**/*.go"}; !slices.Equal(got, want) {
		t.Errorf("globs = %v, want %v inherited", got, want)
	}
	if got, want := merged.Activation.RootMarkers, []string{"go.work", "go.mod", ".git"}; !slices.Equal(got, want) {
		t.Errorf("root markers = %v, want %v inherited", got, want)
	}
	if merged.Activation.Priority != 90 {
		t.Errorf("priority = %d, want 90", merged.Activation.Priority)
	}
	if got, want := merged.Server.Command, []string{"gopls"}; !slices.Equal(got, want) {
		t.Errorf("command = %v, want %v inherited", got, want)
	}
	if merged.Install.Mise == "" {
		t.Error("install.mise was dropped, but the override never mentioned it")
	}
	if got := merged.Server.InitializationOptions["usePlaceholders"]; got != false {
		t.Errorf("initialization_options.usePlaceholders = %#v, want the inherited false", got)
	}

	gopls, ok := merged.Server.Settings["gopls"].(map[string]any)
	if !ok {
		t.Fatalf("settings.gopls = %#v, want a table", merged.Server.Settings["gopls"])
	}
	if got := gopls["staticcheck"]; got != false {
		t.Errorf("settings.gopls.staticcheck = %#v, want the override's false", got)
	}
	if _, ok := gopls["buildFlags"]; !ok {
		t.Error("settings.gopls.buildFlags was not added by the override")
	}
	// The deep merge must not blow away a sibling subtree the override
	// said nothing about.
	hints, ok := gopls["hints"].(map[string]any)
	if !ok {
		t.Fatalf("settings.gopls.hints = %#v, want the inherited table", gopls["hints"])
	}
	if got := hints["assignVariableTypes"]; got != true {
		t.Errorf("settings.gopls.hints.assignVariableTypes = %#v, want the inherited true", got)
	}

	// And the base is untouched.
	if got := base.Server.Settings["gopls"].(map[string]any)["staticcheck"]; got != true {
		t.Errorf("ApplyTo modified its base: staticcheck = %#v, want true", got)
	}
	if got, want := base.Activation.Languages, []string{"go", "gomod"}; !slices.Equal(got, want) {
		t.Errorf("ApplyTo modified its base: languages = %v, want %v", got, want)
	}
}

func TestFragmentExplicitZeroAndEmptyValues(t *testing.T) {
	base, err := Parse([]byte("schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\"]\nglobs = [\"**/*.go\"]\n[server]\ncommand = [\"x\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	frags, err := ParseFragments([]byte("schema_version = 1\nname = \"x\"\n[activation]\npriority = 0\nglobs = []\n"))
	if err != nil {
		t.Fatal(err)
	}
	merged := frags[0].ApplyTo(base)
	if merged.Activation.Priority != 0 {
		t.Errorf("priority = %d, want the explicit 0", merged.Activation.Priority)
	}
	if len(merged.Activation.Globs) != 0 {
		t.Errorf("globs = %v, want the explicit empty list", merged.Activation.Globs)
	}
	// Languages still claim the file, so the result is usable.
	if err := merged.Validate(); err != nil {
		t.Errorf("Validate() = %v", err)
	}
}

func TestNewFragmentSetsEverything(t *testing.T) {
	def, _ := Builtin("gopls")
	f := NewFragment(def)
	if got, want := len(f.Keys()), len(overrideKeys); got != want {
		t.Errorf("NewFragment().Keys() has %d keys, want all %d", got, want)
	}
	// A whole definition folded onto nothing is itself.
	applied := f.ApplyTo(nil)
	if !slices.Equal(applied.Server.Command, def.Server.Command) ||
		!slices.Equal(applied.Activation.RootMarkers, def.Activation.RootMarkers) {
		t.Errorf("NewFragment(def).ApplyTo(nil) = %+v, want the definition back", applied)
	}
}
