package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capability is one question a coding agent asks about a codebase and the
// lightspeed command that answers it. It is the single source
// docs/AGENT-SETUP.md is checked against: a row names a lightspeed command
// and the MCP tool that provides the capability.
type capability struct {
	// Name is the capability, as an agent would describe it.
	Name string
	// Command and Tool are the lightspeed command in the table and the MCP tool
	// that provides the capability.
	Command, Tool string
}

var agentCapabilities = []capability{
	{Name: "workspace status", Command: "index", Tool: "index_status"},
	{Name: "warm the index", Command: "index", Tool: "index_build"},
	{Name: "task context", Command: "task_context", Tool: "task_context"},
	{Name: "symbol search", Command: "search_symbols", Tool: "search_symbols"},
	{Name: "text search", Command: "search_text", Tool: "search_text"},
	{Name: "file outline", Command: "outline", Tool: "outline"},
	{Name: "symbol source", Command: "source", Tool: "source"},
	{Name: "context bundle", Command: "context", Tool: "context"},
	{Name: "file content", Command: "file", Tool: "file"},
	{Name: "repo outline", Command: "repo_outline", Tool: "repo_outline"},
	{Name: "file tree", Command: "tree", Tool: "tree"},
	{Name: "repo map", Command: "repo_map", Tool: "repo_map"},
	{Name: "importers", Command: "find_importers", Tool: "find_importers"},
	{Name: "references", Command: "references", Tool: "references"},
	{Name: "reference check", Command: "check_references", Tool: "check_references"},
	{Name: "dependency graph", Command: "dependency_graph", Tool: "dependency_graph"},
	{Name: "dependency cycles", Command: "dependency_cycles", Tool: "dependency_cycles"},
	{Name: "blast radius", Command: "blast_radius", Tool: "blast_radius"},
	{Name: "changed symbols", Command: "changed_symbols", Tool: "changed_symbols"},
	{Name: "dead code", Command: "dead_code", Tool: "dead_code"},
	{Name: "type hierarchy", Command: "type_hierarchy", Tool: "type_hierarchy"},
	{Name: "call hierarchy", Command: "call_hierarchy", Tool: "call_hierarchy"},
	{Name: "implementations", Command: "implementation", Tool: "implementation"},
	{Name: "related symbols", Command: "related", Tool: "related"},
	{Name: "rename check", Command: "rename_check", Tool: "rename_check"},
	{Name: "delete check", Command: "delete_check", Tool: "delete_check"},
	{Name: "churn", Command: "churn", Tool: "churn"},
	{Name: "hotspots", Command: "hotspots", Tool: "hotspots"},
}

// TestAgentCapabilities fails when a capability of the table has no command
// that exists and is exposed to an MCP client.
func TestAgentCapabilities(t *testing.T) {
	seen := map[string]bool{}
	toolOwner := map[string]string{}
	for _, tool := range mcpTools() {
		toolOwner[tool.spec.Name] = tool.cmd.Name
	}
	for _, row := range agentCapabilities {
		if seen[row.Name] {
			t.Errorf("%s is listed twice", row.Name)
		}
		seen[row.Name] = true
		if row.Command == "" || row.Tool == "" {
			t.Errorf("%s names no command or tool", row.Name)
			continue
		}
		c := lookupCommand(row.Command)
		if c == nil {
			t.Errorf("%s: the command %q is not in the command table", row.Name, row.Command)
			continue
		}
		owner, exposed := toolOwner[row.Tool]
		switch {
		case !exposed:
			t.Errorf("%s: the tool %q is not exposed over MCP", row.Name, row.Tool)
		case owner != row.Command:
			t.Errorf("%s: the tool %q belongs to the command %q, not %q", row.Name, row.Tool, owner, row.Command)
		}
	}
}

// TestAgentSetupDocMatchesTheCapabilityTable fails when docs/AGENT-SETUP.md
// leaves out a command of the capability table or a setup step the doc must
// carry: the doc and the test are one source.
func TestAgentSetupDocMatchesTheCapabilityTable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "AGENT-SETUP.md"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(raw), "\n")
	for _, row := range agentCapabilities {
		var found bool
		for _, line := range lines {
			if !strings.HasPrefix(strings.TrimSpace(line), "|") {
				continue
			}
			if strings.Contains(line, "`"+row.Command+"`") || strings.Contains(line, "`"+row.Command+" ") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docs/AGENT-SETUP.md has no table row naming the command `%s` (%s)", row.Command, row.Name)
		}
	}
	doc := string(raw)
	for _, want := range []string{
		"claude mcp add --scope user lightspeed -- lightspeed mcp", "claude mcp remove lightspeed",
		"lightspeed guide --format claude-md", "lightspeed doctor", "lightspeed install", "Rollback",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("the setup doc lacks %q", want)
		}
	}
}
