package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

// `source` and `context` (docs/DECISIONS.md D31): the doc-comment rule, and
// naming a symbol the way every other command does.

// TestLeadingComment: what belongs to the declaration below it.
func TestLeadingComment(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want int // the first line of the declaration's own text, 0-based
	}{
		{"line comments", "// a\n// b\nfunc f() {}\n", 0},
		{"a blank line stops it", "// a\n\nfunc f() {}\n", 2},
		{"block whose last line is text", "/*\n * a\n b */\nfunc f() {}\n", 0},
		{"block with no stars at all", "/*\na\nb\n*/\nfunc f() {}\n", 0},
		{"block closed on a text line, opened on a text line", "/* a\nb\nc */\nfunc f() {}\n", 0},
		{"one-line block", "/* a */\nfunc f() {}\n", 0},
		{"a blank line inside the block", "/*\n a\n\n b */\nfunc f() {}\n", 0},
		{"a licence, blank line, then the doc block", "/* licence */\n\n/* doc\n more */\nfunc f() {}\n", 2},
		{"code that merely ends in a comment", "x := 1 /* c */\nfunc f() {}\n", 1},
		{"the close of an earlier comment above", "/* a */\nint y;\n/* b */\nfunc f() {}\n", 2},
		{"a block that never opens", "b */\nfunc f() {}\n", 1},
		{"CRLF, block ending in text", "/*\r\n * a\r\n b */\r\nfunc f() {}\r\n", 0},
		{"CRLF, line comments", "// a\r\n// b\r\nfunc f() {}\r\n", 0},
		{"pointer deref is not a comment", "*p = 1\nfunc f() {}\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			x := newLineIndex([]byte(tc.src))
			decl := strings.Count(tc.src[:strings.Index(tc.src, "func f")], "\n")
			if got := x.LeadingStart(decl); got != tc.want {
				t.Errorf("leadingStart(%d) = %d, want %d\n%q", decl, got, tc.want, tc.src)
			}
		})
	}
}

// TestSourceTakesBlockCommentsAndCRLF: through the command, the multi-line
// block comment above Greet comes with it, and a CRLF file's source keeps its
// CRLFs, byte for byte.
func TestSourceTakesBlockCommentsAndCRLF(t *testing.T) {
	_, file := symWorkspace(t, nil)
	symScenario(t, symHierarchical(), nil)
	const id = "sym.go::Greeter.Greet#method"

	// The blank line 11 and the comment on line 12 (0-based) become a two-line
	// block whose last line is text: the line count, and so every range, is
	// unchanged.
	block := strings.Replace(symSource, "\n\n// Greet は挨拶を返す 👋\n", "\n/* Greet は挨拶\nを返す 👋 */\n", 1)
	write(t, file, block)
	wantBlock := "/* Greet は挨拶\nを返す 👋 */\n" + strings.SplitN(greetSource, "\n", 2)[1]

	_, stdout, _ := runMain("source", id)
	data, _ := symbolSourceData(t, stdout)
	if got := data.Symbols[0]; got.Source != wantBlock || got.Line != 12 || got.EndLine != 16 || got.TotalLines != 5 {
		t.Errorf("block comment: %q lines %d-%d (%d), want %q lines 12-16", got.Source, got.Line, got.EndLine, got.TotalLines, wantBlock)
	}

	crlf := strings.ReplaceAll(symSource, "\n", "\r\n")
	write(t, file, crlf)
	wantCRLF := strings.ReplaceAll(greetSource, "\n", "\r\n")
	_, stdout, _ = runMain("source", id)
	data, _ = symbolSourceData(t, stdout)
	if got := data.Symbols[0]; got.Source != wantCRLF || got.Line != 13 || got.EndLine != 16 || got.Hash != sha(wantCRLF) {
		t.Errorf("CRLF: %q lines %d-%d, want %q lines 13-16", got.Source, got.Line, got.EndLine, wantCRLF)
	}
	// A CRLF file's own doc comment too, and its cap on a line boundary.
	_, stdout, _ = runMain("source", id, "--max-lines", "1")
	data, _ = symbolSourceData(t, stdout)
	if got := data.Symbols[0]; got.Source != "// Greet は挨拶を返す 👋" || got.ReturnedLines != 1 || !got.Truncated {
		t.Errorf("CRLF --max-lines 1: %+v", got)
	}
}

