package serverdef

import (
	"strings"
	"testing"
)

// planExample is the gopls definition of PLAN §6, verbatim. If the
// plan's own example stops parsing, the parser is wrong, not the plan.
const planExample = `schema_version = 1
name = "gopls"

[activation]
languages    = ["go", "gomod", "gotmpl"]
globs        = ["**/*.go", "**/go.mod"]
root_markers = ["go.work", "go.mod", ".git"]
priority     = 50

[server]
command   = ["gopls", "serve"]
transport = "stdio"
initialization_options = { usePlaceholders = false }
settings = { gopls = { "ui.diagnostic.staticcheck" = true } }

[install]
mise = "go:golang.org/x/tools/gopls@v0.23.0"   # delegate; mise handles checksums+lockfile
`

func TestParsePlanExample(t *testing.T) {
	def, err := Parse([]byte(planExample))
	if err != nil {
		t.Fatalf("Parse(PLAN §6 example) = %v", err)
	}

	if def.SchemaVersion != 1 {
		t.Errorf("SchemaVersion = %d, want 1", def.SchemaVersion)
	}
	if def.Name != "gopls" {
		t.Errorf("Name = %q, want %q", def.Name, "gopls")
	}
	if got, want := def.Activation.Languages, []string{"go", "gomod", "gotmpl"}; !equalStrings(got, want) {
		t.Errorf("Activation.Languages = %q, want %q", got, want)
	}
	if got, want := def.Activation.Globs, []string{"**/*.go", "**/go.mod"}; !equalStrings(got, want) {
		t.Errorf("Activation.Globs = %q, want %q", got, want)
	}
	if got, want := def.Activation.RootMarkers, []string{"go.work", "go.mod", ".git"}; !equalStrings(got, want) {
		t.Errorf("Activation.RootMarkers = %q, want %q", got, want)
	}
	if def.Activation.Priority != 50 {
		t.Errorf("Activation.Priority = %d, want 50", def.Activation.Priority)
	}
	if got, want := def.Server.Command, []string{"gopls", "serve"}; !equalStrings(got, want) {
		t.Errorf("Server.Command = %q, want %q", got, want)
	}
	if def.Server.Transport != TransportStdio {
		t.Errorf("Server.Transport = %q, want %q", def.Server.Transport, TransportStdio)
	}
	if got := def.Server.InitializationOptions["usePlaceholders"]; got != false {
		t.Errorf("initialization_options.usePlaceholders = %#v, want false", got)
	}
	gopls, ok := def.Server.Settings["gopls"].(map[string]any)
	if !ok {
		t.Fatalf("settings.gopls = %#v, want a table", def.Server.Settings["gopls"])
	}
	if got := gopls["ui.diagnostic.staticcheck"]; got != true {
		t.Errorf("settings.gopls[\"ui.diagnostic.staticcheck\"] = %#v, want true", got)
	}
	if want := "go:golang.org/x/tools/gopls@v0.23.0"; def.Install.Mise != want {
		t.Errorf("Install.Mise = %q, want %q", def.Install.Mise, want)
	}
}

func TestParseDefaults(t *testing.T) {
	// A definition that leans on every default: no priority, no
	// transport, no install.
	const src = `
schema_version = 1
name = "minimal"
[activation]
languages = ["lua"]
[server]
command = ["lua-language-server"]
`
	def, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if def.Activation.Priority != DefaultPriority {
		t.Errorf("Activation.Priority = %d, want the default %d", def.Activation.Priority, DefaultPriority)
	}
	if def.Server.Transport != TransportStdio {
		t.Errorf("Server.Transport = %q, want %q", def.Server.Transport, TransportStdio)
	}
	if def.Install.Mise != "" {
		t.Errorf("Install.Mise = %q, want empty", def.Install.Mise)
	}
	if len(def.Activation.Globs) != 0 {
		t.Errorf("Activation.Globs = %q, want none", def.Activation.Globs)
	}
}

