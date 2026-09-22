package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// The declarative parameter spec of a command.
//
// A command's parameters are written down once, here, and two things read
// them: the MCP layer (mcp.go), which turns each into a typed, described
// property of a tool's input schema and each call's arguments back into the
// argument vector the command's own flag parsing understands, and
// TestParamSpecMatchesCLIFlags, which fails when a command registers a flag
// the spec does not know about or the spec names one the command does not
// have. The spec does not *drive* flag registration — each command still
// declares its flags where it reads them — so the test is what keeps the two
// surfaces from drifting (docs/DECISIONS.md D20).
//
// The consequence for a new command is that its table entry is enough: give
// it `MCP` (its tools and their parameters) or `NoMCP` (why it is not one),
// and there is no MCP code to write.

// paramType is the type of a parameter's value.
type paramType int

const (
	// typeString is a JSON string.
	typeString paramType = iota
	// typeInt is a JSON integer.
	typeInt
	// typeBool is a JSON boolean; on the command line the flag is given
	// only when it is true.
	typeBool
	// typeDuration is a Go duration ("30s") spelled as a JSON string, as
	// the flag spells it.
	typeDuration
	// typeStrings is a list of strings; only a positional can be one
	// (`format <path...>`).
	typeStrings
)

// A param is one parameter of a command: a flag, or a positional argument.
type param struct {
	// Name is the property name in the MCP input schema. A flag's default
	// is its own name with dashes turned into underscores (`allow-dirty`
	// is `allow_dirty`); a positional must name itself.
	Name string
	// Flag is the flag as typed after the dashes, "" for a positional.
	Flag string
	// Aliases are other spellings of the same flag (`references -d`).
	// They are CLI-only.
	Aliases []string
	Type    paramType
	// Desc is the description an agent reads. Terse: it is paid for in
	// every tools/list.
	Desc string
	// Required makes the parameter mandatory. It is for parameters that
	// are unconditionally needed; "a location or a symbol" is checked by
	// the command itself, which is the one place that rule is written.
	Required bool
	// Enum lists the accepted values of a string parameter.
	Enum []string
	// Min and Max bound an integer parameter.
	Min, Max *int
	// Path marks a value that is a file or directory (or, with Location, a
	// file followed by a position). A relative one is resolved against the
	// call's workspace, not against the server's working directory.
	Path bool
	// Location marks a span (`file:line:col[-line:col]`); see Path.
	Location bool
	// IDOrLocation marks a value that is a symbol id or, if it is not one, a
	// location: an id (path::Name#kind) is left as it is, because its path
	// is relative to the workspace by definition, and anything else is
	// resolved as Path values are.
	IDOrLocation bool
	// WorkspaceDefault makes an unset parameter mean the call's workspace.
	// The command line defaults such a flag to ".", the working directory
	// of the process, which is the wrong directory when a call names a
	// different workspace.
	WorkspaceDefault bool
	// CLIOnly is why this flag is not offered to an MCP client, and the
	// mark that it is not. Every flag a command registers is declared, so
	// that adding one without deciding this fails the test.
	CLIOnly string
}

// name is the parameter's property name.
func (p param) name() string {
	if p.Name != "" {
		return p.Name
	}
	return strings.ReplaceAll(p.Flag, "-", "_")
}

// exposed reports whether an MCP client sees the parameter.
func (p param) exposed() bool { return p.CLIOnly == "" }

// schemaType is the JSON Schema type of the parameter's value.
func (p param) schemaType() string {
	switch p.Type {
	case typeInt:
		return "integer"
	case typeBool:
		return "boolean"
	case typeStrings:
		return "array"
	default:
		return "string"
	}
}

// schema is the parameter's property in a tool's input schema.
func (p param) schema() map[string]any {
	s := map[string]any{"type": p.schemaType(), "description": p.Desc}
	switch p.Type {
	case typeStrings:
		s["items"] = map[string]any{"type": "string"}
		if p.Required {
			s["minItems"] = 1
		}
	case typeString:
		if len(p.Enum) > 0 {
			s["enum"] = p.Enum
		}
	case typeInt:
		if p.Min != nil {
			s["minimum"] = *p.Min
		}
		if p.Max != nil {
			s["maximum"] = *p.Max
		}
	}
	return s
}

