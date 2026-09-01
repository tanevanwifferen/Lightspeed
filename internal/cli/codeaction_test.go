package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// actionLoc is where the code actions are asked for: the use of Old in
// b.go, at 1-based byte column 26.
func actionLoc(dir string) string { return filepath.Join(dir, "b.go") + ":3:26" }

// bFileEdit renames the use in b.go and nothing else, so that a test
// can tell which of several scripted routes produced the edit.
func bFileEdit(dir string) map[string]any {
	return changesEdit(map[string][]any{
		filepath.Join(dir, "b.go"): {textEdit(2, 25, 28, "New")},
	})
}

// bFileRenamed is the fixture's b.go once that edit lands.
const bFileRenamed = "package fixture\n\nfunc useB() int { return New() }\n"

// TestCodeActionListsWhatTheServerOffers: with no action selected the
// command is a listing, and the 1-based index in the label is what
// --index takes.
func TestCodeActionListsWhatTheServerOffers(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	scenario{
		capabilities: mutationServerCaps(nil),
		results: map[string]any{methodCodeAction: []any{
			map[string]any{"title": "Replace with New", "kind": "refactor.rewrite", "edit": bFileEdit(dir)},
			map[string]any{"title": "Organize imports", "kind": "source.organizeImports", "isPreferred": true},
		}},
	}.apply(t)

	code, stdout, stderr := runMain("codeaction", actionLoc(dir), "--format", "text")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	for _, want := range []string{"[1] Replace with New", "[2] Organize imports"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("listing is missing %q:\n%s", want, stdout)
		}
	}

	code, stdout, _ = runMain("codeaction", actionLoc(dir))
	if code != ExitOK {
		t.Fatalf("json: exit code = %d, want %d", code, ExitOK)
	}
	got := decodeResults(t, stdout)
	if got.Kind != "codeaction" || got.Count != 2 {
		t.Fatalf("payload = %+v, want kind codeaction with 2 results", got)
	}
	if got.Results[0].Kind != "refactor.rewrite" {
		t.Errorf("result 0 kind = %q, want refactor.rewrite", got.Results[0].Kind)
	}
	assertUnchanged(t, dir, before)
}

// TestCodeActionEmptyListIsProblems: no actions at that position is an
// authoritative empty answer, which is exit 1 exactly as it is for
// references.
func TestCodeActionEmptyListIsProblems(t *testing.T) {
	dir := tree(t, fixtureFiles)
	scenario{
		capabilities: mutationServerCaps(nil),
		results:      map[string]any{methodCodeAction: []any{}},
	}.apply(t)

	if code, _, _ := runMain("codeaction", actionLoc(dir), "--settle", "20ms"); code != ExitProblems {
		t.Errorf("exit code = %d, want %d", code, ExitProblems)
	}
}

