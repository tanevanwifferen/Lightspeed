package serverdef

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// Parse decodes exactly one server definition from the TOML bytes of
// PLAN §6 and validates it. Unknown keys are an error: a typo in
// `root_markers` must not silently produce a server that never
// activates.
//
// Errors from the TOML layer name the line; errors from decoding name
// the dotted key. Neither touches the filesystem, so the caller is
// free to wrap the error with whatever it read the bytes from.
func Parse(data []byte) (*ServerDef, error) {
	raw, err := parseTOML(data)
	if err != nil {
		return nil, fmt.Errorf("invalid TOML: %w", err)
	}
	def, err := decode(raw)
	if err != nil {
		return nil, err
	}
	if err := def.Validate(); err != nil {
		return nil, err
	}
	return def, nil
}

func decode(raw map[string]any) (*ServerDef, error) {
	if err := checkKeys("", raw, "schema_version", "name", "activation", "server", "install"); err != nil {
		return nil, err
	}
	def := &ServerDef{Activation: Activation{Priority: DefaultPriority}}

	var err error
	if def.SchemaVersion, err = intKey("", raw, "schema_version", 0); err != nil {
		return nil, err
	}
	if def.Name, err = stringKey("", raw, "name", ""); err != nil {
		return nil, err
	}

	act, err := tableKey("", raw, "activation")
	if err != nil {
		return nil, err
	}
	if act != nil {
		if err := decodeActivation(act, &def.Activation); err != nil {
			return nil, err
		}
	}

	srv, err := tableKey("", raw, "server")
	if err != nil {
		return nil, err
	}
	if srv != nil {
		if err := decodeServer(srv, &def.Server); err != nil {
			return nil, err
		}
	}

	inst, err := tableKey("", raw, "install")
	if err != nil {
		return nil, err
	}
	if inst != nil {
		if err := checkKeys("install", inst, "mise"); err != nil {
			return nil, err
		}
		if def.Install.Mise, err = stringKey("install", inst, "mise", ""); err != nil {
			return nil, err
		}
	}
	return def, nil
}

func decodeActivation(raw map[string]any, out *Activation) error {
	if err := checkKeys("activation", raw, "languages", "globs", "root_markers", "priority"); err != nil {
		return err
	}
	var err error
	if out.Languages, err = stringsKey("activation", raw, "languages"); err != nil {
		return err
	}
	if out.Globs, err = stringsKey("activation", raw, "globs"); err != nil {
		return err
	}
	if out.RootMarkers, err = stringsKey("activation", raw, "root_markers"); err != nil {
		return err
	}
	// Absent priority means the default, not zero.
	if out.Priority, err = intKey("activation", raw, "priority", DefaultPriority); err != nil {
		return err
	}
	return nil
}

func decodeServer(raw map[string]any, out *Server) error {
	if err := checkKeys("server", raw, "command", "transport", "initialization_options", "settings"); err != nil {
		return err
	}
	var err error
	if out.Command, err = stringsKey("server", raw, "command"); err != nil {
		return err
	}
	transport, err := stringKey("server", raw, "transport", string(TransportStdio))
	if err != nil {
		return err
	}
	out.Transport = Transport(transport)
	if out.InitializationOptions, err = tableKey("server", raw, "initialization_options"); err != nil {
		return err
	}
	if out.Settings, err = tableKey("server", raw, "settings"); err != nil {
		return err
	}
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
