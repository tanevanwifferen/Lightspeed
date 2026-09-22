package cli

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

// The symbol-retrieval tools over MCP: outline, source, context, tree,
// repo_outline and file, and `id` on the tools that take a location. They are
// the command table, so what is tested here is what the table cannot promise
// by itself: that an id is not mistaken for a path, that a relative location
// resolves against the call's workspace, and that the answer is the CLI's.

// mcpEnvelope decodes a tool result's envelope.
func mcpEnvelope(t *testing.T, textResult string) rawEnvelope {
	t.Helper()
	var env rawEnvelope
	if err := json.Unmarshal([]byte(textResult), &env); err != nil {
		t.Fatalf("result is not an envelope: %v\n%s", err, textResult)
	}
	return env
}

func TestMCPSymbolTools(t *testing.T) {
	dir, _ := symWorkspace(t, map[string]string{"notes.txt": "one\ntwo\nthree\n"})
	symScenario(t, symHierarchical(), map[string]any{methodReferences: json.RawMessage(positionEcho)})

	// The server's own working directory is elsewhere, so that everything
	// below depends on the `workspace` parameter and not on the process.
	cs := mcpSession(t, t.TempDir())
	ws := map[string]any{"workspace": dir}
	with := func(args map[string]any) map[string]any {
		out := map[string]any{"workspace": dir}
		for k, v := range args {
			out[k] = v
		}
		return out
	}

	t.Run("outline is the CLI's answer", func(t *testing.T) {
		_, want, _ := runMain("outline", "sym.go")
		res := callTool(t, cs, "outline", with(map[string]any{"files": []any{"sym.go"}}))
		if res.IsError {
			t.Fatalf("isError: %s", resultText(t, res))
		}
		if got := resultText(t, res); got != strings.TrimSpace(want) {
			t.Errorf("outline differs from the CLI's envelope\n got: %s\nwant: %s", got, want)
		}
		if exit := resultExit(t, res); exit != ExitOK {
			t.Errorf("exit = %d", exit)
		}
		var data outlineData
		json.Unmarshal(mcpEnvelope(t, resultText(t, res)).Data, &data)
		if len(data.Files) != 1 || data.Files[0].Symbols[0].ID != "sym.go::Greeter#struct" {
			t.Errorf("outline data = %+v", data)
		}
	})

	t.Run("source takes an id as it is and a relative location against the workspace", func(t *testing.T) {
		res := callTool(t, cs, "source", with(map[string]any{
			"ids": []any{"sym.go::Greeter.Greet#method", "sym.go:27:21"},
		}))
		if res.IsError {
			t.Fatalf("isError: %s", resultText(t, res))
		}
		var data sourceData
		json.Unmarshal(mcpEnvelope(t, resultText(t, res)).Data, &data)
		if data.Count != 2 || data.Symbols[0].Source != greetSource || data.Symbols[1].ID != "sym.go::名前#variable" {
			t.Errorf("source data = %+v", data)
		}
		capped := callTool(t, cs, "source", with(map[string]any{"ids": []any{"sym.go::Greeter.Greet#method"}, "max_lines": 1}))
		data = sourceData{}
		json.Unmarshal(mcpEnvelope(t, resultText(t, capped)).Data, &data)
		if !data.Symbols[0].Truncated || data.Symbols[0].Source != "// Greet は挨拶を返す 👋" {
			t.Errorf("max_lines: %+v", data.Symbols[0])
		}
	})

	t.Run("a stale id is an error result with candidates", func(t *testing.T) {
		res := callTool(t, cs, "source", with(map[string]any{"ids": []any{"sym.go::Greeter.Greeet#method"}}))
		env := mcpEnvelope(t, resultText(t, res))
		if !res.IsError || env.OK || env.Error.Code != "stale_id" {
			t.Fatalf("isError=%v envelope=%+v, want ok:false stale_id", res.IsError, env)
		}
		if cands := candidateIDs(env.Error.Data); len(cands) == 0 || cands[0] != "sym.go::Greeter.Greet#method" {
			t.Errorf("candidates = %q", cands)
		}
		if exit := resultExit(t, res); exit != ExitProblems {
			t.Errorf("exit = %d, want %d", exit, ExitProblems)
		}
	})

	t.Run("context", func(t *testing.T) {
		res := callTool(t, cs, "context", with(map[string]any{"id": "sym.go::Greeter.Greet#method"}))
		if res.IsError {
			t.Fatalf("isError: %s", resultText(t, res))
		}
		var data contextData
		json.Unmarshal(mcpEnvelope(t, resultText(t, res)).Data, &data)
		if data.Source != greetSource || data.Header == nil || data.Header.EndLine != 6 || data.Hover == "" {
			t.Errorf("context data = %+v", data)
		}
	})

	t.Run("every location tool takes id", func(t *testing.T) {
		res := callTool(t, cs, "references", with(map[string]any{"id": "sym.go::Greeter.Greet#method"}))
		if res.IsError {
			t.Fatalf("isError: %s", resultText(t, res))
		}
		got := decodeResults(t, resultText(t, res)).Results[0]
		if got.Start.Line != 14 {
			t.Errorf("references by id asked about line %d, want 14", got.Start.Line)
		}
		// Giving two ways to name the position is refused, as on the CLI.
		res = callTool(t, cs, "references", with(map[string]any{"id": "sym.go::Greeter#struct", "location": "sym.go:1:1"}))
		if env := mcpEnvelope(t, resultText(t, res)); !res.IsError || env.Error.Code != "usage" {
			t.Errorf("id and location: isError=%v %+v", res.IsError, env.Error)
		}
		// symbols and workspace_symbol give the ids that the others take.
		res = callTool(t, cs, "symbols", with(map[string]any{"file": "sym.go"}))
		if got := decodeResults(t, resultText(t, res)); got.Results[0].ID != "sym.go::Greeter#struct" {
			t.Errorf("symbols by MCP: first id %q", got.Results[0].ID)
		}
	})

	t.Run("file", func(t *testing.T) {
		res := callTool(t, cs, "file", with(map[string]any{"file": "sym.go", "start": 13, "end": 16}))
		if res.IsError {
			t.Fatalf("isError: %s", resultText(t, res))
		}
		var data fileData
		json.Unmarshal(mcpEnvelope(t, resultText(t, res)).Data, &data)
		if data.Source != greetSource || data.EndLine != 16 {
			t.Errorf("file data = %+v", data)
		}
		// The confinement holds over MCP too.
		res = callTool(t, cs, "file", with(map[string]any{"file": "../../etc/passwd"}))
		if env := mcpEnvelope(t, resultText(t, res)); !res.IsError || env.Error.Code != "outside_workspace" {
			t.Errorf("a path out of the workspace: isError=%v %+v", res.IsError, env.Error)
		}
	})

	t.Run("tree and repo_outline", func(t *testing.T) {
		res := callTool(t, cs, "tree", ws)
		if res.IsError {
			t.Fatalf("tree: %s", resultText(t, res))
		}
		var files treeData
		json.Unmarshal(mcpEnvelope(t, resultText(t, res)).Data, &files)
		if got := treePaths(files); !slices.Contains(got, "sym.go") || !slices.Contains(got, "notes.txt") {
			t.Errorf("tree files = %q", got)
		}
		res = callTool(t, cs, "tree", with(map[string]any{"max_files": 1}))
		files = treeData{}
		json.Unmarshal(mcpEnvelope(t, resultText(t, res)).Data, &files)
		if files.Count != 1 || !files.Truncated {
			t.Errorf("max_files 1: %+v", files)
		}
		res = callTool(t, cs, "repo_outline", ws)
		if res.IsError {
			t.Fatalf("repo_outline: %s", resultText(t, res))
		}
		var outline repoOutlineData
		json.Unmarshal(mcpEnvelope(t, resultText(t, res)).Data, &outline)
		if outline.Root == "" || outline.Files == 0 || len(outline.Languages) == 0 {
			t.Errorf("repo_outline = %+v", outline)
		}
	})
}