// TestCodeActionAppliesChosenAction: by index and by title, previewing
// by default and writing with --apply.
func TestCodeActionAppliesChosenAction(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector []string
	}{
		{"by index", []string{"--index", "1"}},
		{"by title", []string{"--title", "Replace with New"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tree(t, fixtureFiles)
			before := snapshot(t, dir)
			scenario{
				capabilities: mutationServerCaps(nil),
				results: map[string]any{methodCodeAction: []any{
					map[string]any{"title": "Replace with New", "kind": "refactor.rewrite", "edit": bFileEdit(dir)},
				}},
			}.apply(t)

			args := append([]string{"codeaction", actionLoc(dir)}, tc.selector...)
			code, stdout, stderr := runMain(args...)
			if code != ExitOK {
				t.Fatalf("preview exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
			}
			if !strings.Contains(stdout, "diff --git a/b.go b/b.go") {
				t.Errorf("preview is not a diff:\n%s", stdout)
			}
			assertUnchanged(t, dir, before)

			code, _, stderr = runMain(append(args, "--apply", "--allow-dirty")...)
			if code != ExitOK {
				t.Fatalf("apply exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
			}
			if got := snapshot(t, dir)["b.go"]; got != bFileRenamed {
				t.Errorf("b.go = %q, want %q", got, bFileRenamed)
			}
		})
	}
}

// TestCodeActionResolvesLazyEdit: an action whose edit is computed
// lazily is filled in with codeAction/resolve, which the server has to
// have advertised first (PLAN §5.4).
func TestCodeActionResolvesLazyEdit(t *testing.T) {
	dir := tree(t, fixtureFiles)
	s := scenario{
		capabilities: mutationServerCaps(map[string]any{
			"codeActionProvider": map[string]any{"resolveProvider": true},
		}),
		results: map[string]any{
			methodCodeAction: []any{
				map[string]any{"title": "Replace with New", "kind": "quickfix", "data": map[string]any{"id": 7}},
			},
			methodCodeActionResolve: map[string]any{
				"title": "Replace with New", "kind": "quickfix",
				"data": map[string]any{"id": 7},
				"edit": bFileEdit(dir),
			},
		},
	}
	readTrace := s.traceTo(t)
	s.apply(t)

	code, _, stderr := runMain("codeaction", actionLoc(dir), "--index", "1", "--apply", "--allow-dirty")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	if got := snapshot(t, dir)["b.go"]; got != bFileRenamed {
		t.Errorf("b.go = %q, want %q", got, bFileRenamed)
	}

	var resolved bool
	for _, msg := range readTrace() {
		if msg.Method == methodCodeActionResolve {
			resolved = true
			// The server's private `data` has to come back untouched,
			// or the resolve is answering about a different action.
			if !strings.Contains(string(msg.Params), `"id":7`) {
				t.Errorf("resolve params lost the action's data: %s", msg.Params)
			}
		}
	}
	if !resolved {
		t.Error("codeAction/resolve was never called")
	}
}

// TestCodeActionRunsServerCommand: an action that is only a command
// still ends up in the transactional applier — the edits arrive as
// workspace/applyEdit while the command runs, and are staged rather
// than written by the server.
func TestCodeActionRunsServerCommand(t *testing.T) {
	dir := tree(t, fixtureFiles)
	s := scenario{
		capabilities: mutationServerCaps(map[string]any{
			"executeCommandProvider": map[string]any{"commands": []any{"fixture.rewrite"}},
		}),
		results: map[string]any{
			methodCodeAction: []any{
				map[string]any{
					"title": "Rewrite via command",
					"kind":  "source",
					"command": map[string]any{
						"title": "rewrite", "command": "fixture.rewrite",
						"arguments": []any{map[string]any{"file": "b.go"}},
					},
				},
			},
			methodExecuteCommand: pushEdits(bFileEdit(dir)),
		},
	}
	readTrace := s.traceTo(t)
	s.apply(t)

	code, stdout, stderr := runMain("codeaction", actionLoc(dir), "--index", "1",
		"--apply", "--allow-dirty", "--format", "json")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	if got := snapshot(t, dir)["b.go"]; got != bFileRenamed {
		t.Errorf("b.go = %q, want %q", got, bFileRenamed)
	}

	env := decodeEnvelope(t, stdout)
	if !env.OK {
		t.Fatalf("envelope ok = false: %+v", env.Error)
	}
	// Running a command to compute a preview is a side effect, and it
	// has to be said out loud.
	if !strings.Contains(strings.Join(env.Warnings, "\n"), "fixture.rewrite") {
		t.Errorf("warnings do not mention the command that was run: %v", env.Warnings)
	}

	var executed bool
	for _, msg := range readTrace() {
		if msg.Method == methodExecuteCommand {
			executed = true
			if !strings.Contains(string(msg.Params), `"file":"b.go"`) {
				t.Errorf("executeCommand lost the action's arguments: %s", msg.Params)
			}
		}
	}
	if !executed {
		t.Error("workspace/executeCommand was never called")
	}
}

// TestCodeActionRefusesTwoPushedEdits: two independent edit sets cannot
// be composed without knowing what the second was computed against, so
// the command refuses both rather than applying one.
func TestCodeActionRefusesTwoPushedEdits(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	scenario{
		capabilities: mutationServerCaps(map[string]any{
			"executeCommandProvider": map[string]any{"commands": []any{"fixture.rewrite"}},
		}),
		results: map[string]any{
			methodCodeAction: []any{
				map[string]any{
					"title":   "Rewrite via command",
					"command": map[string]any{"title": "rewrite", "command": "fixture.rewrite"},
				},
			},
			methodExecuteCommand: pushEdits(
				bFileEdit(dir),
				changesEdit(map[string][]any{
					filepath.Join(dir, "c.go"): {textEdit(2, 25, 28, "New")},
				}),
			),
		},
	}.apply(t)

	code, stdout, _ := runMain("codeaction", actionLoc(dir), "--index", "1", "--apply", "--allow-dirty")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "edit_conflict" {
		t.Errorf("envelope = %+v, want ok:false code edit_conflict", env)
	}
	assertUnchanged(t, dir, before)
}

// TestCodeActionRefusesUnappliableAction: an action with no edit, no
// resolve and no command cannot be applied, and saying so beats
// pretending it did nothing.
func TestCodeActionRefusesUnappliableAction(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	scenario{
		capabilities: mutationServerCaps(nil),
		results: map[string]any{methodCodeAction: []any{
			map[string]any{"title": "Mystery action", "kind": "source"},
		}},
	}.apply(t)

	code, stdout, _ := runMain("codeaction", actionLoc(dir), "--index", "1", "--apply", "--allow-dirty")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNoServer, stdout)
	}
	if env := decodeEnvelope(t, stdout); env.OK || env.Error == nil || env.Error.Code != "unsupported_method" {
		t.Errorf("envelope = %+v, want ok:false code unsupported_method", env)
	}
	assertUnchanged(t, dir, before)
}

