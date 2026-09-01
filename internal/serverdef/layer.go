package serverdef

import "fmt"

// Names and environment variables of the configuration layers of
// PLAN §6.
const (
	// WorkspaceFile is the in-tree, version-controlled configuration
	// file, looked for in the workspace root. It is the only file
	// PLAN §6 asks a user to write.
	WorkspaceFile = ".lightspeed.toml"

	// ConfigSubdir is lightspeed's directory inside the user's
	// configuration home.
	ConfigSubdir = "lightspeed"

	// ServersDir is the directory of user overrides inside the
	// configuration directory. Every *.toml in it is loaded.
	ServersDir = "servers.d"

	// EnvConfigHome is the XDG variable that locates the user layer.
	EnvConfigHome = "XDG_CONFIG_HOME"

	// EnvConfigDir overrides the whole user configuration directory,
	// XDG or not. It exists because a CLI needs a flag-equivalent
	// escape hatch, and because tests need one.
	EnvConfigDir = "LIGHTSPEED_CONFIG_DIR"

	// EnvOffline is PLAN §6's hard network kill switch. Set it to 1
	// (or true/yes/on) and nothing will reach the network, whatever
	// the flags say.
	EnvOffline = "LIGHTSPEED_OFFLINE"
)

// A Layer is one source of server definitions. The three that can
// supply a definition are ordered by strength: [LayerWorkspace] beats
// [LayerUser] beats [LayerBuiltin], key by key.
//
// PLAN §6's fourth item, PATH sniffing, supplies executables rather
// than definitions — a binary on PATH is only meaningful once some
// layer has said what to do with it — so it is a [BinarySource] and
// not a Layer. Zero configuration therefore means: definition from
// [LayerBuiltin], executable from [BinaryPATH].
type Layer int

// The definition layers, strongest first.
const (
	// LayerUnknown is the zero value, used by no real definition.
	LayerUnknown Layer = iota
	// LayerWorkspace is .lightspeed.toml in the workspace root.
	LayerWorkspace
	// LayerUser is $XDG_CONFIG_HOME/lightspeed/servers.d/*.toml.
	LayerUser
	// LayerBuiltin is the table generated from the vendored corpus at
	// build time (internal/gen).
	LayerBuiltin
)

// Layers returns the definition layers, strongest first, which is the
// order reports list them in.
func Layers() []Layer { return []Layer{LayerWorkspace, LayerUser, LayerBuiltin} }

// String is the short machine-ish name of the layer, stable enough to
// appear in JSON output.
func (l Layer) String() string {
	switch l {
	case LayerWorkspace:
		return "workspace"
	case LayerUser:
		return "user"
	case LayerBuiltin:
		return "builtin"
	default:
		return "unknown"
	}
}

// Describe is the human sentence fragment for the layer, naming the
// file it comes from, for `doctor` output.
func (l Layer) Describe() string {
	switch l {
	case LayerWorkspace:
		return "workspace " + WorkspaceFile
	case LayerUser:
		return "user override in " + ServersDir
	case LayerBuiltin:
		return "built-in default"
	default:
		return "unknown layer"
	}
}

// stronger reports whether l overrides other.
func (l Layer) stronger(other Layer) bool {
	if other == LayerUnknown {
		return l != LayerUnknown
	}
	return l != LayerUnknown && l < other
}

// An Origin is where one fragment came from: which layer, and which
// file within it.
type Origin struct {
	// Layer is the configuration layer.
	Layer Layer `json:"layer"`
	// File is the absolute path of the file, empty for
	// [LayerBuiltin], whose definitions are compiled in.
	File string `json:"file,omitempty"`
}

func (o Origin) String() string {
	if o.File == "" {
		return o.Layer.Describe()
	}
	return fmt.Sprintf("%s (%s layer)", o.File, o.Layer)
}

// An Override records one layer's contribution to a definition: where
// it came from and which keys it set. It is what makes the winning
// layer inspectable, which `doctor` needs in order to explain why a
// server is configured the way it is.
type Override struct {
	// Origin is the contributing file or table.
	Origin Origin `json:"origin"`
	// Keys are the dotted keys this layer set, in definition order.
	// A whole definition sets all of them.
	Keys []string `json:"keys"`
	// Whole reports that this contribution was a complete definition
	// rather than a partial override.
	Whole bool `json:"whole"`
}

func (o Override) String() string {
	if o.Whole {
		return fmt.Sprintf("%s: whole definition", o.Origin)
	}
	return fmt.Sprintf("%s: %v", o.Origin, o.Keys)
}
