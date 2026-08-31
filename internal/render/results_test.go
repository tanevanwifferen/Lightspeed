package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// referencesFixture is a result set spanning an ASCII and a non-ASCII
// file, in the shape `lightspeed references` produces.
func referencesFixture(t *testing.T) ResultSet {
	t.Helper()
	ascii := mapper("internal/store/user.go", asciiSource)
	cjk := mapper("internal/fixture/fixture.go", cjkSource)

	rs := ResultSet{Kind: "references"}
	add := func(m *protocol.Mapper, needle string, n int, kind string) {
		t.Helper()
		r, err := NewResult(m, rangeOf(t, m, needle, n))
		if err != nil {
			t.Fatalf("NewResult(%q): %v", needle, err)
		}
		r.Kind = kind
		rs.Results = append(rs.Results, r)
	}
	add(ascii, "UserRepo", 1, "declaration")
	add(ascii, "UserRepo", 2, "use")
	add(cjk, "ユーザー名", 1, "declaration")
	add(cjk, "ユーザー名", 2, "use")
	add(cjk, "名前", 0, "declaration")
	rs.Sort()
	return rs
}

func TestResultsText(t *testing.T) {
	rs := referencesFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatText, rs, Options{Root: fixtureRoot})
	})
	golden(t, "references_text.txt", got)

	// The contract is one result per line, `file:line:col: text`.
	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != len(rs.Results) {
		t.Fatalf("got %d lines for %d results", len(lines), len(rs.Results))
	}
	for i, line := range lines {
		want := Options{Root: fixtureRoot}.displayPath(rs.Results[i].Path)
		prefix := fmt.Sprintf("%s:%d:%d: ", want, rs.Results[i].Start.Line, rs.Results[i].Start.Column)
		if !strings.HasPrefix(line, prefix) {
			t.Errorf("line %d = %q, want it to start with %q", i, line, prefix)
		}
	}
}

func TestResultsTextWithContext(t *testing.T) {
	rs := referencesFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatText, rs, Options{Root: fixtureRoot, Context: 2})
	})
	golden(t, "references_context_text.txt", got)

	// grep's convention: a match is `path:line:col: text`, a context
	// line is `path-line-text`, and groups are separated by `--`.
	matches, contexts, separators := 0, 0, 0
	for _, line := range strings.Split(strings.TrimSuffix(string(got), "\n"), "\n") {
		switch {
		case line == "--":
			separators++
		case strings.Contains(line, ".go:"):
			matches++
		case strings.Contains(line, ".go-"):
			contexts++
		default:
			t.Errorf("line %q is neither a match, a context line nor a separator", line)
		}
	}
	if matches != len(rs.Results) {
		t.Errorf("found %d match lines, want %d", matches, len(rs.Results))
	}
	if separators != len(rs.Results)-1 {
		t.Errorf("found %d group separators, want %d", separators, len(rs.Results)-1)
	}
	if contexts == 0 {
		t.Error("--context 2 produced no context lines")
	}
}

func TestResultsJSON(t *testing.T) {
	rs := referencesFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatJSON, rs, Options{Root: fixtureRoot, Indent: true})
	})
	golden(t, "references_json.json", got)

	var env struct {
		Version  int  `json:"version"`
		OK       bool `json:"ok"`
		Warnings []string
		Data     resultsData
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if env.Version != EnvelopeVersion || !env.OK {
		t.Errorf("envelope = version %d ok %v", env.Version, env.OK)
	}
	if env.Data.Truncated {
		t.Error("truncated set on a complete result set")
	}
	if env.Data.Count != len(rs.Results) || env.Data.Total != len(rs.Results) {
		t.Errorf("count %d total %d, want %d", env.Data.Count, env.Data.Total, len(rs.Results))
	}
	if len(env.Warnings) != 0 {
		t.Errorf("unexpected warnings %v", env.Warnings)
	}
}

func TestResultsJSONIsOneLineByDefault(t *testing.T) {
	rs := referencesFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatJSON, rs, Options{Root: fixtureRoot})
	})
	if n := bytes.Count(got, []byte("\n")); n != 1 {
		t.Errorf("compact JSON has %d newlines, want 1 (JSON-lines)", n)
	}
}

func TestResultsJSONWithContext(t *testing.T) {
	rs := referencesFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatJSON, rs, Options{Root: fixtureRoot, Context: 1, Indent: true})
	})
	golden(t, "references_context_json.json", got)
}