// TestParseExplicitZeroPriority guards the difference between "absent"
// and "zero": a definition that deliberately says 0 must not be
// silently promoted to the default.
func TestParseExplicitZeroPriority(t *testing.T) {
	const src = `
schema_version = 1
name = "last-resort"
[activation]
globs = ["**/*"]
priority = 0
[server]
command = ["cat"]
`
	def, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	if def.Activation.Priority != 0 {
		t.Errorf("Activation.Priority = %d, want 0", def.Activation.Priority)
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string // substring the error must mention
	}{{
		name: "unknown top-level key",
		src:  "schema_version = 1\nname = \"x\"\nactivations = 1\n",
		want: `unknown key "activations"`,
	}, {
		name: "unknown activation key",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nroot_marker = [\"go.mod\"]\n",
		want: `unknown key "root_marker"`,
	}, {
		name: "missing schema_version",
		src:  "name = \"x\"\n[activation]\nlanguages = [\"go\"]\n[server]\ncommand = [\"x\"]\n",
		want: "schema_version is required",
	}, {
		name: "future schema_version",
		src:  "schema_version = 2\nname = \"x\"\n[activation]\nlanguages = [\"go\"]\n[server]\ncommand = [\"x\"]\n",
		want: "unsupported schema_version 2",
	}, {
		name: "missing name",
		src:  "schema_version = 1\n[activation]\nlanguages = [\"go\"]\n[server]\ncommand = [\"x\"]\n",
		want: "name is required",
	}, {
		name: "bad name",
		src:  "schema_version = 1\nname = \"../etc\"\n[activation]\nlanguages = [\"go\"]\n[server]\ncommand = [\"x\"]\n",
		want: `name "../etc"`,
	}, {
		name: "inert activation",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nroot_markers = [\"go.mod\"]\n[server]\ncommand = [\"x\"]\n",
		want: "at least one of languages or globs",
	}, {
		name: "missing command",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\"]\n",
		want: "server.command is required",
	}, {
		name: "unsupported transport",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\"]\n[server]\ncommand = [\"x\"]\ntransport = \"tcp\"\n",
		want: `unsupported server.transport "tcp"`,
	}, {
		name: "wrong type for name",
		src:  "schema_version = 1\nname = 7\n",
		want: "name: expected a string, found an integer",
	}, {
		name: "wrong type for languages",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nlanguages = \"go\"\n",
		want: "activation.languages: expected an array of strings",
	}, {
		name: "non-string in languages",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\", 3]\n",
		want: "activation.languages[1]: expected a string, found an integer",
	}, {
		name: "wrong type for server",
		src:  "schema_version = 1\nname = \"x\"\nserver = \"gopls\"\n",
		want: "server: expected a table",
	}, {
		name: "empty glob",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nglobs = [\"\"]\n[server]\ncommand = [\"x\"]\n",
		want: "activation.globs[0] is empty",
	}, {
		name: "arrays of tables",
		src:  "schema_version = 1\nname = \"x\"\n[[activation]]\n",
		want: "arrays of tables",
	}, {
		name: "duplicate key",
		src:  "schema_version = 1\nname = \"x\"\nname = \"y\"\n",
		want: `line 3: duplicate key "name"`,
	}, {
		name: "duplicate table",
		src:  "schema_version = 1\n[activation]\n[activation]\n",
		want: "line 3: table [activation] is defined more than once",
	}, {
		name: "unterminated string",
		src:  "schema_version = 1\nname = \"x\n",
		want: "line 2: unterminated string",
	}, {
		name: "multi-line string",
		src:  "schema_version = 1\nname = \"\"\"x\"\"\"\n",
		want: "line 2: multi-line strings are not supported",
	}, {
		name: "datetime",
		src:  "schema_version = 1\nname = \"x\"\npinned = 1979-05-27T07:32:00Z\n",
		want: "line 3: date and time values are not supported",
	}, {
		name: "missing equals",
		src:  "schema_version = 1\nname \"x\"\n",
		want: `line 2: expected '=' after key "name"`,
	}, {
		name: "trailing garbage",
		src:  "schema_version = 1 oops\n",
		want: `line 1: unexpected 'o' after value`,
	}, {
		name: "unterminated array",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\"\n",
		want: "expected ',' or ']' in array",
	}, {
		name: "unclosed table header",
		src:  "schema_version = 1\n[activation\n",
		want: "expected ']' to close table header [activation]",
	}, {
		name: "wrong type for priority",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\"]\npriority = \"high\"\n",
		want: "activation.priority: expected an integer, found a string",
	}, {
		name: "priority out of range",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\"]\npriority = 99999999999\n",
		want: "activation.priority: 99999999999 is out of range",
	}, {
		name: "wrong type for command",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\"]\n[server]\ncommand = \"gopls\"\n",
		want: "server.command: expected an array of strings, found a string",
	}, {
		name: "wrong type for settings",
		src:  "schema_version = 1\nname = \"x\"\n[activation]\nlanguages = [\"go\"]\n[server]\ncommand = [\"x\"]\nsettings = [1]\n",
		want: "server.settings: expected a table, found an array",
	}, {
		name: "wrong type for install",
		src:  "schema_version = 1\nname = \"x\"\ninstall = 1.5\n",
		want: "install: expected a table, found a float",
	}, {
		name: "unknown install key",
		src:  "schema_version = 1\nname = \"x\"\n[install]\nnpm = \"pyright\"\n",
		want: `unknown key "npm" in [install]`,
	}, {
		name: "wrong type for mise",
		src:  "schema_version = 1\nname = \"x\"\n[install]\nmise = true\n",
		want: "install.mise: expected a string, found a boolean",
	}, {
		name: "wrong type for activation",
		src:  "schema_version = 1\nname = \"x\"\nactivation = true\n",
		want: "activation: expected a table, found a boolean",
	}, {
		name: "value where table expected",
		src:  "schema_version = 1\nname = \"x\"\nserver = 1\n[server]\n",
		want: `cannot define table "server"`,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def, err := Parse([]byte(tt.src))
			if err == nil {
				t.Fatalf("Parse() = %+v, want an error mentioning %q", def, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse() error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
