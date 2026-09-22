package cli

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
)

// The guide is generated from the command table so that it cannot drift; these
// tests are what makes that true. A command with MCP tools that no group names,
// a group that names a command that does not exist, and a `--flag` that no
// command has, each fail here.

func TestGuideEveryMCPCommandIsInAGroup(t *testing.T) {
	grouped := map[string]bool{}
	for _, g := range guideGroups {
		for _, n := range g.Commands {
			grouped[n] = true
		}
	}
	for _, c := range commands {
		if len(c.MCP) > 0 && !grouped[c.Name] {
			t.Errorf("command %q has MCP tools and is in no guideGroups group: add it to the group that says when to use it", c.Name)
		}
	}
}

func TestGuideGroupsNameRealCommands(t *testing.T) {
	for _, g := range guideGroups {
		if g.Title == "" || g.When == "" || len(g.Commands) == 0 {
			t.Errorf("group %+v is incomplete", g)
		}
		for _, n := range g.Commands {
			if lookupCommand(n) == nil {
				t.Errorf("group %q names %q, which is not in the command table", g.Title, n)
			}
		}
	}
}

var (
	allFlagsOnce sync.Once
	allFlagsSet  map[string]bool
)

// everyCLIFlag is the union of the flags the commands really register, read the
// way a user would with -h (with the subcommand of the commands that have them).
func everyCLIFlag() map[string]bool {
	allFlagsOnce.Do(func() {
		allFlagsSet = map[string]bool{}
		add := func(args ...string) {
			_, _, stderr := runMain(append(slices.Clone(args), "-h")...)
			for _, line := range strings.Split(stderr, "\n") {
				if m := flagUsageLine.FindStringSubmatch(line); m != nil {
					allFlagsSet[m[1]] = true
				}
			}
		}
		for _, c := range commands {
			if c.Name == "mcp" {
				continue
			}
			add(c.Name)
			for _, spec := range c.MCP {
				if len(spec.Prefix) > 0 {
					add(append([]string{c.Name}, spec.Prefix...)...)
				}
			}
		}
	})
	return allFlagsSet
}

var guideFlagRef = regexp.MustCompile("--([a-z][a-z-]*)")

func TestGuideFlagsExist(t *testing.T) {
	real := everyCLIFlag()
	renderings := map[string]string{
		"text":      buildGuide(false, guideText),
		"claude-md": buildGuide(false, guideClaudeMD),
		"compact":   buildGuide(true, guideText),
	}
	for name, text := range renderings {
		for _, m := range guideFlagRef.FindAllStringSubmatch(text, -1) {
			if !real[m[1]] {
				t.Errorf("the %s guide mentions --%s, which no command has", name, m[1])
			}
		}
	}
	for _, claim := range guideFlagClaims {
		for _, cmd := range claim.Commands {
			c := lookupCommand(cmd)
			if c == nil {
				t.Errorf("claim --%s names the command %q, which does not exist", claim.Flag, cmd)
				continue
			}
			have := false
			for _, spec := range c.MCP {
				for _, p := range spec.Params {
					have = have || p.Flag == claim.Flag
				}
			}
			if !have {
				t.Errorf("the guide relies on %s having --%s, and its parameter spec has no such flag", cmd, claim.Flag)
			}
		}
	}
}

func TestGuideIsGeneratedFromTheTable(t *testing.T) {
	text := buildGuide(false, guideText)
	for _, g := range guideGroups {
		for _, n := range g.Commands {
			c := lookupCommand(n)
			if c == nil {
				continue
			}
			if !strings.Contains(text, "`"+n) || !strings.Contains(text, c.Summary) {
				t.Errorf("the guide does not carry %s and its summary from the table", n)
			}
		}
	}
	// A summary edited in the table is what the guide says.
	c := lookupCommand("search_symbols")
	old := c.Summary
	c.Summary = "SUMMARY EDITED IN THE TABLE"
	defer func() { c.Summary = old }()
	if !strings.Contains(buildGuide(false, guideText), "SUMMARY EDITED IN THE TABLE") {
		t.Error("the guide did not follow an edit of the table")
	}
}

