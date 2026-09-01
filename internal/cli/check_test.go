package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// checkFixture writes a two-file Go module and returns the root and
// the two files. The source is the CJK fixture again, because a
// diagnostic's column has exactly the same UTF-16-versus-bytes problem
// a reference's does, and `check` resolves it through the same Mapper.
func checkFixture(t *testing.T) (dir, main, helper string) {
	t.Helper()
	dir = t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module cjk\n\ngo 1.27\n")
	main = filepath.Join(dir, "cjk.go")
	write(t, main, cjkSource)
	helper = filepath.Join(dir, "helper.go")
	write(t, helper, "package cjk\n\nfunc helper() {}\n")
	return dir, main, helper
}

// TestCheckMixedSeveritiesDecideTheExitCode is PLAN §4's rule for
// `check`: exit 1 if any diagnostic is an error, and not otherwise. A
// tree with warnings in it is a tree that compiles.
func TestCheckMixedSeveritiesDecideTheExitCode(t *testing.T) {
	dir, main, helper := checkFixture(t)

	t.Run("an error anywhere is exit 1", func(t *testing.T) {
		scenario{
			capabilities: m5Capabilities(nil),
			diagnostics: map[string]any{
				main: []any{
					diagnostic(2, 4, 6, 2, "変数 is never used", "compiler", "unusedvar"),
					diagnostic(5, 8, 10, 1, "undefined: 変数", "compiler", "UndeclaredName"),
				},
				helper: []any{
					diagnostic(2, 5, 11, 3, "helper is unexported", "staticcheck", "ST1000"),
				},
			},
		}.apply(t)

		code, stdout, stderr := runMain("check", dir, "--settle", "20ms")
		if code != ExitProblems {
			t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitProblems, stderr, stdout)
		}
		got := decodeDiagnostics(t, stdout)
		if got.Count != 3 || got.Total != 3 || got.Errors != 1 {
			t.Fatalf("payload = %+v, want 3 diagnostics of which 1 error", got)
		}
		// Sorted by file then position: cjk.go before helper.go, and
		// line 3 before line 6. Paths are relative to the workspace
		// root, and columns are byte columns.
		want := []struct {
			path     string
			severity string
			line     int
			col      int
			code     string
		}{
			{"cjk.go", "warning", 3, 5, "unusedvar"},
			{"cjk.go", "error", 6, 9, "UndeclaredName"},
			{"helper.go", "info", 3, 6, "ST1000"},
		}
		for i, w := range want {
			d := got.Diagnostics[i]
			if d.Path != w.path || d.Severity != w.severity || d.Start.Line != w.line ||
				d.Start.Column != w.col || d.Code != w.code {
				t.Errorf("diagnostic %d = %s:%d:%d %s [%s], want %s:%d:%d %s [%s]",
					i, d.Path, d.Start.Line, d.Start.Column, d.Severity, d.Code,
					w.path, w.line, w.col, w.severity, w.code)
			}
		}
	})

	t.Run("warnings only is exit 0", func(t *testing.T) {
		scenario{
			capabilities: m5Capabilities(nil),
			diagnostics: map[string]any{
				main: []any{diagnostic(2, 4, 6, 2, "変数 is never used", "compiler", "unusedvar")},
			},
		}.apply(t)

		code, stdout, stderr := runMain("check", dir, "--settle", "20ms")
		if code != ExitOK {
			t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
		}
		if got := decodeDiagnostics(t, stdout); got.Count != 1 || got.Errors != 0 {
			t.Errorf("payload = %+v, want one non-error diagnostic", got)
		}
	})

	t.Run("a clean tree is exit 0 with an empty set", func(t *testing.T) {
		scenario{
			capabilities: m5Capabilities(nil),
			// Every file mentioned, all of them clean: the server has
			// spoken about each one, so this is an answer.
			diagnostics: map[string]any{main: []any{}, helper: []any{}},
		}.apply(t)

		code, stdout, stderr := runMain("check", dir, "--settle", "20ms")
		if code != ExitOK {
			t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
		}
		if got := decodeDiagnostics(t, stdout); got.Count != 0 || got.Errors != 0 {
			t.Errorf("payload = %+v, want an empty set", got)
		}
	})
}

