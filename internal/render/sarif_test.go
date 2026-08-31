package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestDiagnosticsSARIF(t *testing.T) {
	ds := diagnosticsFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, ds, Options{
			Root:        fixtureRoot,
			ToolVersion: "0.0.0-test",
			Indent:      true,
		})
	})
	golden(t, "diagnostics_sarif.json", got)
	validateSARIF(t, got)
}

func TestDiagnosticsSARIFWithoutRoot(t *testing.T) {
	ds := diagnosticsFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, ds, Options{ToolVersion: "0.0.0-test", Indent: true})
	})
	golden(t, "diagnostics_sarif_absolute.json", got)
	validateSARIF(t, got)

	// With no root there is nothing for a uriBaseId to be relative to,
	// so results must carry absolute file:// URIs instead.
	log := parseSARIF(t, got)
	run := log["runs"].([]any)[0].(map[string]any)
	if _, declared := run["originalUriBaseIds"]; declared {
		t.Error("originalUriBaseIds declared without a root")
	}
	for _, uri := range artifactURIs(t, log) {
		if !strings.HasPrefix(uri, "file://") {
			t.Errorf("artifact uri %q is not absolute", uri)
		}
	}
}

// TestSARIFColumnsAreUTF16 is the CJK half of the format: SARIF regions
// carry the server's own UTF-16 columns, and the run says so, so a
// consumer never has to guess which encoding it is looking at.
func TestSARIFColumnsAreUTF16(t *testing.T) {
	cjk := mapper("internal/fixture/fixture.go", cjkSource)
	d, err := NewDiagnostic(cjk, rangeOf(t, cjk, "ユーザー名", 1), SeverityError, "boom")
	if err != nil {
		t.Fatal(err)
	}
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, DiagnosticSet{Diagnostics: []Diagnostic{d}}, Options{Root: fixtureRoot})
	})
	validateSARIF(t, got)

	log := parseSARIF(t, got)
	run := log["runs"].([]any)[0].(map[string]any)
	if got := run["columnKind"]; got != "utf16CodeUnits" {
		t.Errorf("columnKind = %v, want utf16CodeUnits", got)
	}
	region := firstRegion(t, log)
	// The byte column is 13 and the UTF-16 column is 8 (0-based), so a
	// SARIF startColumn of 9 proves nothing was silently re-encoded.
	if got := region["startColumn"]; got != float64(9) {
		t.Errorf("startColumn = %v, want 9 (UTF-16 column 8, 1-based)", got)
	}
	if got := d.Start.Column; got != 13 {
		t.Fatalf("fixture drifted: byte column = %d, want 13", got)
	}
	if got := region["endColumn"]; got != float64(14) {
		t.Errorf("endColumn = %v, want 14 (exclusive end of a 5-unit identifier at 9)", got)
	}
	if got := region["snippet"].(map[string]any)["text"]; got != d.Text {
		t.Errorf("snippet = %q, want the matched line %q", got, d.Text)
	}
}

func TestSARIFCarriesDiagnosticTags(t *testing.T) {
	m := mapper("internal/store/user.go", asciiSource)
	d, err := NewDiagnostic(m, rangeOf(t, m, "db *DB", 0), SeverityHint, "field db is never read")
	if err != nil {
		t.Fatal(err)
	}
	d.Source, d.Code = "staticcheck", "U1000"
	d.Tags = []string{"unnecessary"}

	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, DiagnosticSet{Diagnostics: []Diagnostic{d}},
			Options{Root: fixtureRoot})
	})
	validateSARIF(t, got)

	log := parseSARIF(t, got)
	result := log["runs"].([]any)[0].(map[string]any)["results"].([]any)[0].(map[string]any)
	props, ok := result["properties"].(map[string]any)
	if !ok {
		t.Fatalf("no result.properties: %s", got)
	}
	tags := props["tags"].([]any)
	if len(tags) != 1 || tags[0] != "unnecessary" {
		t.Errorf("tags = %v, want [unnecessary]", tags)
	}

	// A diagnostic without tags must not grow an empty property bag.
	d.Tags = nil
	got = mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, DiagnosticSet{Diagnostics: []Diagnostic{d}}, Options{})
	})
	if bytes.Contains(got, []byte("properties")) {
		t.Errorf("an untagged diagnostic emitted a property bag:\n%s", got)
	}
}

func TestSARIFTruncationIsAnnounced(t *testing.T) {
	ds := diagnosticsFixture(t)
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, ds, Options{Root: fixtureRoot, Limit: 2, Indent: true})
	})
	golden(t, "diagnostics_sarif_truncated.json", got)
	validateSARIF(t, got)

	log := parseSARIF(t, got)
	run := log["runs"].([]any)[0].(map[string]any)
	props, ok := run["properties"].(map[string]any)
	if !ok {
		t.Fatal("no run.properties on a truncated run")
	}
	if props["truncated"] != true || props["total"] != float64(5) || props["count"] != float64(2) {
		t.Errorf("properties = %v", props)
	}
	notes := run["invocations"].([]any)[0].(map[string]any)["toolExecutionNotifications"].([]any)
	if len(notes) != 1 {
		t.Fatalf("got %d notifications, want 1", len(notes))
	}
	text := notes[0].(map[string]any)["message"].(map[string]any)["text"].(string)
	if !strings.Contains(text, "showing 2 of 5") {
		t.Errorf("notification = %q", text)
	}
}

