package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// diagnosticsFixture is the shape `lightspeed check` produces: several
// severities, a server that reports source and code, a server that
// reports neither, and a non-ASCII file.
func diagnosticsFixture(t *testing.T) DiagnosticSet {
	t.Helper()
	ascii := mapper("internal/store/user.go", asciiSource)
	cjk := mapper("internal/fixture/fixture.go", cjkSource)

	var ds DiagnosticSet
	add := func(m *protocol.Mapper, needle string, n int, sev Severity, msg, source, code string) {
		t.Helper()
		d, err := NewDiagnostic(m, rangeOf(t, m, needle, n), sev, msg)
		if err != nil {
			t.Fatalf("NewDiagnostic(%q): %v", needle, err)
		}
		d.Source, d.Code = source, code
		ds.Diagnostics = append(ds.Diagnostics, d)
	}
	add(ascii, "DB", 0, SeverityError, "undefined: DB", "compiler", "UndeclaredName")
	add(ascii, "User", 3, SeverityError, "undefined: User", "compiler", "UndeclaredName")
	add(ascii, "id int", 0, SeverityWarn, "parameter id is unused", "staticcheck", "ST1016")
	add(cjk, "ユーザー名", 1, SeverityHint, "ユーザー名 is never reassigned; consider a constant", "", "")
	add(cjk, "使う", 0, SeverityInfo, "exported function 使う should have a comment", "revive", "")
	ds.Sort()
	return ds
}

func TestDiagnosticsText(t *testing.T) {
	ds := diagnosticsFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatText, ds, Options{Root: fixtureRoot})
	})
	golden(t, "diagnostics_text.txt", got)

	lines := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	if len(lines) != len(ds.Diagnostics) {
		t.Fatalf("got %d lines for %d diagnostics", len(lines), len(ds.Diagnostics))
	}
	// `file:line:col: severity: message` is what editors and CI log
	// scrapers already parse.
	for i, line := range lines {
		want := " " + ds.Diagnostics[i].Severity.String() + ": "
		if !strings.Contains(line, want) {
			t.Errorf("line %d = %q, want it to contain %q", i, line, want)
		}
	}
}

func TestDiagnosticsJSON(t *testing.T) {
	ds := diagnosticsFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatJSON, ds, Options{Root: fixtureRoot, Indent: true})
	})
	golden(t, "diagnostics_json.json", got)

	var env struct {
		Data diagnosticsData `json:"data"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	if env.Data.Errors != 2 {
		t.Errorf("errors = %d, want 2", env.Data.Errors)
	}
	if env.Data.Count != 5 || env.Data.Truncated {
		t.Errorf("count = %d truncated = %v", env.Data.Count, env.Data.Truncated)
	}
}

func TestDiagnosticsSeverityRendersAsAName(t *testing.T) {
	// A number would make every consumer carry a lookup table.
	for sev, want := range map[Severity]string{
		SeverityError: "error", SeverityWarn: "warning",
		SeverityInfo: "info", SeverityHint: "hint",
		SeverityUnset: "warning",
	} {
		b, err := json.Marshal(sev)
		if err != nil {
			t.Fatal(err)
		}
		if string(b) != `"`+want+`"` {
			t.Errorf("Severity(%d) marshalled as %s, want %q", sev, b, want)
		}
	}
}

func TestDiagnosticsHasErrors(t *testing.T) {
	ds := diagnosticsFixture(t)
	if !ds.HasErrors() {
		t.Error("HasErrors() = false, want true")
	}
	warnOnly := DiagnosticSet{Diagnostics: []Diagnostic{{Severity: SeverityWarn}, {Severity: SeverityHint}}}
	if warnOnly.HasErrors() {
		t.Error("HasErrors() = true for a set with no errors")
	}
}

func TestDiagnosticsTruncation(t *testing.T) {
	ds := diagnosticsFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatText, ds, Options{Root: fixtureRoot, Limit: 2})
	})
	if !bytes.Contains(got, []byte("# diagnostics truncated: showing 2 of 5 diagnostics (--limit 2)")) {
		t.Errorf("truncation was not announced:\n%s", got)
	}
}

func TestDiagnosticsRejectsDiffFormat(t *testing.T) {
	err := Diagnostics(&bytes.Buffer{}, FormatDiff, diagnosticsFixture(t), Options{})
	if got := CodeForError(err); got != CodeUnsupportedFormat {
		t.Errorf("code = %q, want %q", got, CodeUnsupportedFormat)
	}
}

func TestDiagnosticOriginSuffix(t *testing.T) {
	for _, tt := range []struct {
		source, code, want string
	}{
		{"staticcheck", "SA1000", " [staticcheck:SA1000]"},
		{"compiler", "", " [compiler]"},
		{"", "E501", " [E501]"},
		{"", "", ""},
	} {
		d := Diagnostic{Source: tt.source, Code: tt.code}
		if got := d.originSuffix(); got != tt.want {
			t.Errorf("Diagnostic{%q,%q}.originSuffix() = %q, want %q", tt.source, tt.code, got, tt.want)
		}
	}
}

func TestDiagnosticRuleID(t *testing.T) {
	for _, tt := range []struct {
		source, code, want string
	}{
		{"staticcheck", "SA1000", "staticcheck/SA1000"},
		{"compiler", "", "compiler"},
		{"", "E501", "E501"},
		{"", "", "lightspeed/diagnostic"},
	} {
		d := Diagnostic{Source: tt.source, Code: tt.code}
		if got := d.ruleID(); got != tt.want {
			t.Errorf("Diagnostic{%q,%q}.ruleID() = %q, want %q", tt.source, tt.code, got, tt.want)
		}
	}
}