// TestCheckLimitDoesNotHideErrors: --limit is a display concession. If
// it could turn exit 1 into exit 0 then `check --limit 1` would be a
// way to make CI pass.
func TestCheckLimitDoesNotHideErrors(t *testing.T) {
	dir, main, _ := checkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		diagnostics: map[string]any{
			main: []any{
				diagnostic(2, 4, 6, 2, "変数 is never used", "compiler", "unusedvar"),
				diagnostic(5, 8, 10, 1, "undefined: 変数", "compiler", "UndeclaredName"),
			},
		},
	}.apply(t)

	code, stdout, _ := runMain("check", dir, "--limit", "1", "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d (the error is past the limit but still real)", code, ExitProblems)
	}
	got := decodeDiagnostics(t, stdout)
	if got.Count != 1 || got.Total != 2 || !got.Truncated {
		t.Errorf("payload = %+v, want 1 of 2 with truncated:true", got)
	}
	env := decodeEnvelope(t, stdout)
	if !hasWarning(env.Warnings, "truncated") {
		t.Errorf("warnings = %q, want a truncation warning", env.Warnings)
	}
}

// TestCheckSilentFileIsNotACleanFile is the decision this command
// exists to get right. A file the server never published anything
// about is unknown, not clean, and reporting it as clean is the
// failure PLAN §5.2 is about — with the added twist that an agent
// trusting a silent `check` will commit.
func TestCheckSilentFileIsNotACleanFile(t *testing.T) {
	dir, main, helper := checkFixture(t)
	silent := scenario{
		capabilities: m5Capabilities(nil),
		diagnostics: map[string]any{
			main: []any{},
			// A JSON null: the server never mentions helper.go.
			helper: nil,
		},
	}

	t.Run("exit 5 by default", func(t *testing.T) {
		silent.apply(t)
		code, stdout, stderr := runMain("check", dir, "--timeout", "400ms", "--settle", "20ms")
		if code != ExitNotReady {
			t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitNotReady, stderr, stdout)
		}
		env := decodeEnvelope(t, stdout)
		if env.OK || env.Error == nil || env.Error.Code != "not_ready" {
			t.Fatalf("envelope = %+v, want ok:false code not_ready", env)
		}
		if !strings.Contains(env.Error.Message, "helper.go") {
			t.Errorf("message %q does not name the file the server was silent about", env.Error.Message)
		}
		if strings.Contains(stdout, `"diagnostics"`) {
			t.Errorf("a report was printed for a tree we cannot vouch for: %s", stdout)
		}
	})

	t.Run("--allow-silent reports it and says so", func(t *testing.T) {
		silent.apply(t)
		code, stdout, stderr := runMain("check", dir, "--allow-silent", "--timeout", "400ms", "--settle", "20ms")
		if code != ExitOK {
			t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
		}
		env := decodeEnvelope(t, stdout)
		if !hasWarning(env.Warnings, "helper.go") {
			t.Errorf("warnings = %q, want one naming the silent file", env.Warnings)
		}
		if !hasWarning(env.Warnings, "assumption") {
			t.Errorf("warnings = %q, want one saying the clean report is an assumption", env.Warnings)
		}
	})
}

// TestCheckWhileIndexingExitsNotReady: the readiness gate applies to
// diagnostics exactly as it does to a query. A server still loading
// has not finished finding the problems.
func TestCheckWhileIndexingExitsNotReady(t *testing.T) {
	dir, main, helper := checkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		indexing:     true,
		diagnostics:  map[string]any{main: []any{}, helper: []any{}},
	}.apply(t)

	code, stdout, stderr := runMain("check", dir, "--timeout", "400ms", "--settle", "20ms")
	if code != ExitNotReady {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitNotReady, stderr, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "not_ready" {
		t.Fatalf("error = %+v, want code not_ready", env.Error)
	}
	if !strings.Contains(env.Error.Message, "indexing") {
		t.Errorf("message %q does not say the server was still indexing", env.Error.Message)
	}
}