// A toolSpec is one MCP tool a command provides. A command is usually one
// tool named after it; `daemon` is one command with several subcommands of
// which only `status` is a tool, so the two are separate things.
type toolSpec struct {
	// Name is the tool's name, and the subcommand as typed unless Prefix
	// says otherwise.
	Name string
	// Prefix is the argument vector between the command name and its
	// parameters: `daemon status` is the command `daemon` with prefix
	// {"status"}.
	Prefix []string
	// Description is what an agent is told the tool does. When the tool
	// can write, "Previews unless apply is true." is appended.
	Description string
	Params      []param
}

// mutates reports whether the tool can write: it has an `apply` flag.
// Deriving it, rather than declaring it beside the parameters, is what
// keeps the read-only hint honest — a tool that gains an --apply is no
// longer marked read-only without anyone touching the mark.
func (t toolSpec) mutates() bool {
	return slices.ContainsFunc(t.Params, func(p param) bool { return p.Flag == "apply" })
}

// description is the tool's description as listed.
func (t toolSpec) description() string {
	if t.mutates() {
		return t.Description + " Previews unless apply is true."
	}
	return t.Description
}

// workspaceParam is the property every tool has, and no command has as a
// flag: the directory relative paths resolve against.
const workspaceParam = "workspace"

const workspaceDesc = "Directory that relative paths resolve against (default: the server's working directory)."

