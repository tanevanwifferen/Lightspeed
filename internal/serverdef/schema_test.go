package serverdef

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// schemaPath is the published JSON Schema for the TOML files this
// package parses. It is checked in at the repository root, so it is
// reached relatively from the package directory `go test` runs in.
const schemaPath = "../../schema/serverdef.schema.json"

// TestSchemaMatchesTheParser keeps schema/serverdef.schema.json honest.
// lightspeed has no JSON-Schema validator (and no third-party
// dependencies to get one from, per docs/DECISIONS.md D3), so the
// schema cannot be enforced at runtime — which makes it exactly the
// kind of file that silently rots. What can be checked is that it
// describes the same keys the parser accepts, and that is the drift
// that actually misleads someone writing a .lightspeed.toml.
func TestSchemaMatchesTheParser(t *testing.T) {
	data, err := os.ReadFile(filepath.FromSlash(schemaPath))
	if err != nil {
		t.Fatalf("reading the published schema: %v", err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatalf("the published schema is not valid JSON: %v", err)
	}

	defs, _ := schema["$defs"].(map[string]any)
	if defs == nil {
		t.Fatal("the schema has no $defs")
	}

	// Each table's properties, against the parser's own allow-lists.
	// These are the lists checkKeys is called with; if one gains a key
	// the schema has to gain it too.
	for _, tt := range []struct {
		where string
		def   string
		want  []string
	}{
		{"[activation]", "activation", []string{"languages", "globs", "root_markers", "priority"}},
		{"[server]", "server", []string{"command", "transport", "initialization_options", "settings"}},
		{"[install]", "install", []string{"mise"}},
		{"[servers.<name>]", "definition", []string{"activation", "server", "install"}},
	} {
		got := propertyNames(t, defs[tt.def])
		want := slices.Sorted(slices.Values(tt.want))
		if !slices.Equal(got, want) {
			t.Errorf("schema $defs.%s describes %v for %s, but the parser accepts %v", tt.def, got, tt.where, want)
		}
	}

	// The top level, which additionally carries schema_version, name
	// and the multi-server table.
	top := propertyNames(t, schema)
	want := slices.Sorted(slices.Values([]string{"schema_version", "name", "servers", "activation", "server", "install"}))
	if !slices.Equal(top, want) {
		t.Errorf("schema top level describes %v, but the parser accepts %v", top, want)
	}

	// The parser rejects unknown keys everywhere, so the schema must
	// too: a schema that quietly allowed them would validate a file
	// lightspeed refuses.
	for _, subject := range []any{schema, defs["activation"], defs["server"], defs["install"], defs["definition"]} {
		obj, _ := subject.(map[string]any)
		if extra, ok := obj["additionalProperties"].(bool); !ok || extra {
			t.Errorf("schema object %v does not set additionalProperties:false, but the parser rejects unknown keys", obj["title"])
		}
	}

	// schema_version is the one key every file must carry, whichever
	// shape it uses, because it is what makes the file readable at all.
	required, _ := schema["required"].([]any)
	if len(required) != 1 || required[0] != "schema_version" {
		t.Errorf("schema requires %v at the top level, want only schema_version: every other key is optional in a fragment", required)
	}

	// The version the schema pins is the version this build implements.
	props, _ := schema["properties"].(map[string]any)
	version, _ := props["schema_version"].(map[string]any)
	if got, ok := version["const"].(float64); !ok || int(got) != SchemaVersion {
		t.Errorf("schema pins schema_version %v, want %d", version["const"], SchemaVersion)
	}

	// The name pattern is the one Validate enforces.
	name, _ := defs["name"].(map[string]any)
	if got, want := name["pattern"], nameRE.String(); got != want {
		t.Errorf("schema name pattern = %v, want %q from nameRE", got, want)
	}

	// The only transport is the only transport.
	server, _ := defs["server"].(map[string]any)
	serverProps, _ := server["properties"].(map[string]any)
	transport, _ := serverProps["transport"].(map[string]any)
	enum, _ := transport["enum"].([]any)
	if len(enum) != 1 || enum[0] != string(TransportStdio) {
		t.Errorf("schema transport enum = %v, want only %q", enum, TransportStdio)
	}
}

// TestSchemaAcceptsThePlanExample is a sanity check on the shapes rather
// than on the keys: both file forms this package documents parse, so the
// schema is describing files that actually exist.
func TestSchemaAcceptsThePlanExample(t *testing.T) {
	// The example of PLAN §6, verbatim in shape.
	single := `schema_version = 1
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
mise = "go:golang.org/x/tools/gopls@v0.23.0"
`
	if _, err := Parse([]byte(single)); err != nil {
		t.Errorf("the PLAN §6 example does not parse: %v", err)
	}

	multi := `schema_version = 1

[servers.gopls.activation]
priority = 60

[servers.vtsls.server]
command = ["vtsls", "--stdio"]
`
	frags, err := ParseFragments([]byte(multi))
	if err != nil {
		t.Fatalf("the multi-server shape does not parse: %v", err)
	}
	if len(frags) != 2 {
		t.Errorf("parsed %d fragments, want 2", len(frags))
	}
}

// propertyNames lists the "properties" keys of a schema object, sorted.
func propertyNames(t *testing.T, node any) []string {
	t.Helper()
	obj, ok := node.(map[string]any)
	if !ok {
		t.Fatalf("schema node is %T, want an object", node)
	}
	props, ok := obj["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema node has no properties: %v", obj)
	}
	out := make([]string, 0, len(props))
	for k := range props {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}