func TestGuideRules(t *testing.T) {
	full := buildGuide(false, guideText)
	for _, want := range []string{
		"Never fall back to Read, Grep, Glob or Bash", "only when you are about to Edit",
		"--apply", "--allow-dirty", "stale_id", "truncated:true", "not ready", "path::Container.Name#kind",
	} {
		if !strings.Contains(full, want) {
			t.Errorf("the guide lacks %q", want)
		}
	}
	compact := buildGuide(true, guideText)
	if n := len(compact); n > 2500 {
		t.Errorf("the compact guide is %d characters, want at most 2500: it is in every session's context", n)
	}
	for _, want := range []string{"not_ready", "apply", "Read a file only when you are about to Edit", "stale_id"} {
		if !strings.Contains(compact, want) {
			t.Errorf("the compact guide lacks %q", want)
		}
	}
	if strings.Contains(compact, "--") {
		t.Error("the compact guide spells flags; the MCP client has parameters, not flags")
	}
}

func TestGuideClaudeMDIsARegenerableSection(t *testing.T) {
	code, stdout, stderr := runMain("guide", "--format", "claude-md")
	if code != 0 {
		t.Fatalf("exit %d: %s%s", code, stdout, stderr)
	}
	if !strings.HasPrefix(stdout, guideMarkerPrefix+" v1 build ") {
		t.Errorf("the section does not open with its marker: %.80q", stdout)
	}
	if !strings.HasSuffix(strings.TrimSpace(stdout), guideEnd) {
		t.Errorf("the section does not end with %s", guideEnd)
	}
	if !strings.Contains(stdout, "\n## Code navigation: lightspeed\n") || strings.Contains(stdout, "\n# ") {
		t.Error("a CLAUDE.md section is one heading level below the file's title")
	}
	_, again, _ := runMain("guide", "--format", "claude-md")
	if again != stdout {
		t.Error("two runs of the same binary print different sections")
	}
	if code, _, _ := runMain("guide", "--compact", "--format", "claude-md"); code != ExitUsage {
		t.Errorf("--compact with claude-md exits %d, want %d", code, ExitUsage)
	}
	if code, out, _ := runMain("guide", "--format", "sarif"); code != ExitUsage || !strings.Contains(out, "unsupported_format") {
		t.Errorf("sarif exits %d: %s", code, out)
	}
}

func TestGuideJSONAndDefaultText(t *testing.T) {
	code, stdout, _ := runMain("guide", "--format", "json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stdout)
	}
	env := decodeEnvelope(t, stdout)
	var data guideData
	raw, _ := json.Marshal(env.Data)
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data.Version != guideVersion || data.Build == "" || data.Text != buildGuide(false, guideText) || len(data.Groups) != len(guideGroups) {
		t.Errorf("guide payload = version %d build %q, %d groups", data.Version, data.Build, len(data.Groups))
	}
	// Off a terminal the default is the markdown, not an envelope: it is prose.
	_, text, _ := runMain("guide")
	if !strings.HasPrefix(text, "# Lightspeed agent guide") {
		t.Errorf("default guide = %.60q", text)
	}
	_, compact, _ := runMain("guide", "--compact")
	if strings.TrimSpace(compact) != buildGuide(true, guideText) {
		t.Error("--compact does not print the compact guide")
	}
}

// guideCodeSpan is a `code span` of the guide.
var guideCodeSpan = regexp.MustCompile("`([^`]+)`")

