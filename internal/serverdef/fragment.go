package serverdef

import (
	"fmt"
	"slices"
)

// The dotted TOML keys a configuration file can set, and therefore the
// unit of override in the layering of PLAN §6. A [Fragment] records
// exactly which of these a file mentioned, so that a user override of
// one key does not silently blank the rest of the definition.
const (
	KeyLanguages             = "activation.languages"
	KeyGlobs                 = "activation.globs"
	KeyRootMarkers           = "activation.root_markers"
	KeyPriority              = "activation.priority"
	KeyCommand               = "server.command"
	KeyTransport             = "server.transport"
	KeyInitializationOptions = "server.initialization_options"
	KeySettings              = "server.settings"
	KeyInstallMise           = "install.mise"
)

// overrideKeys is every key of the constants above, in the order a
// definition file writes them. Reports iterate this rather than a map,
// so that two runs describe the same override the same way.
var overrideKeys = []string{
	KeyLanguages,
	KeyGlobs,
	KeyRootMarkers,
	KeyPriority,
	KeyCommand,
	KeyTransport,
	KeyInitializationOptions,
	KeySettings,
	KeyInstallMise,
}

// A Fragment is one definition as a single configuration file spelled
// it: a name, the file's schema version, and values for just the keys
// the file set. It is the currency of the layered loader — the
// workspace file, a servers.d file and the built-in table are all
// reduced to fragments and folded together in layer order.
//
// A fragment is not a definition: it may be missing a command, or
// activation, or both. Only the result of [Fragment.ApplyTo] is
// validated.
type Fragment struct {
	// Name is the server this fragment describes.
	Name string
	// SchemaVersion is the file's schema_version, always
	// [SchemaVersion] for a fragment that parsed.
	SchemaVersion int

	def ServerDef
	set map[string]bool
}

// NewFragment builds a fragment that sets every key of def, which is
// how a whole definition — a built-in, or a complete file — enters the
// layering.
func NewFragment(def *ServerDef) *Fragment {
	f := &Fragment{Name: def.Name, SchemaVersion: def.SchemaVersion, def: *def.Clone(), set: map[string]bool{}}
	for _, key := range overrideKeys {
		f.set[key] = true
	}
	return f
}

// Has reports whether the fragment set the given key, one of the Key
// constants of this package.
func (f *Fragment) Has(key string) bool { return f.set[key] }

// Keys returns the keys the fragment set, in definition-file order.
func (f *Fragment) Keys() []string {
	out := make([]string, 0, len(f.set))
	for _, key := range overrideKeys {
		if f.set[key] {
			out = append(out, key)
		}
	}
	return out
}

// Empty reports whether the fragment sets nothing at all, which is a
// file that mentions a server and then says nothing about it.
func (f *Fragment) Empty() bool { return len(f.set) == 0 }

// ApplyTo folds the fragment onto base and returns the result; base is
// never modified. A nil base means the fragment is the first mention of
// this server, so the result is built from the fragment alone.
//
// Merge rules, chosen so that an override can always be predicted from
// the file alone:
//
//   - a key the fragment did not set is inherited from base;
//   - arrays and scalars replace — a shorter languages list is how you
//     take a language away, and merging would make removal impossible;
//   - the two free-form tables, server.initialization_options and
//     server.settings, merge key by key and recursively, because they
//     are collections of independent knobs and nobody wants to restate
//     a server's whole settings tree to change one of them. There is
//     therefore no way to unset a single setting; set it to the value
//     you want instead.
func (f *Fragment) ApplyTo(base *ServerDef) *ServerDef {
	var out *ServerDef
	if base == nil {
		out = &ServerDef{
			SchemaVersion: SchemaVersion,
			Name:          f.Name,
			Activation:    Activation{Priority: DefaultPriority},
			Server:        Server{Transport: TransportStdio},
		}
	} else {
		out = base.Clone()
	}
	out.Name = f.Name

	if f.set[KeyLanguages] {
		out.Activation.Languages = cloneStrings(f.def.Activation.Languages)
	}
	if f.set[KeyGlobs] {
		out.Activation.Globs = cloneStrings(f.def.Activation.Globs)
	}
	if f.set[KeyRootMarkers] {
		out.Activation.RootMarkers = cloneStrings(f.def.Activation.RootMarkers)
	}
	if f.set[KeyPriority] {
		out.Activation.Priority = f.def.Activation.Priority
	}
	if f.set[KeyCommand] {
		out.Server.Command = cloneStrings(f.def.Server.Command)
	}
	if f.set[KeyTransport] {
		out.Server.Transport = f.def.Server.Transport
	}
	if f.set[KeyInitializationOptions] {
		out.Server.InitializationOptions = mergeTables(out.Server.InitializationOptions, f.def.Server.InitializationOptions)
	}
	if f.set[KeySettings] {
		out.Server.Settings = mergeTables(out.Server.Settings, f.def.Server.Settings)
	}
	if f.set[KeyInstallMise] {
		out.Install.Mise = f.def.Install.Mise
	}
	return out
}

// mergeTables deep-merges over onto base without touching either. A
// table value merges recursively; anything else replaces.
func mergeTables(base, over map[string]any) map[string]any {
	if over == nil {
		return cloneTable(base)
	}
	out := cloneTable(base)
	if out == nil {
		out = make(map[string]any, len(over))
	}
	keys := make([]string, 0, len(over))
	for k := range over {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v := over[k]
		if sub, ok := v.(map[string]any); ok {
			if existing, ok := out[k].(map[string]any); ok {
				out[k] = mergeTables(existing, sub)
				continue
			}
		}
		out[k] = cloneValue(v)
	}
	return out
}

// validateName checks a definition name in isolation, which the layered
// loader needs before it has a whole definition to validate: the name
// is the key everything else is filed under.
func validateName(name string) error {
	if name == "" {
		return fmt.Errorf("server definition: name is required")
	}
	if !nameRE.MatchString(name) {
		return fmt.Errorf("server definition: name %q must start with a letter or digit and contain only letters, digits and any of .+-_", name)
	}
	return nil
}
