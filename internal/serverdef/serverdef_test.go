package serverdef

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuiltinsValidate(t *testing.T) {
	got := Builtins()
	if len(got) == 0 {
		t.Fatal("Builtins() is empty")
	}
	names := map[string]bool{}
	for _, def := range got {
		if err := def.Validate(); err != nil {
			t.Errorf("built-in %q does not validate: %v", def.Name, err)
		}
		if names[def.Name] {
			t.Errorf("built-in %q appears twice", def.Name)
		}
		names[def.Name] = true
		if def.Install.Mise == "" {
			t.Errorf("built-in %q has no install.mise; PLAN §6 requires an exact command to suggest on exit 3", def.Name)
		}
		if len(def.Activation.RootMarkers) == 0 {
			t.Errorf("built-in %q has no root markers", def.Name)
		}
	}
	for _, want := range []string{"gopls", "rust-analyzer", "pyright"} {
		if !names[want] {
			t.Errorf("Builtins() is missing %q", want)
		}
	}
}

// TestBuiltinsAreCopies is the guard that matters for M4 layering:
// merging an override into a built-in must not change what the next
// caller sees.
func TestBuiltinsAreCopies(t *testing.T) {
	first := Builtins()
	first[0].Name = "mutated"
	first[0].Activation.Languages[0] = "mutated"
	first[0].Activation.Priority = 999
	first[0].Server.Command[0] = "mutated"

	second := Builtins()
	if second[0].Name == "mutated" {
		t.Error("mutating a returned definition changed the built-in table")
	}
	if second[0].Activation.Languages[0] == "mutated" {
		t.Error("mutating a returned language list changed the built-in table")
	}
	if second[0].Activation.Priority == 999 {
		t.Error("mutating a returned priority changed the built-in table")
	}
	if second[0].Server.Command[0] == "mutated" {
		t.Error("mutating a returned command changed the built-in table")
	}
}

func TestBuiltin(t *testing.T) {
	def, ok := Builtin("gopls")
	if !ok {
		t.Fatal(`Builtin("gopls") not found`)
	}
	if got, want := def.Server.Command[0], "gopls"; got != want {
		t.Errorf("gopls command[0] = %q, want %q", got, want)
	}
	if _, ok := Builtin("no-such-server"); ok {
		t.Error(`Builtin("no-such-server") reported found`)
	}
}

func TestCloneIsDeep(t *testing.T) {
	def, err := Parse([]byte(planExample))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	clone := def.Clone()

	settings := clone.Server.Settings["gopls"].(map[string]any)
	settings["ui.diagnostic.staticcheck"] = false
	clone.Server.InitializationOptions["usePlaceholders"] = true

	orig := def.Server.Settings["gopls"].(map[string]any)
	if orig["ui.diagnostic.staticcheck"] != true {
		t.Error("Clone() shares its nested settings table with the original")
	}
	if def.Server.InitializationOptions["usePlaceholders"] != false {
		t.Error("Clone() shares its initialization options with the original")
	}
	if clone.Clone() == nil {
		t.Error("Clone() of a clone is nil")
	}
	var nilDef *ServerDef
	if nilDef.Clone() != nil {
		t.Error("(*ServerDef)(nil).Clone() is not nil")
	}
}

func TestTransportOrDefault(t *testing.T) {
	if got := Transport("").OrDefault(); got != TransportStdio {
		t.Errorf(`Transport("").OrDefault() = %q, want %q`, got, TransportStdio)
	}
	if got := Transport("tcp").OrDefault(); got != "tcp" {
		t.Errorf(`Transport("tcp").OrDefault() = %q, want "tcp"`, got)
	}
}

func TestValidateNil(t *testing.T) {
	var def *ServerDef
	if err := def.Validate(); err == nil {
		t.Error("(*ServerDef)(nil).Validate() = nil, want an error")
	}
}

// TestJSONShape pins the wire names, since definitions are reported to
// agents through the JSON envelope of PLAN §4 and the field names are
// the same snake_case as the TOML they came from.
func TestJSONShape(t *testing.T) {
	def, err := Parse([]byte(planExample))
	if err != nil {
		t.Fatalf("Parse() = %v", err)
	}
	out, err := json.Marshal(def)
	if err != nil {
		t.Fatalf("json.Marshal() = %v", err)
	}
	for _, want := range []string{
		`"schema_version":1`,
		`"name":"gopls"`,
		`"root_markers":`,
		`"initialization_options":`,
		`"mise":"go:golang.org/x/tools/gopls@v0.23.0"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("marshalled definition %s does not contain %s", out, want)
		}
	}
}

// TestCloneNestedArrays covers the container case the settings blob
// can contain: an array of tables inside a settings table must be
// copied, not shared.
func TestCloneNestedArrays(t *testing.T) {
	def := &ServerDef{
		SchemaVersion: SchemaVersion,
		Name:          "arrays",
		Activation:    Activation{Languages: []string{"go"}},
		Server: Server{
			Command: []string{"srv"},
			Settings: map[string]any{
				"paths": []any{"a", map[string]any{"b": int64(1)}},
			},
		},
	}
	clone := def.Clone()
	cloned := clone.Server.Settings["paths"].([]any)
	cloned[0] = "mutated"
	cloned[1].(map[string]any)["b"] = int64(2)

	orig := def.Server.Settings["paths"].([]any)
	if orig[0] != "a" {
		t.Error("Clone() shares an array inside settings with the original")
	}
	if orig[1].(map[string]any)["b"] != int64(1) {
		t.Error("Clone() shares a table inside an array with the original")
	}
}

// TestValidateFieldErrors covers the checks that no TOML fixture
// naturally reaches, because Go literals can be sloppier than TOML.
func TestValidateFieldErrors(t *testing.T) {
	valid := func() *ServerDef {
		return &ServerDef{
			SchemaVersion: SchemaVersion,
			Name:          "srv",
			Activation:    Activation{Languages: []string{"go"}},
			Server:        Server{Command: []string{"srv"}},
		}
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("the baseline definition does not validate: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*ServerDef)
		want   string
	}{
		{"empty language", func(d *ServerDef) { d.Activation.Languages = []string{""} }, "activation.languages[0] is empty"},
		{"empty root marker", func(d *ServerDef) { d.Activation.RootMarkers = []string{"go.mod", ""} }, "activation.root_markers[1] is empty"},
		{"empty command word", func(d *ServerDef) { d.Server.Command = []string{""} }, "server.command[0] is empty"},
		{"bad transport", func(d *ServerDef) { d.Server.Transport = "tcp" }, "unsupported server.transport"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			def := valid()
			tt.mutate(def)
			err := def.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want an error mentioning %q", tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}
