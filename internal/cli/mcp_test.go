package cli

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The MCP surface, driven through the go-sdk's own client over its in-memory
// transport, against the same scripted fake language server every other
// command test uses. Nothing here starts a real server or touches the
// network.
//
// The clients are built on `envNoDaemon()`, so the suite mode decides whether
// the tools run in process or through a daemon, and
// `LIGHTSPEED_TEST_VIA_DAEMON=1 go test ./internal/cli` runs these in both.

// mcpSession connects a client to a fresh server whose working directory is
// cwd.
func mcpSession(t *testing.T, cwd string) *mcp.ClientSession {
	t.Helper()
	cs, _ := mcpSessionState(t, cwd)
	return cs
}

// mcpSessionState is mcpSession that also returns the server's call state.
func mcpSessionState(t *testing.T, cwd string) (*mcp.ClientSession, *mcpServer) {
	t.Helper()
	ctx := context.Background()
	server, state := newMCPServerState(&env{stderr: &safeBuffer{}, noDaemon: envNoDaemon()}, cwd)
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	ss, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs, state
}

// callTool calls a tool and returns its result, failing on a protocol
// error: a tool that fails must fail as a result, not as a JSON-RPC error.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	return res
}

// resultText is the text content of a result, which must be exactly one.
func resultText(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("result has %d content items, want 1", len(res.Content))
	}
	text, ok := res.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content is %T, want text", res.Content[0])
	}
	return text.Text
}

// resultExit is the exit code a result reports.
func resultExit(t *testing.T, res *mcp.CallToolResult) int {
	t.Helper()
	exit, ok := res.Meta[exitMeta].(float64)
	if !ok {
		t.Fatalf("result carries no %s in its _meta: %v", exitMeta, res.Meta)
	}
	return int(exit)
}

// wantTools is the MCP surface, spelled out. It is written down a second
// time on purpose: a command added to the table with the wrong entry, or an
// entry that stops being one, fails here rather than passing unnoticed.
var wantTools = []string{
	"blast_radius", "call_hierarchy", "changed_symbols", "check", "check_references", "churn",
	"codeaction", "context", "daemon_status", "dead_code", "definition", "delete_check",
	"dependency_cycles", "dependency_graph", "doctor", "file", "find_importers", "format", "guide",
	"hotspots", "hover", "implementation", "imports", "index_build", "index_clear", "index_status",
	"outline", "references", "related", "rename", "rename_check", "repo_map", "repo_outline",
	"search_symbols", "search_text", "servers", "source", "symbols", "task_context", "tree",
	"type_hierarchy", "version", "workspace_symbol",
}

// wantExcluded is every command that is deliberately not a tool.
var wantExcluded = []string{"batch", "help", "install", "mcp", "raw"}

// TestMCPToolsList: tools/list is the table's surface, with a typed and
// described schema for each, and the read-only hint on exactly the tools that
// cannot write.
func TestMCPToolsList(t *testing.T) {
	cs := mcpSession(t, t.TempDir())
	listed, err := cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		names = append(names, tool.Name)
		byName[tool.Name] = tool
	}
	slices.Sort(names)
	if !slices.Equal(names, wantTools) {
		t.Fatalf("tools = %v\nwant    %v", names, wantTools)
	}

	writers := map[string]bool{"rename": true, "codeaction": true, "format": true}
	if len(byName) != len(wantTools) {
		t.Fatalf("listed %d tools, want %d", len(byName), len(wantTools))
	}
	for name, tool := range byName {
		if tool.Description == "" || len(tool.Description) > 160 {
			t.Errorf("%s: description %q is empty or not terse", name, tool.Description)
		}
		if tool.Annotations == nil {
			t.Errorf("%s: no annotations", name)
			continue
		}
		if tool.Annotations.ReadOnlyHint == writers[name] {
			t.Errorf("%s: readOnlyHint = %v, but the tool %s write", name, tool.Annotations.ReadOnlyHint,
				map[bool]string{true: "can", false: "cannot"}[writers[name]])
		}

		schema := schemaOf(t, tool)
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Errorf("%s: schema is not a closed object: %v", name, schema)
		}
		props, _ := schema["properties"].(map[string]any)
		if _, ok := props[workspaceParam]; !ok {
			t.Errorf("%s: no %s property", name, workspaceParam)
		}
		for prop, def := range props {
			d, _ := def.(map[string]any)
			if d["type"] == nil || d["description"] == nil || len(d["description"].(string)) > 200 {
				t.Errorf("%s.%s: property %v is untyped, undescribed or not terse", name, prop, def)
			}
			if prop == "format" || prop == "indent" {
				t.Errorf("%s offers %s, but a tool always returns the JSON envelope", name, prop)
			}
		}
	}

	// A location command takes a location or a symbol, plus the common
	// parameters, typed.
	props := schemaOf(t, byName["references"])["properties"].(map[string]any)
	for prop, wantType := range map[string]string{
		"location": "string", "symbol": "string", "path": "string", "declaration": "boolean",
		"limit": "integer", "context": "integer", "timeout": "string", "server": "string", "workspace": "string",
	} {
		got, _ := props[prop].(map[string]any)
		if got["type"] != wantType {
			t.Errorf("references.%s: type = %v, want %s", prop, got["type"], wantType)
		}
	}
	if _, ok := schemaOf(t, byName["references"])["required"]; ok {
		t.Errorf("references requires something, but location and symbol are alternatives")
	}
	if got := schemaOf(t, byName["rename"])["required"]; !reflect.DeepEqual(got, []any{"new_name"}) {
		t.Errorf("rename.required = %v, want [new_name]", got)
	}
	if got := schemaOf(t, byName["format"])["required"]; !reflect.DeepEqual(got, []any{"paths"}) {
		t.Errorf("format.required = %v, want [paths]", got)
	}
	depth := schemaOf(t, byName["call_hierarchy"])["properties"].(map[string]any)["depth"].(map[string]any)
	if depth["minimum"] != float64(1) || depth["maximum"] != float64(maxCallDepth) {
		t.Errorf("call_hierarchy.depth bounds = %v..%v, want 1..%d", depth["minimum"], depth["maximum"], maxCallDepth)
	}
	direction := schemaOf(t, byName["call_hierarchy"])["properties"].(map[string]any)["direction"].(map[string]any)
	if !reflect.DeepEqual(direction["enum"], []any{"incoming", "outgoing", "both"}) {
		t.Errorf("call_hierarchy.direction enum = %v", direction["enum"])
	}

	if init := cs.InitializeResult(); init == nil || !strings.Contains(init.Instructions, "not_ready") ||
		!strings.Contains(init.Instructions, "apply") {
		t.Errorf("the server's instructions do not cover not_ready and apply: %+v", init)
	}
}