// TestResultsTruncation is the token-discipline requirement of PLAN §4:
// --limit N must announce itself, in every format.
func TestResultsTruncation(t *testing.T) {
	rs := referencesFixture(t)

	jsonOut := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatJSON, rs, Options{Root: fixtureRoot, Limit: 2, Indent: true})
	})
	golden(t, "references_truncated_json.json", jsonOut)

	var env struct {
		Warnings []string    `json:"warnings"`
		Data     resultsData `json:"data"`
	}
	if err := json.Unmarshal(jsonOut, &env); err != nil {
		t.Fatal(err)
	}
	if !env.Data.Truncated {
		t.Error("truncated is not set")
	}
	if env.Data.Count != 2 || env.Data.Total != 5 || env.Data.Limit != 2 {
		t.Errorf("count %d total %d limit %d, want 2/5/2", env.Data.Count, env.Data.Total, env.Data.Limit)
	}
	if len(env.Warnings) != 1 || !strings.Contains(env.Warnings[0], "showing 2 of 5") {
		t.Errorf("warnings = %v", env.Warnings)
	}

	textOut := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatText, rs, Options{Root: fixtureRoot, Limit: 2})
	})
	golden(t, "references_truncated_text.txt", textOut)
	if !bytes.Contains(textOut, []byte("# references truncated: showing 2 of 5")) {
		t.Errorf("text output does not announce truncation:\n%s", textOut)
	}
	// The notice must not be mistakable for a result.
	for _, line := range strings.Split(strings.TrimSuffix(string(textOut), "\n"), "\n") {
		if strings.HasPrefix(line, "#") {
			continue
		}
		if strings.Count(line, ":") < 2 {
			t.Errorf("non-notice line %q is not file:line:col: text", line)
		}
	}
}

func TestResultsTruncationHonoursUpstreamFlag(t *testing.T) {
	rs := referencesFixture(t)
	rs.Total = 40
	rs.Truncated = true
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatJSON, rs, Options{Root: fixtureRoot})
	})
	var env struct {
		Data resultsData `json:"data"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatal(err)
	}
	if !env.Data.Truncated || env.Data.Total != 40 {
		t.Errorf("upstream truncation lost: truncated=%v total=%d", env.Data.Truncated, env.Data.Total)
	}
}

func TestResultsLabelOverridesMatchedLine(t *testing.T) {
	m := mapper("a.go", asciiSource)
	r, err := NewResult(m, rangeOf(t, m, "UserRepo", 0))
	if err != nil {
		t.Fatal(err)
	}
	r.Label = "type UserRepo struct{…}\n\nstores users"
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatText, ResultSet{Results: []Result{r}}, Options{Root: fixtureRoot})
	})
	if n := bytes.Count(got, []byte("\n")); n != 1 {
		t.Errorf("a multi-line label produced %d lines, want 1:\n%s", n, got)
	}
	if !bytes.Contains(got, []byte(`type UserRepo struct{…}\n\nstores users`)) {
		t.Errorf("label newlines were not escaped:\n%s", got)
	}
}

func TestResultsUnsupportedFormats(t *testing.T) {
	rs := referencesFixture(t)
	for _, f := range []Format{FormatDiff, FormatSARIF} {
		err := Results(&bytes.Buffer{}, f, rs, Options{})
		if err == nil {
			t.Fatalf("format %q was accepted for results", f)
		}
		if got := CodeForError(err); got != CodeUnsupportedFormat {
			t.Errorf("format %q: code = %q, want %q", f, got, CodeUnsupportedFormat)
		}
		if got := ExitCode(err); got != ExitUsage {
			t.Errorf("format %q: exit = %d, want %d", f, got, ExitUsage)
		}
	}
}

func TestResultsRejectsUnknownFormat(t *testing.T) {
	err := Results(&bytes.Buffer{}, Format("yaml"), ResultSet{}, Options{})
	if got := CodeForError(err); got != CodeInvalidFormat {
		t.Errorf("code = %q, want %q", got, CodeInvalidFormat)
	}
}

func TestResultsRejectsNegativeOptions(t *testing.T) {
	for _, o := range []Options{{Context: -1}, {Limit: -1}, {DiffContext: DiffContextLines(-1)}} {
		err := Results(&bytes.Buffer{}, FormatText, ResultSet{}, o)
		if got := CodeForError(err); got != CodeUsage {
			t.Errorf("%+v: code = %q, want %q", o, got, CodeUsage)
		}
	}
}

func TestResultsEmptySetRendersEmptyArray(t *testing.T) {
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatJSON, ResultSet{Kind: "references"}, Options{})
	})
	if !bytes.Contains(got, []byte(`"results":[]`)) {
		t.Errorf("empty result set rendered as %s, want an empty array not null", got)
	}
	textOut := mustRender(t, func(w *bytes.Buffer) error {
		return Results(w, FormatText, ResultSet{Kind: "references"}, Options{})
	})
	if len(textOut) != 0 {
		t.Errorf("empty result set rendered %q in text, want nothing", textOut)
	}
}