// TestCheckTextFormat: the grep-compatible line compilers emit, which
// editors and CI log parsers already understand.
func TestCheckTextFormat(t *testing.T) {
	dir, main, _ := checkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		diagnostics: map[string]any{
			main: []any{diagnostic(5, 8, 10, 1, "undefined: 変数", "compiler", "UndeclaredName")},
		},
	}.apply(t)

	code, stdout, stderr := runMain("check", dir, "--format", "text", "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitProblems, stderr)
	}
	want := "cjk.go:6:9: error: undefined: 変数 [compiler:UndeclaredName]\n"
	if stdout != want {
		t.Errorf("text output\n got: %q\nwant: %q", stdout, want)
	}
}

// TestCheckSARIF compares the whole SARIF log against a golden file.
//
// The workspace root is a temporary directory, so the one absolute URI
// in the log — the SRCROOT base id every result is relative to — is
// replaced with a placeholder before comparison. Everything else must
// match byte for byte: a SARIF consumer is a third-party tool, and a
// field quietly changing shape is a compatibility break we should have
// to notice.
func TestCheckSARIF(t *testing.T) {
	dir, main, helper := checkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		diagnostics: map[string]any{
			main: []any{
				diagnostic(2, 4, 6, 2, "変数 is never used", "compiler", "unusedvar"),
				diagnostic(5, 8, 10, 1, "undefined: 変数", "compiler", "UndeclaredName"),
			},
			helper: []any{
				map[string]any{
					"range": map[string]any{
						"start": map[string]any{"line": 2, "character": 5},
						"end":   map[string]any{"line": 2, "character": 11},
					},
					"severity": 4,
					"message":  "helper is unused",
					"source":   "staticcheck",
					"code":     "U1000",
					"tags":     []int{1},
				},
			},
		},
	}.apply(t)

	code, stdout, stderr := runMain("check", dir, "--format", "sarif", "--indent", "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitProblems, stderr, stdout)
	}
	// SARIF is not wrapped in the envelope: a SARIF consumer expects
	// SARIF and nothing else.
	if strings.Contains(stdout, `"version": 1`) {
		t.Errorf("sarif output is wrapped in the lightspeed envelope: %s", stdout)
	}

	golden := filepath.Join("testdata", "check.sarif.json")
	got := strings.ReplaceAll(stdout, rootURI(t, dir), "file:///SRCROOT/")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("sarif output does not match %s\n got: %s\nwant: %s", golden, got, want)
	}
}

// rootURI is the file:// URI of dir with its trailing slash, as the
// SARIF renderer writes it.
func rootURI(t *testing.T, dir string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		resolved = dir
	}
	return "file://" + resolved + "/"
}