// schemaOf reads a tool's input schema as a generic object.
func schemaOf(t *testing.T, tool *mcp.Tool) map[string]any {
	t.Helper()
	raw, err := json.Marshal(tool.InputSchema)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

// TestMCPEveryCommandIsExposedOrExcluded fails for a command-table entry that
// is neither a tool nor explicitly not one — the guard that makes a new
// command's place on the agent surface a decision.
func TestMCPEveryCommandIsExposedOrExcluded(t *testing.T) {
	var exposed, excluded, tools []string
	for _, c := range commands {
		switch {
		case len(c.MCP) > 0 && c.NoMCP != "":
			t.Errorf("command %q is both exposed and excluded (%q)", c.Name, c.NoMCP)
		case len(c.MCP) == 0 && c.NoMCP == "":
			t.Errorf("command %q is neither an MCP tool nor excluded: give it MCP tools, or NoMCP saying why not", c.Name)
		case len(c.MCP) > 0:
			exposed = append(exposed, c.Name)
		default:
			excluded = append(excluded, c.Name)
		}
		for _, spec := range c.MCP {
			tools = append(tools, spec.Name)
			// One tool per subcommand, named as it is typed; `daemon` and
			// `index` are the commands whose tools are subcommands of them.
			if spec.Name != c.Name && !(c.Name == "daemon" && spec.Name == "daemon_status") &&
				!(c.Name == "index" && strings.HasPrefix(spec.Name, "index_")) {
				t.Errorf("command %q provides a tool named %q", c.Name, spec.Name)
			}
		}
	}
	slices.Sort(tools)
	slices.Sort(excluded)
	if !slices.Equal(tools, wantTools) {
		t.Errorf("the table exposes %v, want %v; update wantTools if this is deliberate", tools, wantTools)
	}
	if !slices.Equal(excluded, wantExcluded) {
		t.Errorf("the table excludes %v, want %v; update wantExcluded if this is deliberate", excluded, wantExcluded)
	}
	for _, name := range wantExcluded {
		if slices.Contains(exposed, name) {
			t.Errorf("%q must not be exposed", name)
		}
	}
	// daemon's other subcommands stay off the surface.
	daemon := lookupCommand("daemon")
	if len(daemon.MCP) != 1 || !slices.Equal(daemon.MCP[0].Prefix, []string{"status"}) {
		t.Errorf("daemon exposes %+v, want only `daemon status`", daemon.MCP)
	}
}

var flagUsageLine = regexp.MustCompile(`^  -(\S+)(?: ([^\t]*))?(?:\t.*)?$`)

// cliFlags asks a command for its flags the way a user would, with -h, and
// returns each flag's name and the type word flag prints for it.
func cliFlags(t *testing.T, args ...string) map[string]string {
	t.Helper()
	_, _, stderr := runMain(append(slices.Clone(args), "-h")...)
	flags := map[string]string{}
	for _, line := range strings.Split(stderr, "\n") {
		if m := flagUsageLine.FindStringSubmatch(line); m != nil {
			flags[m[1]] = m[2]
		}
	}
	if len(flags) == 0 {
		t.Fatalf("%v -h printed no flags:\n%s", args, stderr)
	}
	return flags
}

// TestParamSpecMatchesCLIFlags is the guard against the two surfaces
// drifting: every flag a command registers is in its parameter spec — offered,
// or declared CLI-only with a reason — and every flag the spec names exists,
// with the type the spec claims.
func TestParamSpecMatchesCLIFlags(t *testing.T) {
	typeWord := map[paramType]string{typeString: "string", typeInt: "int", typeBool: "", typeDuration: "duration", typeStrings: "value"}
	for _, tool := range mcpTools() {
		t.Run(tool.spec.Name, func(t *testing.T) {
			cli := cliFlags(t, append([]string{tool.cmd.Name}, tool.spec.Prefix...)...)
			declared := map[string]bool{}
			for _, p := range tool.spec.Params {
				if p.Flag == "" {
					if p.CLIOnly != "" {
						t.Errorf("positional %q cannot be CLI-only", p.name())
					}
					if p.Name == "" {
						t.Errorf("a positional must name itself")
					}
					continue
				}
				names := append([]string{p.Flag}, p.Aliases...)
				for _, name := range names {
					declared[name] = true
					if _, ok := cli[name]; !ok {
						t.Errorf("%s names --%s, which the command does not have", describeParam(p), name)
					}
				}
				if p.exposed() {
					if want, got := typeWord[p.Type], cli[p.Flag]; want != got {
						t.Errorf("%s is declared %q but the command's flag is %q", describeParam(p), want, got)
					}
					if p.Desc == "" {
						t.Errorf("%s has no description", describeParam(p))
					}
				}
			}
			for name := range cli {
				if !declared[name] {
					t.Errorf("the command has --%s, which its parameter spec does not declare (offer it, or declare it CLIOnly with a reason)", name)
				}
			}
			checkPositionals(t, tool)
		})
	}
}

// arityProbe is what a command asked parseFlagsRange to enforce.
type arityProbe struct{ min, max int }

// realArity runs a command far enough to learn how many positional arguments
// it really accepts: the seam in parseFlagsRange reports the count and the
// probe stops the command there, before it reaches for a server.
func realArity(t *testing.T, tool mcpTool) (min, max int) {
	t.Helper()
	var got *arityProbe
	func() {
		defer func() {
			if r := recover(); r != nil {
				probe, ok := r.(arityProbe)
				if !ok {
					panic(r)
				}
				got = &probe
			}
		}()
		e := &env{stdout: &safeBuffer{}, stderr: &safeBuffer{}, noDaemon: true,
			onArity: func(min, max int) { panic(arityProbe{min, max}) }}
		tool.cmd.Run(e, tool.cmd, append(slices.Clone(tool.spec.Prefix), "--format=json"))
	}()
	if got == nil {
		t.Fatalf("%s never asked parseFlagsRange for a positional count, so its positionals cannot be checked", tool.cmd.Name)
	}
	return got.min, got.max
}

// usagePositionals reads the positional arguments out of a command's usage
// summary (`<loc> <newname>`, `[path...]`): each one's alternatives, whether
// it is optional and whether it is variadic. Flags (`[--path DIR]`) and a
// subcommand word (`status|stop|logs`) are not positionals.
func usagePositionals(args string) (out []usagePositional) {
	skipping := false
	for _, f := range strings.Fields(args) {
		switch {
		case skipping:
			skipping = !strings.HasSuffix(f, "]")
			continue
		case strings.HasPrefix(f, "[--"):
			skipping = !strings.HasSuffix(f, "]")
			continue
		case !strings.HasPrefix(f, "<") && !strings.HasPrefix(f, "["):
			continue
		}
		u := usagePositional{optional: strings.HasPrefix(f, "[")}
		f = strings.Trim(f, "<>[]")
		if strings.HasSuffix(f, "...") {
			u.variadic = true
			f = strings.TrimSuffix(f, "...")
		}
		u.names = strings.Split(f, "|")
		out = append(out, u)
	}
	return out
}

type usagePositional struct {
	names              []string
	optional, variadic bool
}

// checkPositionals compares a tool's positional parameters with what the
// command does: how many it enforces (the real parse), and the names, order,
// optionality and variadic-ness its usage line shows.
func checkPositionals(t *testing.T, tool mcpTool) {
	t.Helper()
	var spec []param
	for _, p := range tool.spec.Params {
		if p.Flag == "" {
			spec = append(spec, p)
		}
	}
	for i, p := range spec {
		if p.Type == typeStrings && i != len(spec)-1 {
			t.Errorf("positional %q is a list but not the last one: nothing could follow it", p.name())
		}
	}

	// What the command enforces.
	min, max := realArity(t, tool)
	wantMin, wantMax := 0, len(spec)
	for _, p := range spec {
		if p.Required {
			wantMin++
		}
	}
	if len(spec) > 0 && spec[len(spec)-1].Type == typeStrings {
		wantMax = -1
	}
	if min != wantMin || max != wantMax {
		t.Errorf("the command enforces %s positional argument(s), but its spec declares %s (%d required of %d, a list last: %v)",
			countRange(min, max), countRange(wantMin, wantMax), wantMin, len(spec), wantMax == -1)
	}

	// What the command says.
	usage := usagePositionals(tool.cmd.Args)
	if len(usage) != len(spec) {
		t.Errorf("the usage %q shows %d positional(s), the spec declares %d", tool.cmd.Args, len(usage), len(spec))
		return
	}
	normalized := func(s string) string {
		return strings.ToLower(strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' {
				return r
			}
			return -1
		}, s))
	}
	for i, u := range usage {
		p := spec[i]
		named := slices.ContainsFunc(u.names, func(n string) bool {
			return n != "" && strings.HasPrefix(normalized(p.name()), normalized(n))
		})
		if !named {
			t.Errorf("positional %d is %q in the usage %q but %q in the spec", i+1, strings.Join(u.names, "|"), tool.cmd.Args, p.name())
		}
		if u.variadic != (p.Type == typeStrings) {
			t.Errorf("positional %q: the usage says variadic=%v, the spec says type %d", p.name(), u.variadic, p.Type)
		}
		// A location is optional in the spec although the usage writes it
		// as required, because `--symbol` names the position instead.
		if !p.Location && u.optional == p.Required {
			t.Errorf("positional %q: the usage says optional=%v, the spec says Required=%v", p.name(), u.optional, p.Required)
		}
	}
}