// Over MCP the guide describes the interface the agent has: tools and their
// parameters. It must not name a command that is no tool (`install`, `index`,
// `daemon`), nor spell a parameter as a flag (`--apply`).
func TestMCPGuideNamesToolsAndParametersNotCommandsAndFlags(t *testing.T) {
	cs := mcpSession(t, t.TempDir())
	tools, params := map[string]bool{}, map[string]bool{}
	for _, tool := range mcpTools() {
		tools[tool.spec.Name] = true
		for _, p := range tool.spec.Params {
			if p.exposed() {
				params[p.name()] = true
			}
		}
	}
	init := cs.InitializeResult()
	if init == nil || init.Instructions != buildGuide(true, guideMCP) {
		t.Fatal("the MCP server's instructions are not the compact guide")
	}
	var payloads []guideData
	for _, args := range []map[string]any{nil, {"compact": true}} {
		res := callTool(t, cs, "guide", args)
		if res.IsError {
			t.Fatalf("guide %v: %s", args, resultText(t, res))
		}
		var data guideData
		decodeData(t, resultText(t, res), &data)
		payloads = append(payloads, data)
		if want := buildGuide(args != nil, guideMCP); data.Text != want {
			t.Errorf("guide %v over MCP is not the MCP rendering:\n%.300s", args, data.Text)
		}
	}
	full := payloads[0]
	for name, text := range map[string]string{"full": full.Text, "compact": payloads[1].Text, "instructions": init.Instructions} {
		if strings.Contains(text, "--") {
			t.Errorf("the %s MCP guide spells a flag; the client has parameters:\n%s", name, guideFlagRef.FindString(text))
		}
	}
	for _, want := range []string{"`apply: true`", "`allow_dirty: true`", "`id`", "`limit`", "`budget`", "lightspeed/exit",
		"Never fall back to Read, Grep, Glob or Bash", "`index_status`", "`index_build`", "`index_clear`", "`daemon_status`"} {
		if !strings.Contains(full.Text, want) {
			t.Errorf("the MCP guide lacks %s", want)
		}
	}
	// Every tool is listed, in the text and in the groups; every listed line
	// is a tool, and every parameter it lists is one of that surface.
	for name := range tools {
		if !strings.Contains(full.Text, "- `"+name+"` — ") || !strings.Contains(init.Instructions, name) {
			t.Errorf("the MCP guide or the instructions do not list the tool %s", name)
		}
	}
	for _, line := range strings.Split(full.Text, "\n") {
		if !strings.HasPrefix(line, "- `") || !strings.Contains(line, "` — ") {
			continue
		}
		name := guideCodeSpan.FindStringSubmatch(line)[1]
		if !tools[name] {
			t.Errorf("the MCP guide lists %q, which is not a tool", name)
		}
		if i := strings.LastIndex(line, " ["); i >= 0 && strings.HasSuffix(line, "]") {
			for _, p := range strings.Fields(strings.Trim(line[i+1:], "[]")) {
				if p != "…" && !params[p] {
					t.Errorf("the MCP guide gives %s the parameter %q, which no tool has", name, p)
				}
			}
		}
	}
	for _, g := range full.Groups {
		for _, n := range g.Commands {
			if !tools[n] {
				t.Errorf("group %q names %q, which is not a tool", g.Title, n)
			}
		}
	}
	// `install` is named once, as what it is: a shell command for the user.
	for _, text := range []string{full.Text, init.Instructions} {
		for _, m := range guideCodeSpan.FindAllStringSubmatch(text, -1) {
			if first := strings.Fields(m[1])[0]; first == "install" || first == "index" || first == "daemon" {
				t.Errorf("the MCP guide names `%s` as if it could be called", m[1])
			}
		}
	}
	if !strings.Contains(full.Text, "`lightspeed install <name>` in a shell") {
		t.Error("the MCP guide does not say how a missing server is installed")
	}
	// The command line keeps its own words.
	_, cli, _ := runMain("guide")
	if !strings.Contains(cli, "--apply") || !strings.Contains(cli, "`install <name>`") || strings.Contains(cli, "index_status") {
		t.Error("the CLI guide lost its flags and commands")
	}
}

func init() {
	registerNoLimit("guide", "one document, not a list; --compact is its bound")
	registerNoLimit("version", "one object describing the build")
}
