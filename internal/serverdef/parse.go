package serverdef

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Parse decodes exactly one whole server definition from the TOML
// bytes of PLAN §6 and validates it. Unknown keys are an error: a typo
// in `root_markers` must not silently produce a server that never
// activates.
//
// A file that carries a [servers.<name>] table, or more than one
// definition, is an error here: use [ParseFragments], which is what
// the config layers of PLAN §6 use and which also accepts the partial
// overrides Parse rejects.
//
// Errors from the TOML layer name the line; errors from decoding name
// the dotted key. Neither touches the filesystem, so the caller is
// free to wrap the error with whatever it read the bytes from.
func Parse(data []byte) (*ServerDef, error) {
	frags, err := ParseFragments(data)
	if err != nil {
		return nil, err
	}
	switch len(frags) {
	case 0:
		return nil, fmt.Errorf("server definition: name is required")
	case 1:
	default:
		names := make([]string, len(frags))
		for i, f := range frags {
			names[i] = f.Name
		}
		return nil, fmt.Errorf("expected one server definition, found %d (%s); use the [servers.<name>] form with a loader that accepts it",
			len(frags), strings.Join(names, ", "))
	}
	def := frags[0].ApplyTo(nil)
	if err := def.Validate(); err != nil {
		return nil, err
	}
	return def, nil
}

// ParseFragments decodes every definition in one configuration file
// into a [Fragment]: the keys the file actually set, and nothing more.
// It is the parser the layered loader uses, because a user override
// that sets one key must be told apart from one that sets a key to an
// empty value.
//
// Two file shapes are accepted, and a file may not mix them:
//
//	schema_version = 1        one definition, the shape of PLAN §6
//	name = "gopls"
//	[activation]
//	priority = 60
//
//	schema_version = 1        several definitions, named by table key
//	[servers.gopls.activation]
//	priority = 60
//	[servers.vtsls.server]
//	command = ["vtsls", "--stdio"]
//
// Fragments come back ordered by name. Nothing is validated beyond the
// file's own syntax and its names: completeness is only meaningful
// after layering, so [Fragment.ApplyTo] output is what gets validated.
func ParseFragments(data []byte) ([]*Fragment, error) {
	raw, err := parseTOML(data)
	if err != nil {
		return nil, fmt.Errorf("invalid TOML: %w", err)
	}
	return decodeFile(raw)
}

func decodeFile(raw map[string]any) ([]*Fragment, error) {
	if err := checkKeys("", raw, "schema_version", "name", "servers", "activation", "server", "install"); err != nil {
		return nil, err
	}

	version, err := intKey("", raw, "schema_version", 0)
	if err != nil {
		return nil, err
	}
	if version != SchemaVersion {
		if version == 0 {
			return nil, fmt.Errorf("schema_version is required (expected %d)", SchemaVersion)
		}
		return nil, fmt.Errorf("unsupported schema_version %d (expected %d)", version, SchemaVersion)
	}

	servers, err := tableKey("", raw, "servers")
	if err != nil {
		return nil, err
	}
	if servers == nil {
		frag, err := decodeSingle(raw, version)
		if err != nil {
			return nil, err
		}
		return []*Fragment{frag}, nil
	}
	return decodeMulti(raw, servers, version)
}

// decodeSingle handles the one-definition file shape of PLAN §6.
func decodeSingle(raw map[string]any, version int) (*Fragment, error) {
	name, err := stringKey("", raw, "name", "")
	if err != nil {
		return nil, err
	}
	if name == "" {
		return nil, fmt.Errorf("server definition: name is required")
	}
	return decodeFragment("", name, version, raw)
}

// decodeMulti handles the [servers.<name>] file shape, which exists so
// that one .lightspeed.toml can configure a polyglot repo.
func decodeMulti(raw, servers map[string]any, version int) ([]*Fragment, error) {
	for _, forbidden := range []string{"name", "activation", "server", "install"} {
		if _, ok := raw[forbidden]; ok {
			return nil, fmt.Errorf("a file with a [servers.<name>] table cannot also set %q at the top level: move it into the server's own table", forbidden)
		}
	}
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	slices.Sort(names)

	frags := make([]*Fragment, 0, len(names))
	for _, name := range names {
		prefix := "servers." + name
		tbl, err := tableKey("servers", servers, name)
		if err != nil {
			return nil, err
		}
		if _, ok := tbl["name"]; ok {
			return nil, fmt.Errorf("[%s]: the server is named by the table key; remove the name key", prefix)
		}
		frag, err := decodeFragment(prefix, name, version, tbl)
		if err != nil {
			return nil, err
		}
		frags = append(frags, frag)
	}
	return frags, nil
}

// decodeFragment decodes one definition's tables, recording which keys
// were present. prefix is the dotted path of tbl for error messages,
// empty at the top level of a single-definition file.
func decodeFragment(prefix, name string, version int, tbl map[string]any) (*Fragment, error) {
	allowed := []string{"activation", "server", "install"}
	if prefix == "" {
		allowed = append([]string{"schema_version", "name"}, allowed...)
	}
	if err := checkKeys(prefix, tbl, allowed...); err != nil {
		return nil, err
	}
	if err := validateName(name); err != nil {
		return nil, err
	}

	f := &Fragment{Name: name, SchemaVersion: version, set: map[string]bool{}}

	act, err := tableKey(prefix, tbl, "activation")
	if err != nil {
		return nil, err
	}
	if act != nil {
		if err := f.decodeActivation(qualify(prefix, "activation"), act); err != nil {
			return nil, err
		}
	}

	srv, err := tableKey(prefix, tbl, "server")
	if err != nil {
		return nil, err
	}
	if srv != nil {
		if err := f.decodeServer(qualify(prefix, "server"), srv); err != nil {
			return nil, err
		}
	}

	inst, err := tableKey(prefix, tbl, "install")
	if err != nil {
		return nil, err
	}
	if inst != nil {
		if err := f.decodeInstall(qualify(prefix, "install"), inst); err != nil {
			return nil, err
		}
	}
	return f, nil
}