// TestMCPArgv: how a call's arguments become a command line.
func TestMCPArgv(t *testing.T) {
	dir := t.TempDir()
	cwd := t.TempDir()
	tool := func(name string) toolSpec {
		for _, tl := range mcpTools() {
			if tl.spec.Name == name {
				return tl.spec
			}
		}
		t.Fatalf("no tool %q", name)
		return toolSpec{}
	}
	for _, tc := range []struct {
		name string
		tool string
		args string
		want []string
	}{
		{"location relative to the workspace", "references",
			`{"workspace":` + q(dir) + `,"location":"pkg/a.go:3:5","limit":5,"declaration":true}`,
			[]string{"--format=json", "--path=" + dir, "--limit=5", "--declaration", "--", filepath.Join(dir, "pkg/a.go:3:5")}},
		{"relative workspace resolves against the working directory", "definition",
			`{"workspace":"sub","location":"a.go:1:1"}`, nil},
		{"symbol and its search directory", "hover",
			`{"symbol":"pkg.T.M","path":"internal"}`,
			[]string{"--format=json", "--symbol=pkg.T.M", "--path=" + filepath.Join(cwd, "internal")}},
		{"a false boolean is not sent, an empty string is unset", "rename",
			`{"symbol":"a.B","new_name":"-x","apply":false,"server":""}`,
			[]string{"--format=json", "--symbol=a.B", "--path=" + cwd, "--", "-x"}},
		{"a list of paths", "format",
			`{"paths":["a.go","/abs/b.go"],"apply":true,"tab_size":2}`,
			[]string{"--format=json", "--tab-size=2", "--apply", "--", filepath.Join(cwd, "a.go"), "/abs/b.go"}},
		{"check defaults to the workspace", "check", `{}`,
			[]string{"--format=json", "--", cwd}},
		{"the daemon tool is a subcommand", "daemon_status", `{}`,
			[]string{"status", "--format=json", "--path=" + cwd}},
		{"no arguments at all", "servers", ``,
			[]string{"--format=json", "--path=" + cwd}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tool(tc.tool).argv(json.RawMessage(tc.args), cwd)
			if tc.want == nil {
				// The workspace names a directory that does not exist.
				if err == nil {
					t.Fatalf("argv = %v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("argv =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}

	for _, tc := range []struct {
		name string
		tool string
		args string
		want string
	}{
		{"unknown parameter", "references", `{"file":"a.go"}`, `unknown parameter "file"`},
		{"missing required", "rename", `{"location":"a.go:1:1"}`, `missing required parameter "new_name"`},
		{"wrong type", "references", `{"limit":"5"}`, `"limit" must be an integer`},
		{"fractional integer", "references", `{"limit":1.5}`, `"limit" must be an integer`},
		{"enum", "call_hierarchy", `{"direction":"up"}`, `must be one of incoming, outgoing, both`},
		{"range", "call_hierarchy", `{"depth":9}`, `out of range`},
		{"list element", "format", `{"paths":["a.go",3]}`, `must be a list of strings`},
		{"not an object", "references", `[1]`, `must be a JSON object`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := tool(tc.tool).argv(json.RawMessage(tc.args), cwd)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

func q(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestMCPReferencesMatchesCLI: a tool call is the command, not a
// reimplementation of it — the envelope is the one the CLI prints for the same
// question, byte for byte, whether the location is absolute or relative to a
// workspace, and it arrives as text and as structured content.
func TestMCPReferencesMatchesCLI(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{
		methodReferences: []any{loc(file, 5, 13, 15), loc(file, 2, 4, 6), loc(file, 5, 8, 10)},
	}}.apply(t)

	code, want, stderr := runMain("references", file+":3:5")
	if code != ExitOK {
		t.Fatalf("CLI exit code = %d; stderr: %s", code, stderr)
	}

	// The server's own working directory is elsewhere: the workspace
	// parameter is what the relative path resolves against.
	cs := mcpSession(t, t.TempDir())
	res := callTool(t, cs, "references", map[string]any{"workspace": dir, "location": "cjk.go:3:5"})
	if res.IsError {
		t.Fatalf("isError: %s", resultText(t, res))
	}
	if got := resultText(t, res); got != strings.TrimSpace(want) {
		t.Errorf("tool text differs from the CLI's envelope\n got: %s\nwant: %s", got, want)
	}
	if exit := resultExit(t, res); exit != ExitOK {
		t.Errorf("exit = %d, want %d", exit, ExitOK)
	}
	structured, ok := res.StructuredContent.(map[string]any)
	if !ok || structured["ok"] != true || structured["version"] != float64(1) {
		t.Fatalf("structuredContent = %v, want the envelope", res.StructuredContent)
	}
	payload := decodeResults(t, resultText(t, res))
	if payload.Kind != "references" || payload.Count != 3 || payload.Results[0].Start.Column != 5 {
		t.Errorf("payload = %+v", payload)
	}

	// The server's working directory is the default workspace.
	cs = mcpSession(t, dir)
	if got := resultText(t, callTool(t, cs, "references", map[string]any{"location": "cjk.go:3:5"})); got != strings.TrimSpace(want) {
		t.Errorf("default workspace: tool text differs from the CLI's envelope\n got: %s\nwant: %s", got, want)
	}

	// Token discipline reaches through: limit truncates, and says so.
	limited := decodeResults(t, resultText(t, callTool(t, cs, "references",
		map[string]any{"location": "cjk.go:3:5", "limit": 1})))
	if limited.Count != 1 || limited.Total != 3 || !limited.Truncated {
		t.Errorf("limit 1: count=%d total=%d truncated=%v, want 1, 3, true", limited.Count, limited.Total, limited.Truncated)
	}
}

// TestMCPErrorEnvelopeSetsIsError: ok:false is a tool error carrying the
// same envelope the CLI prints; an authoritative empty answer (ok:true, exit
// 1) is not.
func TestMCPErrorEnvelopeSetsIsError(t *testing.T) {
	dir, _ := cjkFixture(t)
	scenario{results: map[string]any{methodReferences: []any{}}}.apply(t)
	cs := mcpSession(t, dir)

	missing := filepath.Join(dir, "gone.go") + ":1:1"
	_, want, _ := runMain("references", missing)
	res := callTool(t, cs, "references", map[string]any{"location": "gone.go:1:1"})
	if !res.IsError {
		t.Fatalf("a missing file is not an error result: %s", resultText(t, res))
	}
	if got := resultText(t, res); got != strings.TrimSpace(want) {
		t.Errorf("error envelope differs from the CLI's\n got: %s\nwant: %s", got, want)
	}
	env := decodeEnvelope(t, resultText(t, res))
	if env.OK || env.Error == nil || env.Error.Code != "no_such_file" {
		t.Errorf("envelope = %+v, want ok:false code no_such_file", env)
	}
	if exit := resultExit(t, res); exit != ExitUsage {
		t.Errorf("exit = %d, want %d", exit, ExitUsage)
	}

	// Bad arguments are the same kind of result, not a protocol error.
	for name, args := range map[string]map[string]any{
		"unknown parameter": {"location": "cjk.go:1:1", "bogus": 1},
		"wrong type":        {"location": "cjk.go:1:1", "limit": "many"},
		"no position":       {},
	} {
		res := callTool(t, cs, "references", args)
		env := decodeEnvelope(t, resultText(t, res))
		if !res.IsError || env.OK || env.Error == nil || env.Error.Code != "usage" {
			t.Errorf("%s: isError=%v envelope=%+v, want an ok:false usage envelope", name, res.IsError, env)
		}
	}
	if res := callTool(t, cs, "references", map[string]any{"workspace": filepath.Join(dir, "nope")}); !res.IsError {
		t.Errorf("a workspace that does not exist is not an error result")
	}

	// Nothing found, authoritatively: an answer, so not isError.
	res = callTool(t, cs, "references", map[string]any{"location": "cjk.go:3:5"})
	if res.IsError {
		t.Errorf("an empty answer is isError: %s", resultText(t, res))
	}
	if exit := resultExit(t, res); exit != ExitProblems {
		t.Errorf("exit = %d, want %d (the CLI's exit for an empty answer)", exit, ExitProblems)
	}
}

// TestMCPNotReady: a server that is still indexing is an error result with
// the not-ready code, never an empty list.
func TestMCPNotReady(t *testing.T) {
	dir, _ := cjkFixture(t)
	scenario{indexing: true, results: map[string]any{methodReferences: []any{}}}.apply(t)
	cs := mcpSession(t, dir)

	res := callTool(t, cs, "references", map[string]any{"location": "cjk.go:3:5", "timeout": "1s"})
	if !res.IsError {
		t.Fatalf("an indexing server produced a result: %s", resultText(t, res))
	}
	env := decodeEnvelope(t, resultText(t, res))
	if env.OK || env.Error == nil || env.Error.Code != "not_ready" {
		t.Errorf("envelope = %+v, want ok:false code not_ready", env)
	}
	if exit := resultExit(t, res); exit != ExitNotReady {
		t.Errorf("exit = %d, want %d", exit, ExitNotReady)
	}
}

// TestMCPRenamePreviewsUnlessApplied: the mutating tools do what the CLI
// does — preview by default, write only with apply:true — and a preview
// carries the change set as data.
func TestMCPRenamePreviewsUnlessApplied(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	renameScenario(t, dir, renameToNew(dir)).apply(t)
	cs := mcpSession(t, t.TempDir())

	preview := callTool(t, cs, "rename", map[string]any{"workspace": dir, "location": "a.go:3:6", "new_name": "New"})
	if preview.IsError {
		t.Fatalf("preview failed: %s", resultText(t, preview))
	}
	assertUnchanged(t, dir, before)
	var changes struct {
		Changes []struct {
			Path string `json:"path"`
			Diff string `json:"diff"`
		} `json:"changes"`
		Count int `json:"count"`
	}
	decodeData(t, resultText(t, preview), &changes)
	if changes.Count != 3 || !strings.Contains(changes.Changes[0].Diff, "+func New() int { return 1 }") {
		t.Errorf("the preview does not describe the edit: %+v", changes)
	}

	// apply:false is the same as leaving it out.
	callTool(t, cs, "rename", map[string]any{"workspace": dir, "location": "a.go:3:6", "new_name": "New", "apply": false})
	assertUnchanged(t, dir, before)

	applied := callTool(t, cs, "rename", map[string]any{"workspace": dir, "location": "a.go:3:6", "new_name": "New", "apply": true})
	if applied.IsError {
		t.Fatalf("apply failed: %s", resultText(t, applied))
	}
	after := snapshot(t, dir)
	for name, want := range renamedFiles {
		if after[name] != want {
			t.Errorf("%s =\n%q\nwant\n%q", name, after[name], want)
		}
	}
}

// TestMCPApplyStillRefusesADirtyWorktree: the refusal of D8 applies to the
// tool as it does to the command, and allow_dirty is the way past it.
func TestMCPApplyStillRefusesADirtyWorktree(t *testing.T) {
	dir := tree(t, fixtureFiles)
	initRepo(t, dir)
	write(t, filepath.Join(dir, "go.mod"), "module fixture\n\ngo 1.27\n// edited\n")
	before := snapshot(t, dir)
	renameScenario(t, dir, renameToNew(dir)).apply(t)
	cs := mcpSession(t, dir)

	args := map[string]any{"location": "a.go:3:6", "new_name": "New", "apply": true}
	res := callTool(t, cs, "rename", args)
	env := decodeEnvelope(t, resultText(t, res))
	if !res.IsError || env.Error == nil || env.Error.Code != "dirty_worktree" {
		t.Fatalf("isError=%v envelope=%+v, want ok:false code dirty_worktree", res.IsError, env)
	}
	assertUnchanged(t, dir, before)

	args["allow_dirty"] = true
	if res := callTool(t, cs, "rename", args); res.IsError {
		t.Fatalf("allow_dirty did not let the apply through: %s", resultText(t, res))
	}
	if got := snapshot(t, dir)["a.go"]; got != renamedFiles["a.go"] {
		t.Errorf("a.go = %q after an allowed apply", got)
	}
}

// TestMCPMutationWhileIndexingWritesNothing: not-ready applies to a write
// exactly as it does to a read.
func TestMCPMutationWhileIndexingWritesNothing(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	scenario{
		indexing:     true,
		capabilities: mutationServerCaps(nil),
		results:      map[string]any{methodPrepareRename: nil, methodRename: map[string]any{}},
	}.apply(t)
	cs := mcpSession(t, dir)

	res := callTool(t, cs, "rename", map[string]any{
		"location": "a.go:3:6", "new_name": "New", "apply": true, "allow_dirty": true, "timeout": "1s"})
	env := decodeEnvelope(t, resultText(t, res))
	if !res.IsError || env.Error == nil || env.Error.Code != "not_ready" {
		t.Fatalf("isError=%v envelope=%+v, want ok:false code not_ready", res.IsError, env)
	}
	assertUnchanged(t, dir, before)
}

// TestMCPManagementTools: the tools that talk to no language server work
// against the workspace's own configuration, and `daemon_status` describes a
// daemon that is not running without failing.
func TestMCPManagementTools(t *testing.T) {
	dir := tree(t, fixtureFiles)
	useConfigDir(t)
	cs := mcpSession(t, dir)

	var servers struct {
		Workspace string `json:"workspace"`
		Servers   []struct {
			Name string `json:"name"`
		} `json:"servers"`
	}
	res := callTool(t, cs, "servers", nil)
	if res.IsError {
		t.Fatalf("servers: %s", resultText(t, res))
	}
	decodeData(t, resultText(t, res), &servers)
	if resolved, _ := filepath.EvalSymlinks(dir); servers.Workspace != dir && servers.Workspace != resolved {
		t.Errorf("servers.workspace = %q, want the server's working directory %q", servers.Workspace, dir)
	}
	if len(servers.Servers) == 0 {
		t.Errorf("servers lists no servers")
	}

	res = callTool(t, cs, "doctor", map[string]any{"paths": []any{"a.go"}})
	if res.IsError {
		t.Errorf("doctor: %s", resultText(t, res))
	}
	var doctor struct {
		Worst string `json:"worst"`
	}
	decodeData(t, resultText(t, res), &doctor)
	if doctor.Worst == "" {
		t.Errorf("doctor reported no worst severity: %s", resultText(t, res))
	}

	if envNoDaemon() {
		// With no daemon there is nothing to ask, and the command says so.
		res = callTool(t, cs, "daemon_status", nil)
		if !res.IsError || decodeEnvelope(t, resultText(t, res)).Error.Code != "usage" {
			t.Errorf("daemon_status with --no-daemon: %s", resultText(t, res))
		}
		return
	}
	res = callTool(t, cs, "daemon_status", nil)
	var status struct {
		Running bool `json:"running"`
	}
	decodeData(t, resultText(t, res), &status)
	if res.IsError || status.Running {
		t.Errorf("daemon_status: isError=%v running=%v: %s", res.IsError, status.Running, resultText(t, res))
	}
}

// TestMCPConcurrentCalls: an agent issues calls in parallel, and each is
// answered as it would be alone.
func TestMCPConcurrentCalls(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{methodReferences: []any{loc(file, 2, 4, 6), loc(file, 5, 8, 10)}}}.apply(t)
	cs := mcpSession(t, dir)
	want := resultText(t, callTool(t, cs, "references", map[string]any{"location": "cjk.go:3:5"}))

	var wg sync.WaitGroup
	got := make([]string, 4)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{
				Name: "references", Arguments: map[string]any{"location": "cjk.go:3:5"}})
			if err == nil && len(res.Content) == 1 {
				got[i] = res.Content[0].(*mcp.TextContent).Text
			}
		}()
	}
	wg.Wait()
	for i, text := range got {
		if text != want {
			t.Errorf("call %d answered\n%s\nwant\n%s", i, text, want)
		}
	}
}

// TestMCPCommandOverStdio: `lightspeed mcp` itself serves the protocol on the
// streams it is given, and nothing but the protocol reaches stdout.
func TestMCPCommandOverStdio(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{methodReferences: []any{loc(file, 2, 4, 6)}}}.apply(t)
	t.Chdir(dir)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"references","arguments":{"location":"cjk.go:3:5"}}}`,
	}, "\n") + "\n"
	// stdin stays open until the call has been answered: a client that
	// hangs up mid-call is entitled to no answer.
	var out, errOut safeBuffer
	pr, pw := io.Pipe()
	exited := make(chan int, 1)
	go func() { exited <- MainWithStdin([]string{"mcp"}, pr, &out, &errOut) }()
	if _, err := io.WriteString(pw, input); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(30 * time.Second); !strings.Contains(out.String(), `"id":2`); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no answer to the call; stdout: %s\nstderr: %s", out.String(), errOut.String())
		}
	}
	_ = pw.Close()
	if code := <-exited; code != ExitOK {
		t.Fatalf("exit code = %d; stderr: %s", code, errOut.String())
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("stdout has %d lines, want an initialize and a call answer:\n%s", len(lines), out.String())
	}
	for _, line := range lines {
		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil || msg["jsonrpc"] != "2.0" {
			t.Errorf("stdout carries something that is not a JSON-RPC message: %q", line)
		}
	}
	if !strings.Contains(lines[1], `\"ok\":true`) {
		t.Errorf("the call was not answered with an envelope: %s", lines[1])
	}

	if code, _, _ := runMain("mcp", "extra"); code != ExitUsage {
		t.Errorf("mcp with an argument: exit code = %d, want %d", code, ExitUsage)
	}
}