func TestSARIFCallerWarningsBecomeNotifications(t *testing.T) {
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, diagnosticsFixture(t), Options{
			Root:     fixtureRoot,
			Warnings: []string{"gopls reported diagnostics for 1 file that is not open"},
		})
	})
	validateSARIF(t, got)
	if !bytes.Contains(got, []byte("that is not open")) {
		t.Errorf("caller warning was dropped:\n%s", got)
	}
}

func TestSARIFRulesAreDeduplicated(t *testing.T) {
	// The fixture has two compiler/UndeclaredName diagnostics.
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, diagnosticsFixture(t), Options{Root: fixtureRoot})
	})
	log := parseSARIF(t, got)
	run := log["runs"].([]any)[0].(map[string]any)
	rules := run["tool"].(map[string]any)["driver"].(map[string]any)["rules"].([]any)
	seen := map[string]bool{}
	for _, r := range rules {
		id := r.(map[string]any)["id"].(string)
		if seen[id] {
			t.Errorf("rule %q declared twice", id)
		}
		seen[id] = true
	}
	if len(rules) != 4 {
		t.Errorf("got %d rules, want 4 distinct ones", len(rules))
	}
}

func TestSARIFIsNotWrappedInTheEnvelope(t *testing.T) {
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, diagnosticsFixture(t), Options{})
	})
	var probe map[string]any
	if err := json.Unmarshal(got, &probe); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "ok", "data"} {
		if key == "version" {
			// SARIF has its own version, and it is not the envelope's.
			if probe["version"] != sarifVersion {
				t.Errorf("version = %v, want %q", probe["version"], sarifVersion)
			}
			continue
		}
		if _, present := probe[key]; present {
			t.Errorf("SARIF output carries the envelope key %q", key)
		}
	}
}

func TestSARIFEmptyRunIsValid(t *testing.T) {
	got := mustRender(t, func(w *bytes.Buffer) error {
		return Diagnostics(w, FormatSARIF, DiagnosticSet{}, Options{})
	})
	validateSARIF(t, got)
	if !bytes.Contains(got, []byte(`"results":[]`)) {
		t.Errorf("empty run rendered as %s, want an empty results array", got)
	}
}

// -- SARIF 2.1.0 structural validation --

func parseSARIF(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var log map[string]any
	if err := json.Unmarshal(b, &log); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}
	return log
}

func firstRegion(t *testing.T, log map[string]any) map[string]any {
	t.Helper()
	run := log["runs"].([]any)[0].(map[string]any)
	result := run["results"].([]any)[0].(map[string]any)
	loc := result["locations"].([]any)[0].(map[string]any)
	return loc["physicalLocation"].(map[string]any)["region"].(map[string]any)
}

func artifactURIs(t *testing.T, log map[string]any) []string {
	t.Helper()
	var out []string
	for _, r := range log["runs"].([]any) {
		for _, res := range r.(map[string]any)["results"].([]any) {
			for _, loc := range res.(map[string]any)["locations"].([]any) {
				phys := loc.(map[string]any)["physicalLocation"].(map[string]any)
				out = append(out, phys["artifactLocation"].(map[string]any)["uri"].(string))
			}
		}
	}
	return out
}