// TestSourceAndContextTakeID: --id is a way of naming the symbol, like on
// every command that takes a location, and context also takes a location and a
// symbol.
func TestSourceAndContextTakeID(t *testing.T) {
	_, file := symWorkspace(t, nil)
	symScenario(t, symHierarchical(), map[string]any{
		methodWorkspaceSymbol: []any{map[string]any{
			"name": "Greet", "kind": 6, "containerName": "Greeter",
			"location": map[string]any{"uri": "file://" + file, "range": lspRange(13, 0, 15, 1)},
		}},
	})
	const id = "sym.go::Greeter.Greet#method"

	// source --id, alone and with arguments (in the order: arguments, then --id).
	code, stdout, stderr := runMain("source", "--id", id)
	if code != ExitOK {
		t.Fatalf("source --id: exit %d; stderr: %s\n%s", code, stderr, stdout)
	}
	if data, _ := symbolSourceData(t, stdout); data.Count != 1 || data.Symbols[0].Source != greetSource {
		t.Errorf("source --id: %+v", data)
	}
	_, stdout, _ = runMain("source", "sym.go::Long#function", "--id", id)
	if data, _ := symbolSourceData(t, stdout); data.Count != 2 || data.Symbols[0].ID != "sym.go::Long#function" || data.Symbols[1].ID != id {
		t.Errorf("source with an argument and --id: %+v", data)
	}
	if code, stdout, _ := runMain("source"); code != ExitUsage || !strings.Contains(stdout, "usage") {
		t.Errorf("source with nothing to read: exit %d\n%s", code, stdout)
	}
	if code, stdout, _ := runMain("source", "--id", "sym.go::Greeter.Greeet#method"); code != ExitProblems || !strings.Contains(stdout, "stale_id") {
		t.Errorf("source --id with a stale id: exit %d\n%s", code, stdout)
	}

	// context: an id, a location, a symbol; all the same answer.
	var want contextData
	for _, args := range [][]string{
		{"--id", id},
		{id},
		{"sym.go:14:20"},
		{"--symbol", "Greeter.Greet"},
	} {
		code, stdout, stderr := runMain(append([]string{"context"}, args...)...)
		if code != ExitOK {
			t.Errorf("context %v: exit %d; stderr: %s\n%s", args, code, stderr, stdout)
			continue
		}
		var got contextData
		okData(t, stdout, &got)
		if got.Source != greetSource || got.ID != id && args[0] != "--symbol" || got.Header == nil || got.Hover == "" {
			t.Errorf("context %v: %+v", args, got)
		}
		if want.Source != "" && got.Source != want.Source {
			t.Errorf("context %v differs from the others", args)
		}
		want = got
	}
	for _, args := range [][]string{
		{},
		{"--id", id, id},
		{"--id", id, "--symbol", "Greeter.Greet"},
		{id, "--symbol", "Greeter.Greet"},
	} {
		if code, stdout, _ := runMain(append([]string{"context"}, args...)...); code != ExitUsage {
			t.Errorf("context %v: exit %d, want %d (one way of naming the symbol)\n%s", args, code, ExitUsage, stdout)
		}
	}
}

// TestMCPSourceTakesID: `id` (one) as well as `ids` (many) on source, and
// `id`, `location` and `symbol` on context.
func TestMCPSourceTakesID(t *testing.T) {
	dir, file := symWorkspace(t, nil)
	symScenario(t, symHierarchical(), map[string]any{
		methodWorkspaceSymbol: []any{map[string]any{
			"name": "Greet", "kind": 6, "containerName": "Greeter",
			"location": map[string]any{"uri": "file://" + file, "range": lspRange(13, 0, 15, 1)},
		}},
	})
	cs := mcpSession(t, t.TempDir())
	const id = "sym.go::Greeter.Greet#method"
	call := func(tool string, args map[string]any) (sourceData, contextData, bool) {
		args["workspace"] = dir
		res := callTool(t, cs, tool, args)
		var s sourceData
		var c contextData
		env := mcpEnvelope(t, resultText(t, res))
		json.Unmarshal(env.Data, &s)
		json.Unmarshal(env.Data, &c)
		return s, c, res.IsError
	}

	if s, _, isErr := call("source", map[string]any{"id": id}); isErr || s.Count != 1 || s.Symbols[0].Source != greetSource {
		t.Errorf("source id: isError=%v %+v", isErr, s)
	}
	if s, _, isErr := call("source", map[string]any{"id": id, "ids": []any{"sym.go::Long#function"}}); isErr || s.Count != 2 {
		t.Errorf("source id and ids: isError=%v %+v", isErr, s)
	}
	if _, _, isErr := call("source", map[string]any{}); !isErr {
		t.Errorf("source with neither id nor ids succeeded")
	}
	for _, args := range []map[string]any{{"id": id}, {"location": "sym.go:14:20"}, {"location": id}, {"symbol": "Greeter.Greet"}} {
		if _, c, isErr := call("context", args); isErr || c.Source != greetSource || c.Hover == "" {
			t.Errorf("context %v: isError=%v %+v", args, isErr, c)
		}
	}
}