// TestMCPCancelledCallStops: a call the client cancels with
// notifications/cancelled (the SDK sends one when the call's context is
// cancelled) stops running, instead of running on to its own timeout. The
// server here never finishes indexing, so the call would wait out its
// five-second budget; it has to be gone long before that.
func TestMCPCancelledCallStops(t *testing.T) {
	dir, _ := cjkFixture(t)
	scenario{indexing: true, results: map[string]any{methodReferences: []any{}}}.apply(t)
	cs, state := mcpSessionState(t, dir)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "references",
			Arguments: map[string]any{"location": "cjk.go:3:5", "timeout": "5s"}})
		done <- err
	}()
	eventually(t, "the call to be running", 5*time.Second, func() bool { return state.active.Load() == 1 })
	// It is really waiting, not failing fast for another reason.
	time.Sleep(200 * time.Millisecond)
	if state.active.Load() != 1 {
		t.Fatal("the call ended by itself before it was cancelled")
	}

	started := time.Now()
	cancel()
	eventually(t, "the cancelled call to stop", 2*time.Second, func() bool { return state.active.Load() == 0 })
	if took := time.Since(started); took > 2*time.Second {
		t.Errorf("the call took %s to stop after it was cancelled; its own timeout is 5s", took)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Error("the client got an answer to a call it cancelled")
		}
	case <-time.After(5 * time.Second):
		t.Error("the client's call never returned")
	}
}