// TestCheckPullModel: where a server advertises the pull model, `check`
// uses it — a request that returns beats any amount of inference about
// pushed notifications. The assertion is on the wire, because the two
// modes are indistinguishable from the output alone.
func TestCheckPullModel(t *testing.T) {
	_, main, _ := checkFixture(t)
	sc := scenario{
		capabilities: m5Capabilities(map[string]any{
			"diagnosticProvider": map[string]any{"interFileDependencies": false, "workspaceDiagnostics": false},
		}),
		results: map[string]any{
			methodDocumentDiagnostic: map[string]any{
				"kind": "full",
				"items": []any{
					diagnostic(5, 8, 10, 1, "undefined: 変数", "compiler", "UndeclaredName"),
				},
			},
		},
		// Deliberately also scripted to push something different. The
		// pull answer is the one that must be reported.
		diagnostics: map[string]any{
			main: []any{diagnostic(2, 4, 6, 1, "this push must be ignored", "push", "PUSH")},
		},
	}
	readTrace := sc.traceTo(t)
	sc.apply(t)

	code, stdout, stderr := runMain("check", main, "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitProblems, stderr, stdout)
	}
	got := decodeDiagnostics(t, stdout)
	if got.Count != 1 || got.Diagnostics[0].Code != "UndeclaredName" {
		t.Fatalf("payload = %+v, want the pulled diagnostic", got)
	}
	requestParams(t, readTrace(), methodDocumentDiagnostic)

	// And --diagnostics push must ignore the pull model it advertises.
	sc.apply(t)
	code, stdout, _ = runMain("check", main, "--diagnostics", "push", "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("push mode: exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	if got := decodeDiagnostics(t, stdout); got.Count != 1 || got.Diagnostics[0].Code != "PUSH" {
		t.Errorf("push mode payload = %+v, want the pushed diagnostic", got)
	}
}

// TestCheckPullModelRefusedWithoutCapability: --diagnostics pull is a
// promise the server has to have made. Calling the method anyway is
// exactly what PLAN §5.4 forbids.
func TestCheckPullModelRefusedWithoutCapability(t *testing.T) {
	_, main, _ := checkFixture(t)
	scenario{capabilities: m5Capabilities(nil)}.apply(t)

	code, stdout, _ := runMain("check", main, "--diagnostics", "pull", "--settle", "20ms")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNoServer, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "unsupported_method" {
		t.Errorf("error = %+v, want code unsupported_method", env.Error)
	}
}

// TestCheckSingleFile: naming a file checks that file, and nothing in
// the tree around it is opened.
func TestCheckSingleFile(t *testing.T) {
	_, main, helper := checkFixture(t)
	sc := scenario{
		capabilities: m5Capabilities(nil),
		diagnostics:  map[string]any{main: []any{}, helper: nil},
	}
	readTrace := sc.traceTo(t)
	sc.apply(t)

	// helper.go is scripted silent, so if `check main` opened it the
	// command would exit 5 instead of 0.
	code, stdout, stderr := runMain("check", main, "--timeout", "400ms", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	for _, msg := range readTrace() {
		if msg.Method != "textDocument/didOpen" {
			continue
		}
		if strings.Contains(string(msg.Params), "helper.go") {
			t.Errorf("check <file> opened a file it was not asked about: %s", msg.Params)
		}
	}
}

// TestCheckAnnouncesEachFilesOwnLanguage: one workspace holds several
// kinds of file, and `check .` on a Go module opens both the .go files
// and the go.mod. Announcing the latter as "go" would tell the server
// something untrue about a file it is about to parse — and the server
// that resolved the workspace is not necessarily the one whose
// language id fits every file in it.
func TestCheckAnnouncesEachFilesOwnLanguage(t *testing.T) {
	dir, main, helper := checkFixture(t)
	sc := scenario{
		capabilities: m5Capabilities(nil),
		diagnostics:  map[string]any{main: []any{}, helper: []any{}},
	}
	readTrace := sc.traceTo(t)
	sc.apply(t)

	code, stdout, stderr := runMain("check", dir, "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}

	got := map[string]string{}
	for _, msg := range readTrace() {
		if msg.Method != "textDocument/didOpen" {
			continue
		}
		var opened struct {
			TextDocument struct {
				URI        string `json:"uri"`
				LanguageID string `json:"languageId"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(msg.Params, &opened); err != nil {
			t.Fatal(err)
		}
		got[filepath.Base(opened.TextDocument.URI)] = opened.TextDocument.LanguageID
	}
	for name, want := range map[string]string{
		"cjk.go":    "go",
		"helper.go": "go",
		"go.mod":    "gomod",
	} {
		if got[name] != want {
			t.Errorf("%s was opened as %q, want %q (opened: %v)", name, got[name], want, got)
		}
	}
}

// TestCheckUsage covers the exit-2 surface of the flags this command
// adds, plus the formats that mean nothing for diagnostics.
func TestCheckUsage(t *testing.T) {
	dir, _, _ := checkFixture(t)
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown collection mode", []string{"check", dir, "--diagnostics", "guess"}, "usage"},
		{"zero max-files", []string{"check", dir, "--max-files", "0"}, "usage"},
		{"diff format", []string{"check", dir, "--format", "diff"}, "unsupported_format"},
		{"no such path", []string{"check", filepath.Join(dir, "gone")}, "no_such_file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario{capabilities: m5Capabilities(nil)}.apply(t)
			code, stdout, _ := runMain(tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitUsage, stdout)
			}
			env := decodeEnvelope(t, stdout)
			if env.OK || env.Error == nil || string(env.Error.Code) != tc.want {
				t.Errorf("envelope = %+v, want ok:false code %s", env, tc.want)
			}
		})
	}
}

// TestCheckNoServerForUnknownTree: a tree no definition claims is
// exit 3 and says so, rather than reporting a clean bill of health for
// files nothing ever looked at.
func TestCheckNoServerForUnknownTree(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "notes.wat"), "nothing here\n")

	code, stdout, _ := runMain("check", dir)
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNoServer, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "no_server" {
		t.Errorf("error = %+v, want code no_server", env.Error)
	}
}

// TestCheckMaxFiles: the bound on how many documents one command opens
// announces itself rather than silently reporting on a subset.
func TestCheckMaxFiles(t *testing.T) {
	dir, main, helper := checkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		diagnostics:  map[string]any{main: []any{}, helper: []any{}},
	}.apply(t)

	code, stdout, stderr := runMain("check", dir, "--max-files", "1", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if !hasWarning(env.Warnings, "--max-files") {
		t.Errorf("warnings = %q, want one about the file limit", env.Warnings)
	}
}

// TestCheckTagsReachSARIF asserts the one lossy-looking conversion in
// the SARIF renderer: LSP diagnostic tags have no first-class home in
// SARIF, and they must not disappear.
func TestCheckTagsReachSARIF(t *testing.T) {
	_, main, _ := checkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		diagnostics: map[string]any{
			main: []any{map[string]any{
				"range": map[string]any{
					"start": map[string]any{"line": 2, "character": 4},
					"end":   map[string]any{"line": 2, "character": 6},
				},
				"severity": 2,
				"message":  "deprecated and unnecessary",
				"tags":     []int{1, 2},
			}},
		},
	}.apply(t)

	code, stdout, stderr := runMain("check", main, "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	got := decodeDiagnostics(t, stdout)
	if len(got.Diagnostics) != 1 {
		t.Fatalf("payload = %+v, want one diagnostic", got)
	}
	if want := []string{"unnecessary", "deprecated"}; fmt.Sprint(got.Diagnostics[0].Tags) != fmt.Sprint(want) {
		t.Errorf("tags = %v, want %v", got.Diagnostics[0].Tags, want)
	}

	scenario{
		capabilities: m5Capabilities(nil),
		diagnostics: map[string]any{
			main: []any{map[string]any{
				"range": map[string]any{
					"start": map[string]any{"line": 2, "character": 4},
					"end":   map[string]any{"line": 2, "character": 6},
				},
				"severity": 2,
				"message":  "deprecated and unnecessary",
				"tags":     []int{1, 2},
			}},
		},
	}.apply(t)
	code, stdout, _ = runMain("check", main, "--format", "sarif", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("sarif: exit code = %d, want %d", code, ExitOK)
	}
	var log struct {
		Runs []struct {
			Results []struct {
				Properties struct {
					Tags []string `json:"tags"`
				} `json:"properties"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &log); err != nil {
		t.Fatalf("sarif output is not JSON: %v (%s)", err, stdout)
	}
	if len(log.Runs) != 1 || len(log.Runs[0].Results) != 1 {
		t.Fatalf("sarif log = %+v, want one run with one result", log)
	}
	if got := log.Runs[0].Results[0].Properties.Tags; fmt.Sprint(got) != fmt.Sprint([]string{"unnecessary", "deprecated"}) {
		t.Errorf("sarif tags = %v, want [unnecessary deprecated]", got)
	}
}
