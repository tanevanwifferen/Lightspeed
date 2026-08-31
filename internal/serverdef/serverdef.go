package serverdef

import (
	"fmt"
	"regexp"
)

// SchemaVersion is the only server-definition schema version this
// build understands. It is the `schema_version` key of PLAN §6 and of
// schema/serverdef.schema.json.
const SchemaVersion = 1

// DefaultPriority is the priority assumed for a definition whose
// activation table omits `priority`. It matches the built-in
// definitions, so an override wanting to win must say so explicitly.
const DefaultPriority = 50

// Transport is how the client talks to the server process.
type Transport string

// TransportStdio is the only transport of v1: JSON-RPC over the
// server's stdin/stdout.
const TransportStdio Transport = "stdio"

// OrDefault reports the transport to use, treating the empty value as
// [TransportStdio] so that a definition may omit the key.
func (t Transport) OrDefault() Transport {
	if t == "" {
		return TransportStdio
	}
	return t
}

// A ServerDef is one language server definition: what it activates
// on, how to run it, and how to install it. It is pure data — see
// the package documentation for what is deliberately not here.
type ServerDef struct {
	SchemaVersion int        `json:"schema_version"`
	Name          string     `json:"name"`
	Activation    Activation `json:"activation"`
	Server        Server     `json:"server"`
	Install       Install    `json:"install"`
}

// Activation says which files a server claims and how strongly.
type Activation struct {
	// Languages are LSP language identifiers ("go", "rust",
	// "python"). A file whose language id is listed is claimed.
	Languages []string `json:"languages,omitempty"`

	// Globs are path patterns ("**/*.go"). A file whose path
	// matches is claimed, independently of Languages. Patterns are
	// interpreted relative to the resolved workspace root unless
	// they begin with "/"; internal/router owns the matching rules
	// and rejects malformed patterns.
	Globs []string `json:"globs,omitempty"`

	// RootMarkers are the file or directory names whose presence
	// marks a workspace root ("go.work", "go.mod", ".git"), most
	// significant first. internal/router walks up from the file to
	// find one.
	RootMarkers []string `json:"root_markers,omitempty"`

	// Priority orders servers that claim the same file: higher runs
	// first. Absent in TOML means [DefaultPriority].
	Priority int `json:"priority"`
}

// Server is how to start the server process.
type Server struct {
	// Command is argv, command first. It is not passed through a
	// shell.
	Command []string `json:"command,omitempty"`

	// Transport is the framing; empty means [TransportStdio].
	Transport Transport `json:"transport,omitempty"`

	// InitializationOptions is sent verbatim as the
	// `initializationOptions` field of the LSP initialize request.
	InitializationOptions map[string]any `json:"initialization_options,omitempty"`

	// Settings is sent verbatim as the `settings` field of
	// workspace/didChangeConfiguration, which several servers need
	// after initialize (PLAN §5.4).
	Settings map[string]any `json:"settings,omitempty"`
}

// Install says how to obtain the server. Only mise delegation exists;
// the string is a mise tool spec such as
// "go:golang.org/x/tools/gopls@v0.23.0" and is never executed by this
// package.
type Install struct {
	Mise string `json:"mise,omitempty"`
}

// nameRE constrains definition names because a name is used as a
// human-facing identifier and, later, as part of a session key.
var nameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*$`)

// Validate reports whether d is a complete, usable definition. A
// definition that could never activate, or that could not be started,
// is an error rather than a silently inert entry.
//
// Validate expects a whole definition. Partial fragments — the user
// overriding one key of a built-in — are only meaningful after the M4
// layering merges them, and must be validated then, not before.
func (d *ServerDef) Validate() error {
	if d == nil {
		return fmt.Errorf("server definition: is nil")
	}
	if d.Name == "" {
		return fmt.Errorf("server definition: name is required")
	}
	if !nameRE.MatchString(d.Name) {
		return fmt.Errorf("server definition: name %q must start with a letter or digit and contain only letters, digits and any of .+-_", d.Name)
	}
	if d.SchemaVersion != SchemaVersion {
		if d.SchemaVersion == 0 {
			return fmt.Errorf("server definition %q: schema_version is required (expected %d)", d.Name, SchemaVersion)
		}
		return fmt.Errorf("server definition %q: unsupported schema_version %d (expected %d)", d.Name, d.SchemaVersion, SchemaVersion)
	}
	if len(d.Activation.Languages) == 0 && len(d.Activation.Globs) == 0 {
		return fmt.Errorf("server definition %q: activation needs at least one of languages or globs, otherwise the server can never be selected", d.Name)
	}
	for i, lang := range d.Activation.Languages {
		if lang == "" {
			return fmt.Errorf("server definition %q: activation.languages[%d] is empty", d.Name, i)
		}
	}
	for i, glob := range d.Activation.Globs {
		if glob == "" {
			return fmt.Errorf("server definition %q: activation.globs[%d] is empty", d.Name, i)
		}
	}
	for i, marker := range d.Activation.RootMarkers {
		if marker == "" {
			return fmt.Errorf("server definition %q: activation.root_markers[%d] is empty", d.Name, i)
		}
	}
	if len(d.Server.Command) == 0 {
		return fmt.Errorf("server definition %q: server.command is required", d.Name)
	}
	if d.Server.Command[0] == "" {
		return fmt.Errorf("server definition %q: server.command[0] is empty", d.Name)
	}
	if t := d.Server.Transport.OrDefault(); t != TransportStdio {
		return fmt.Errorf("server definition %q: unsupported server.transport %q (only %q)", d.Name, t, TransportStdio)
	}
	return nil
}

// Clone returns a deep copy of d. Callers get definitions they may
// mutate — layering in M4 merges by copying — without reaching into
// shared state such as the built-in table.
func (d *ServerDef) Clone() *ServerDef {
	if d == nil {
		return nil
	}
	c := *d
	c.Activation.Languages = cloneStrings(d.Activation.Languages)
	c.Activation.Globs = cloneStrings(d.Activation.Globs)
	c.Activation.RootMarkers = cloneStrings(d.Activation.RootMarkers)
	c.Server.Command = cloneStrings(d.Server.Command)
	c.Server.InitializationOptions = cloneTable(d.Server.InitializationOptions)
	c.Server.Settings = cloneTable(d.Server.Settings)
	return &c
}

func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func cloneTable(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = cloneValue(v)
	}
	return out
}

// cloneValue deep-copies a decoded TOML value: the containers are
// maps and slices, the leaves are immutable scalars.
func cloneValue(v any) any {
	switch v := v.(type) {
	case map[string]any:
		return cloneTable(v)
	case []any:
		out := make([]any, len(v))
		for i, e := range v {
			out[i] = cloneValue(e)
		}
		return out
	default:
		return v
	}
}