// TestMCPHangUpCancelsARunningCall: a client that goes away mid-call takes the
// call with it. The SDK does not do this itself — on end of input it waits for
// the calls in flight — so without the server's own lifetime a call on a
// workspace that never finishes indexing would keep `lightspeed mcp` (and its
// language server) alive for the call's whole timeout.
func TestMCPHangUpCancelsARunningCall(t *testing.T) {
	dir, _ := cjkFixture(t)
	scenario{indexing: true, results: map[string]any{methodReferences: []any{}}}.apply(t)
	t.Chdir(dir)

	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"references","arguments":{"location":"cjk.go:3:5","timeout":"8s"}}}`,
	}, "\n") + "\n"
	var out, errOut safeBuffer
	pr, pw := io.Pipe()
	exited := make(chan int, 1)
	go func() { exited <- MainWithStdin([]string{"mcp"}, pr, &out, &errOut) }()
	if _, err := io.WriteString(pw, input); err != nil {
		t.Fatal(err)
	}
	// The call is under way: the server has answered the handshake and is
	// waiting on a language server that never finishes loading.
	eventually(t, "the handshake to be answered", 30*time.Second, func() bool { return strings.Contains(out.String(), `"id":1`) })
	time.Sleep(500 * time.Millisecond)
	select {
	case code := <-exited:
		t.Fatalf("the server exited (%d) before the client hung up; stderr: %s", code, errOut.String())
	default:
	}

	started := time.Now()
	_ = pw.Close()
	select {
	case code := <-exited:
		if code != ExitOK {
			t.Errorf("exit code = %d after a hang-up, want %d; stderr: %s", code, ExitOK, errOut.String())
		}
		if took := time.Since(started); took > 5*time.Second {
			t.Errorf("the server took %s to leave after the client hung up; the call's own timeout is 8s", took)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the server is still running 10s after the client hung up: the call it was serving was left to its own timeout")
	}
}

// TestMCPCallRunsUnderTheRequestContext: the context a call is handed reaches
// the command's own requests, so a cancelled one ends as a cancelled envelope
// and not as a not-ready or a timeout, promptly.
func TestMCPCallRunsUnderTheRequestContext(t *testing.T) {
	dir, _ := cjkFixture(t)
	scenario{indexing: true, results: map[string]any{methodReferences: []any{}}}.apply(t)
	_, state := mcpSessionState(t, dir)

	var refs mcpTool
	for _, tl := range mcpTools() {
		if tl.spec.Name == "references" {
			refs = tl
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(300*time.Millisecond, cancel)
	started := time.Now()
	res := state.call(ctx, refs, json.RawMessage(`{"location":"cjk.go:3:5","workspace":`+q(dir)+`,"timeout":"8s"}`))
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("a call cancelled after 300ms returned after %s; its timeout is 8s", took)
	}
	env := decodeEnvelope(t, resultText(t, res))
	if !res.IsError || env.Error == nil || env.Error.Code != "cancelled" {
		t.Errorf("isError=%v envelope=%+v, want ok:false code cancelled", res.IsError, env)
	}
	if n := state.active.Load(); n != 0 {
		t.Errorf("%d call(s) still counted as running", n)
	}
}

// describeParam names a parameter for a failure message.
func describeParam(p param) string {
	if p.Flag != "" {
		return "flag --" + p.Flag
	}
	return "positional " + strconvQuote(p.name())
}

func strconvQuote(s string) string { return q(s) }