// validateSARIF checks the output against the parts of the SARIF 2.1.0
// specification that can be checked structurally: required properties,
// enumerated values, and the internal consistency of rule indices,
// regions and uriBaseId references.
//
// It deliberately does not fetch the published JSON schema — tests here
// are hermetic — so it encodes the constraints instead.
func validateSARIF(t *testing.T, b []byte) {
	t.Helper()
	log := parseSARIF(t, b)

	if got := log["$schema"]; got != sarifSchema {
		t.Errorf("$schema = %v, want %q", got, sarifSchema)
	}
	if got := log["version"]; got != "2.1.0" {
		t.Errorf("version = %v, want 2.1.0", got)
	}
	runs, ok := log["runs"].([]any)
	if !ok || len(runs) == 0 {
		t.Fatalf("runs = %v, want a non-empty array", log["runs"])
	}

	levels := map[string]bool{"none": true, "note": true, "warning": true, "error": true}
	columnKinds := map[string]bool{"utf16CodeUnits": true, "unicodeCodePoints": true}

	for i, raw := range runs {
		run, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("runs[%d] is not an object", i)
		}
		where := fmt.Sprintf("runs[%d]", i)

		driver, ok := run["tool"].(map[string]any)["driver"].(map[string]any)
		if !ok {
			t.Fatalf("%s.tool.driver is missing", where)
		}
		if name, _ := driver["name"].(string); name == "" {
			t.Errorf("%s.tool.driver.name is required", where)
		}
		if kind, present := run["columnKind"]; present && !columnKinds[kind.(string)] {
			t.Errorf("%s.columnKind = %v, not a SARIF column kind", where, kind)
		}

		var ruleIDs []string
		if rules, present := driver["rules"].([]any); present {
			for j, raw := range rules {
				rule := raw.(map[string]any)
				id, _ := rule["id"].(string)
				if id == "" {
					t.Errorf("%s.tool.driver.rules[%d].id is required", where, j)
				}
				ruleIDs = append(ruleIDs, id)
			}
		}

		baseIDs := map[string]bool{}
		if bases, present := run["originalUriBaseIds"].(map[string]any); present {
			for id, raw := range bases {
				baseIDs[id] = true
				uri, _ := raw.(map[string]any)["uri"].(string)
				if !strings.HasSuffix(uri, "/") {
					t.Errorf("%s.originalUriBaseIds[%q].uri = %q, must end in a slash", where, id, uri)
				}
			}
		}

		if invocations, present := run["invocations"].([]any); present {
			for j, raw := range invocations {
				inv := raw.(map[string]any)
				if _, ok := inv["executionSuccessful"].(bool); !ok {
					t.Errorf("%s.invocations[%d].executionSuccessful is required", where, j)
				}
				for k, raw := range notifications(inv) {
					n := raw.(map[string]any)
					if level, _ := n["level"].(string); !levels[level] {
						t.Errorf("%s.invocations[%d].toolExecutionNotifications[%d].level = %q",
							where, j, k, level)
					}
					if _, ok := n["message"].(map[string]any)["text"].(string); !ok {
						t.Errorf("%s.invocations[%d].toolExecutionNotifications[%d].message.text is required",
							where, j, k)
					}
				}
			}
		}

		results, ok := run["results"].([]any)
		if !ok {
			t.Fatalf("%s.results is missing (a run with no results must still carry [])", where)
		}
		for j, raw := range results {
			result := raw.(map[string]any)
			where := fmt.Sprintf("%s.results[%d]", where, j)

			if level, _ := result["level"].(string); !levels[level] {
				t.Errorf("%s.level = %q, not a SARIF level", where, level)
			}
			if text, _ := result["message"].(map[string]any)["text"].(string); text == "" {
				t.Errorf("%s.message.text is required", where)
			}
			id, _ := result["ruleId"].(string)
			if idx, present := result["ruleIndex"].(float64); present {
				if int(idx) < 0 || int(idx) >= len(ruleIDs) {
					t.Errorf("%s.ruleIndex = %v, out of range for %d rules", where, idx, len(ruleIDs))
				} else if ruleIDs[int(idx)] != id {
					t.Errorf("%s.ruleIndex %v points at rule %q, but ruleId is %q",
						where, idx, ruleIDs[int(idx)], id)
				}
			}

			locations, ok := result["locations"].([]any)
			if !ok || len(locations) == 0 {
				t.Fatalf("%s.locations is missing", where)
			}
			for k, raw := range locations {
				phys := raw.(map[string]any)["physicalLocation"].(map[string]any)
				where := fmt.Sprintf("%s.locations[%d].physicalLocation", where, k)

				art := phys["artifactLocation"].(map[string]any)
				uri, _ := art["uri"].(string)
				if uri == "" {
					t.Errorf("%s.artifactLocation.uri is required", where)
				}
				if base, present := art["uriBaseId"].(string); present {
					if !baseIDs[base] {
						t.Errorf("%s.artifactLocation.uriBaseId = %q, not declared in originalUriBaseIds",
							where, base)
					}
					if strings.HasPrefix(uri, "/") || strings.Contains(uri, "://") {
						t.Errorf("%s.artifactLocation.uri = %q must be relative when uriBaseId is set",
							where, uri)
					}
				}

				region, present := phys["region"].(map[string]any)
				if !present {
					continue
				}
				startLine := region["startLine"].(float64)
				startCol := region["startColumn"].(float64)
				endLine := region["endLine"].(float64)
				endCol := region["endColumn"].(float64)
				if startLine < 1 || startCol < 1 {
					t.Errorf("%s.region start = %v:%v, SARIF lines and columns are 1-based",
						where, startLine, startCol)
				}
				if endLine < startLine {
					t.Errorf("%s.region endLine %v precedes startLine %v", where, endLine, startLine)
				}
				if endLine == startLine && endCol < startCol {
					t.Errorf("%s.region endColumn %v precedes startColumn %v", where, endCol, startCol)
				}
			}
		}
	}
}

func notifications(inv map[string]any) []any {
	n, _ := inv["toolExecutionNotifications"].([]any)
	return n
}