// inputSchema is the tool's JSON input schema, derived from its parameters.
func (t toolSpec) inputSchema() map[string]any {
	props := map[string]any{
		workspaceParam: map[string]any{"type": "string", "description": workspaceDesc},
	}
	var required []string
	for _, p := range t.Params {
		if !p.exposed() {
			continue
		}
		props[p.name()] = p.schema()
		if p.Required {
			required = append(required, p.name())
		}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// argv turns a call's arguments into the argument vector the command
// parses, exactly as if it had been typed.
//
// The output format is forced to JSON: a tool's answer is the envelope,
// and without it a mutation's preview would be a bare diff. Flags come
// first and positionals after a `--`, so that no value — a new name that
// starts with a dash, say — can be read as a flag. A relative path is made
// absolute against the workspace here, before the command sees it, because
// commands resolve against the process's working directory and there is
// exactly one of those per server.
func (t toolSpec) argv(raw json.RawMessage, cwd string) ([]string, error) {
	in, err := decodeArguments(raw)
	if err != nil {
		return nil, err
	}
	known := map[string]bool{workspaceParam: true}
	for _, p := range t.Params {
		if p.exposed() {
			known[p.name()] = true
		}
	}
	for key := range in {
		if !known[key] {
			return nil, render.Errorf(render.CodeUsage, "%s: unknown parameter %q (accepted: %s)",
				t.Name, key, strings.Join(t.acceptedNames(), ", "))
		}
	}

	workspace, err := resolveWorkspace(in, cwd)
	if err != nil {
		return nil, err
	}

	argv := slices.Clone(t.Prefix)
	argv = append(argv, "--format=json")
	var positionals []string
	for _, p := range t.Params {
		if !p.exposed() {
			continue
		}
		values, err := p.values(t.Name, in[p.name()], workspace)
		if err != nil {
			return nil, err
		}
		if len(values) == 0 {
			if p.Required {
				return nil, render.Errorf(render.CodeUsage, "%s: missing required parameter %q", t.Name, p.name())
			}
			continue
		}
		if p.Flag == "" {
			positionals = append(positionals, values...)
			continue
		}
		if p.Type == typeBool {
			// Only true is sent; every boolean flag defaults to false.
			if values[0] == "true" {
				argv = append(argv, "--"+p.Flag)
			}
			continue
		}
		if p.Type == typeStrings {
			// A repeatable flag (`search_text --glob a --glob b`): one per value.
			for _, v := range values {
				argv = append(argv, "--"+p.Flag+"="+v)
			}
			continue
		}
		argv = append(argv, "--"+p.Flag+"="+values[0])
	}
	if len(positionals) > 0 {
		argv = append(argv, "--")
		argv = append(argv, positionals...)
	}
	return argv, nil
}

// acceptedNames lists the property names a call may use.
func (t toolSpec) acceptedNames() []string {
	names := []string{workspaceParam}
	for _, p := range t.Params {
		if p.exposed() {
			names = append(names, p.name())
		}
	}
	slices.Sort(names)
	return names
}

// decodeArguments reads a call's arguments as a JSON object, keeping
// numbers exact so that an integer is not laundered through a float.
func decodeArguments(raw json.RawMessage) (map[string]any, error) {
	in := map[string]any{}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) == 0 || string(trimmed) == "null" {
		return in, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&in); err != nil {
		return nil, render.Errorf(render.CodeUsage, "the arguments must be a JSON object: %v", err)
	}
	return in, nil
}

// resolveWorkspace is the directory the call's relative paths resolve
// against: its `workspace` parameter, made absolute against the server's
// working directory, or that working directory itself.
func resolveWorkspace(in map[string]any, cwd string) (string, error) {
	value, ok := in[workspaceParam]
	if !ok || value == nil {
		return cwd, nil
	}
	dir, ok := value.(string)
	if !ok {
		return "", render.Errorf(render.CodeUsage, "parameter %q must be a string", workspaceParam)
	}
	if dir == "" {
		return cwd, nil
	}
	dir = resolveAgainst(cwd, dir)
	info, err := os.Stat(dir)
	switch {
	case err != nil:
		return "", render.Errorf(render.CodeNoSuchFile, "%s: no such directory", dir)
	case !info.IsDir():
		return "", render.Errorf(render.CodeUsage, "%s is a file, not a directory", dir)
	}
	return dir, nil
}

// resolveAgainst makes path absolute against dir. A location's ":line:col"
// suffix rides along, being part of the last path element.
func resolveAgainst(dir, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(dir, path)
}

// values reads the parameter's value out of a call as the strings it puts
// on the command line: none when it is unset (or empty), one for a scalar,
// several for a list.
func (p param) values(tool string, v any, workspace string) ([]string, error) {
	wrong := func(want string) error {
		return render.Errorf(render.CodeUsage, "%s: parameter %q must be %s", tool, p.name(), want)
	}
	var out []string
	switch p.Type {
	case typeString, typeDuration:
		if v == nil {
			break
		}
		s, ok := v.(string)
		if !ok {
			return nil, wrong("a string")
		}
		if s != "" {
			out = []string{s}
		}
	case typeInt:
		if v == nil {
			break
		}
		n, ok := v.(json.Number)
		if !ok {
			return nil, wrong("an integer")
		}
		i, err := strconv.ParseInt(n.String(), 10, 64)
		if err != nil {
			return nil, wrong("an integer")
		}
		out = []string{strconv.FormatInt(i, 10)}
	case typeBool:
		if v == nil {
			break
		}
		b, ok := v.(bool)
		if !ok {
			return nil, wrong("a boolean")
		}
		out = []string{strconv.FormatBool(b)}
	case typeStrings:
		if v == nil {
			break
		}
		list, ok := v.([]any)
		if !ok {
			return nil, wrong("a list of strings")
		}
		for _, item := range list {
			s, ok := item.(string)
			if !ok {
				return nil, wrong("a list of strings")
			}
			if s != "" {
				out = append(out, s)
			}
		}
	}
	if len(out) == 0 && p.WorkspaceDefault {
		out = []string{workspace}
	}
	if p.Path || p.Location {
		for i, s := range out {
			if p.IDOrLocation && isSymbolID(s) {
				continue
			}
			out[i] = resolveAgainst(workspace, s)
		}
	}
	if len(p.Enum) > 0 && len(out) == 1 && !slices.Contains(p.Enum, out[0]) {
		return nil, render.Errorf(render.CodeUsage, "%s: parameter %q must be one of %s (got %q)",
			tool, p.name(), strings.Join(p.Enum, ", "), out[0])
	}
	if p.Type == typeInt && len(out) == 1 {
		n, _ := strconv.Atoi(out[0])
		if (p.Min != nil && n < *p.Min) || (p.Max != nil && n > *p.Max) {
			return nil, render.Errorf(render.CodeUsage, "%s: parameter %q is out of range (got %d)", tool, p.name(), n)
		}
	}
	return out, nil
}

// --- the parameters commands share ---

// intp is a pointer to n, for a parameter's bounds.
func intp(n int) *int { return &n }

// commonParams are the flags parseFlagsRange registers on every command,
// declared once. expose names the ones a tool offers; the rest are
// declared CLI-only, because a command that has the flag and does not
// offer it must say why.
func commonParams(expose ...string) []param {
	all := []param{
		{Flag: "context", Type: typeInt, Min: intp(0),
			Desc: "Lines of source to include around each result (default 0: the matched line only)."},
		{Flag: "limit", Type: typeInt, Min: intp(0),
			Desc: "Maximum results, 0 for no limit. Truncation is always reported."},
		{Flag: "timeout", Type: typeDuration,
			Desc: "How long to wait for the workspace to become ready, as a Go duration (default 30s)."},
		{Flag: "server", Type: typeString,
			Desc: "Name of the server to use when several claim the file."},
		{Flag: "absolute", Type: typeBool,
			Desc: "Report absolute file paths instead of paths relative to the workspace root (data.root)."},
		{Flag: "verbose-locations", Type: typeBool,
			Desc: "Restore uri, the LSP range and byte offsets on every location result (default: path plus 1-based start/end line/column plus text)."},
		{Flag: "report", Type: typeBool,
			Desc: "Restore the full index revalidation report (files/fresh/skipped/uncovered/scan_ns/elapsed_ns/listed) instead of a one-line summary and a stale flag."},
		{Flag: "format", CLIOnly: "tools always return the JSON envelope"},
		{Flag: "indent", CLIOnly: "tools always return the compact JSON envelope"},
		{Flag: "settle", CLIOnly: "a tuning knob of the readiness rule (PLAN §5.2); the default is what the rule was designed around"},
		{Flag: "no-daemon", CLIOnly: "chosen per server: run `lightspeed --no-daemon mcp`"},
		{Flag: "offline", CLIOnly: "chosen per server: run `lightspeed --offline mcp`"},
	}
	for i, p := range all {
		if p.exposed() && !slices.Contains(expose, p.Flag) {
			all[i].CLIOnly = "has no effect on this command"
		}
	}
	return all
}

// queryParams are the common flags of a command that answers with a result
// set.
func queryParams() []param {
	return commonParams("context", "limit", "timeout", "server", "absolute", "verbose-locations")
}

// treePathDesc describes the `path` of tree and repo_outline.
const treePathDesc = "Any directory inside the workspace; dir must be inside that workspace too, symlinks and .. included (default: workspace)."

// managementParams are the common flags of a command that reports on the
// server setup: of them only the timeout means anything.
func managementParams() []param {
	return commonParams("timeout")
}

// locationParams are how a command that acts on a position names it: a
// location, or a symbol to be resolved into one — the same two ways as the
// command line (docs/DECISIONS.md D11).
func locationParams(locDesc string) []param {
	return []param{
		{Name: "location", Type: typeString, Path: true, Location: true, Desc: locDesc},
		{Flag: "symbol", Type: typeString,
			Desc: "Dotted symbol path such as pkg.Type.Method, resolved with workspace/symbol. Alternative to location; an ambiguous name is an error listing every candidate."},
		{Flag: "id", Type: typeString,
			Desc: "Symbol id (path::Container.Name#kind) from outline, symbols or workspace_symbol. Alternative to location and symbol; a stale id is an error naming the nearest candidates."},
		{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true,
			Desc: "Directory whose workspace symbol searches and id paths are relative to (default: workspace)."},
	}
}

// pointDesc describes a `location` that names one position.
const pointDesc = "file:line:col (1-based line, byte column; file:#offset also works). Give this, symbol or id."

// mutationParams are the flags of a command that can write.
func mutationParams() []param {
	return []param{
		{Flag: "apply", Type: typeBool,
			Desc: "Write the edits to disk. Default false previews them and writes nothing."},
		{Flag: "allow-dirty", Type: typeBool,
			Desc: "Allow apply although the git worktree has uncommitted changes (it is refused otherwise)."},
	}
}

// withParams joins parameter lists.
func withParams(lists ...[]param) []param {
	return slices.Concat(lists...)
}

// sourceParams are the flags of the commands that return a slice of source:
// the workspace the ids are relative to, and the caps.
func sourceParams() []param {
	return []param{
		{Flag: "path", Type: typeString, Path: true, WorkspaceDefault: true,
			Desc: "Any directory inside the workspace the ids and the confinement are relative to (default: workspace)."},
		{Flag: "max-lines", Type: typeInt, Min: intp(1), Desc: "Cap the source at this many lines; a cut is reported as truncated:true."},
		{Flag: "max-bytes", Type: typeInt, Min: intp(1), Desc: "Cap the source at this many bytes, cut on a character boundary; reported as truncated:true."},
	}
}

// indexRootParam is the workspace anchor of the whole-repo query commands. It is
// `root` and not `path` because `search_symbols` and `repo_map` use `path` for
// the scope, as `search_text` does.
func indexRootParam() param {
	return param{Flag: "root", Type: typeString, Path: true, WorkspaceDefault: true,
		Desc: "Any directory inside the workspace to query (default: workspace)."}
}

// indexRootParams is indexRootParam as a list, for withParams.
func indexRootParams() []param { return []param{indexRootParam()} }