// TestMCPSymbolToolsAreReadOnly: the new tools cannot write, and say so.
// TestMCPIDOnAFlatServerAsksAboutTheName: `id` over MCP against a server that
// lists a file as flat SymbolInformation (real gopls does) asks the server about
// the symbol's name, not the `func` keyword its declaration starts with.
func TestMCPIDOnAFlatServerAsksAboutTheName(t *testing.T) {
	dir, file := symWorkspace(t, nil)
	symScenario(t, symFlat(file), map[string]any{methodReferences: json.RawMessage(positionEcho)})
	cs := mcpSession(t, t.TempDir())

	col := strings.LastIndex(strings.Split(symSource, "\n")[13], "Greet") + 1
	res := callTool(t, cs, "references", map[string]any{"workspace": dir, "id": "sym.go::Greeter.Greet#method"})
	if res.IsError {
		t.Fatalf("isError: %s", resultText(t, res))
	}
	var data struct {
		Results []struct {
			Start struct{ Line, Column int }
		}
	}
	json.Unmarshal(mcpEnvelope(t, resultText(t, res)).Data, &data)
	if len(data.Results) != 1 || data.Results[0].Start.Line != 14 || data.Results[0].Start.Column != col {
		t.Errorf("references asked about %+v, want 14:%d", data.Results, col)
	}
}

func TestMCPSymbolToolsAreReadOnly(t *testing.T) {
	cs := mcpSession(t, t.TempDir())
	listed, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, tool := range listed.Tools {
		switch tool.Name {
		case "outline", "source", "context", "tree", "repo_outline", "file":
			seen++
			if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
				t.Errorf("%s is not marked read-only", tool.Name)
			}
			for prop := range schemaOf(t, tool)["properties"].(map[string]any) {
				if prop == "apply" || prop == "allow_dirty" {
					t.Errorf("%s offers %s", tool.Name, prop)
				}
			}
		}
	}
	if seen != 6 {
		t.Errorf("found %d of the 6 symbol-retrieval tools", seen)
	}
}