// TestCodeActionRefusesDisabledAction: the protocol says a disabled
// action must not be applied, and the reason is worth repeating back.
func TestCodeActionRefusesDisabledAction(t *testing.T) {
	dir := tree(t, fixtureFiles)
	before := snapshot(t, dir)
	scenario{
		capabilities: mutationServerCaps(nil),
		results: map[string]any{methodCodeAction: []any{
			map[string]any{
				"title":    "Extract function",
				"edit":     bFileEdit(dir),
				"disabled": map[string]any{"reason": "selection spans two functions"},
			},
		}},
	}.apply(t)

	code, stdout, _ := runMain("codeaction", actionLoc(dir), "--index", "1", "--apply", "--allow-dirty")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	if !strings.Contains(stdout, "selection spans two functions") {
		t.Errorf("the refusal does not repeat the server's reason: %s", stdout)
	}
	assertUnchanged(t, dir, before)
}

// TestCodeActionSelectionErrors covers the ways an action can fail to
// be selected, none of which should start a write.
func TestCodeActionSelectionErrors(t *testing.T) {
	dir := tree(t, fixtureFiles)
	actions := []any{
		map[string]any{"title": "Twin", "edit": bFileEdit(dir)},
		map[string]any{"title": "Twin", "edit": bFileEdit(dir)},
	}
	scenario{
		capabilities: mutationServerCaps(nil),
		results:      map[string]any{methodCodeAction: actions},
	}.apply(t)
	loc := actionLoc(dir)

	for _, tc := range []struct {
		name string
		args []string
		want int
	}{
		{"both selectors", []string{"codeaction", loc, "--index", "1", "--title", "Twin"}, ExitUsage},
		{"negative index", []string{"codeaction", loc, "--index", "-1"}, ExitUsage},
		{"apply without a selection", []string{"codeaction", loc, "--apply"}, ExitUsage},
		{"index past the end", []string{"codeaction", loc, "--index", "9"}, ExitUsage},
		{"ambiguous title", []string{"codeaction", loc, "--title", "Twin"}, ExitUsage},
		{"unknown title", []string{"codeaction", loc, "--title", "Nope"}, ExitProblems},
		{"diff for a listing", []string{"codeaction", loc, "--format", "diff"}, ExitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, _ := runMain(tc.args...)
			if code != tc.want {
				t.Fatalf("exit code = %d, want %d; stdout: %s", code, tc.want, stdout)
			}
			if env := decodeEnvelope(t, stdout); env.OK {
				t.Errorf("envelope ok = true, want a failure")
			}
		})
	}
}

// TestCodeActionRangeReachesTheServer: a range location asks about the
// whole selection, converted from byte columns to UTF-16 (PLAN §5.1).
func TestCodeActionRangeReachesTheServer(t *testing.T) {
	_, file := cjkFixture(t)
	s := scenario{
		capabilities: mutationServerCaps(nil),
		results:      map[string]any{methodCodeAction: []any{}},
	}
	readTrace := s.traceTo(t)
	s.apply(t)

	// Line 6 is "\treturn 変数 + 変数": byte columns 9 to 24 are the two
	// identifiers, which is UTF-16 8 to 15.
	runMain("codeaction", file+":6:9-6:24", "--settle", "20ms")

	var seen bool
	for _, msg := range readTrace() {
		if msg.Method != methodCodeAction {
			continue
		}
		seen = true
		want := `"range":{"start":{"line":5,"character":8},"end":{"line":5,"character":15}}`
		if !strings.Contains(string(msg.Params), want) {
			t.Errorf("codeAction range = %s, want it to contain %s", msg.Params, want)
		}
	}
	if !seen {
		t.Fatal("textDocument/codeAction was never called")
	}
}