func (f *Fragment) decodeActivation(prefix string, raw map[string]any) error {
	if err := checkKeys(prefix, raw, "languages", "globs", "root_markers", "priority"); err != nil {
		return err
	}
	var err error
	if _, ok := raw["languages"]; ok {
		if f.def.Activation.Languages, err = stringsKey(prefix, raw, "languages"); err != nil {
			return err
		}
		f.set[KeyLanguages] = true
	}
	if _, ok := raw["globs"]; ok {
		if f.def.Activation.Globs, err = stringsKey(prefix, raw, "globs"); err != nil {
			return err
		}
		f.set[KeyGlobs] = true
	}
	if _, ok := raw["root_markers"]; ok {
		if f.def.Activation.RootMarkers, err = stringsKey(prefix, raw, "root_markers"); err != nil {
			return err
		}
		f.set[KeyRootMarkers] = true
	}
	if _, ok := raw["priority"]; ok {
		// Absent means "inherit"; present means exactly what it says,
		// including zero.
		if f.def.Activation.Priority, err = intKey(prefix, raw, "priority", DefaultPriority); err != nil {
			return err
		}
		f.set[KeyPriority] = true
	}
	return nil
}

func (f *Fragment) decodeServer(prefix string, raw map[string]any) error {
	if err := checkKeys(prefix, raw, "command", "transport", "initialization_options", "settings"); err != nil {
		return err
	}
	var err error
	if _, ok := raw["command"]; ok {
		if f.def.Server.Command, err = stringsKey(prefix, raw, "command"); err != nil {
			return err
		}
		f.set[KeyCommand] = true
	}
	if _, ok := raw["transport"]; ok {
		var transport string
		if transport, err = stringKey(prefix, raw, "transport", string(TransportStdio)); err != nil {
			return err
		}
		f.def.Server.Transport = Transport(transport)
		f.set[KeyTransport] = true
	}
	if _, ok := raw["initialization_options"]; ok {
		if f.def.Server.InitializationOptions, err = tableKey(prefix, raw, "initialization_options"); err != nil {
			return err
		}
		f.set[KeyInitializationOptions] = true
	}
	if _, ok := raw["settings"]; ok {
		if f.def.Server.Settings, err = tableKey(prefix, raw, "settings"); err != nil {
			return err
		}
		f.set[KeySettings] = true
	}
	return nil
}

func (f *Fragment) decodeInstall(prefix string, raw map[string]any) error {
	if err := checkKeys(prefix, raw, "mise"); err != nil {
		return err
	}
	if _, ok := raw["mise"]; !ok {
		return nil
	}
	var err error
	if f.def.Install.Mise, err = stringKey(prefix, raw, "mise", ""); err != nil {
		return err
	}
	f.set[KeyInstallMise] = true
	return nil
}

// checkKeys rejects any key of tbl that is not allowed, naming the
// allowed set so the error is actionable.
func checkKeys(prefix string, tbl map[string]any, allowed ...string) error {
	var unknown []string
	for k := range tbl {
		if !slices.Contains(allowed, k) {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	slices.Sort(unknown)
	where := "top level"
	if prefix != "" {
		where = "[" + prefix + "]"
	}
	return fmt.Errorf("unknown key %q in %s (known keys: %s)", unknown[0], where, strings.Join(allowed, ", "))
}

func stringKey(prefix string, tbl map[string]any, key, fallback string) (string, error) {
	v, ok := tbl[key]
	if !ok {
		return fallback, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", typeError(prefix, key, "a string", v)
	}
	return s, nil
}

func intKey(prefix string, tbl map[string]any, key string, fallback int) (int, error) {
	v, ok := tbl[key]
	if !ok {
		return fallback, nil
	}
	n, ok := v.(int64)
	if !ok {
		return 0, typeError(prefix, key, "an integer", v)
	}
	if n > math.MaxInt32 || n < math.MinInt32 {
		return 0, fmt.Errorf("%s: %d is out of range", qualify(prefix, key), n)
	}
	return int(n), nil
}

func stringsKey(prefix string, tbl map[string]any, key string) ([]string, error) {
	v, ok := tbl[key]
	if !ok {
		return nil, nil
	}
	list, ok := v.([]any)
	if !ok {
		return nil, typeError(prefix, key, "an array of strings", v)
	}
	out := make([]string, 0, len(list))
	for i, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, typeError(prefix, fmt.Sprintf("%s[%d]", key, i), "a string", e)
		}
		out = append(out, s)
	}
	return out, nil
}

func tableKey(prefix string, tbl map[string]any, key string) (map[string]any, error) {
	v, ok := tbl[key]
	if !ok {
		return nil, nil
	}
	t, ok := v.(map[string]any)
	if !ok {
		return nil, typeError(prefix, key, "a table", v)
	}
	return t, nil
}

func typeError(prefix, key, want string, got any) error {
	return fmt.Errorf("%s: expected %s, found %s", qualify(prefix, key), want, tomlTypeName(got))
}

func tomlTypeName(v any) string {
	switch v.(type) {
	case string:
		return "a string"
	case int64:
		return "an integer"
	case float64:
		return "a float"
	case bool:
		return "a boolean"
	case []any:
		return "an array"
	case map[string]any:
		return "a table"
	default:
		return fmt.Sprintf("%T", v)
	}
}
