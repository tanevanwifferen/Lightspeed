package cli

import (
	"strings"
	"testing"
)

// The call-hierarchy fixture is the CJK file again: a call graph whose
// positions come back as byte columns is the same property references
// has, and the arrows must not disturb it.
//
// The coordinates below are the UTF-16 ones a server sends, with the
// byte columns they must be reported as:
//
//	関数    line 5, UTF-16 5-7   ->  5:6
//	変数    line 3, UTF-16 4-6   ->  3:5
//	call    line 6, UTF-16 8-10  ->  6:9

// prepared is the prepareCallHierarchy answer for 関数.
func prepared(file string) []any {
	return []any{callItemJSON("関数", 12, file, 4, 5, 7)}
}

// TestCallHierarchyBothDirections is the shape of the answer: one row
// per call, pointing at the other symbol's declaration, with an arrow
// saying which way the call goes and the call site in the detail.
func TestCallHierarchyBothDirections(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodPrepareCallHierarchy: prepared(file)},
		calls: map[string]any{
			"関数": map[string]any{
				"incoming": []any{incomingCall(callItemJSON("変数", 13, file, 2, 4, 6), callRange(5, 8, 10))},
				"outgoing": []any{outgoingCall(callItemJSON("callee", 12, file, 2, 4, 6), callRange(5, 13, 15))},
			},
		},
	}.apply(t)

	code, stdout, stderr := runMain("call_hierarchy", file+":5:6", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	got := decodeResults(t, stdout)
	if got.Kind != "call_hierarchy" || got.Count != 2 {
		t.Fatalf("payload = %+v, want two call_hierarchy results", got)
	}
	// Incoming first, then outgoing: the order is the traversal, not a
	// sort, because indentation only means something in traversal order.
	incoming, outgoing := got.Results[0], got.Results[1]
	if incoming.Label != "<- 変数" {
		t.Errorf("incoming label = %q, want %q", incoming.Label, "<- 変数")
	}
	if outgoing.Label != "-> callee" {
		t.Errorf("outgoing label = %q, want %q", outgoing.Label, "-> callee")
	}
	if incoming.Start.Line != 3 || incoming.Start.Column != 5 {
		t.Errorf("incoming at %d:%d, want 3:5", incoming.Start.Line, incoming.Start.Column)
	}
	if incoming.Kind != "variable" || outgoing.Kind != "function" {
		t.Errorf("kinds = %q/%q, want variable/function", incoming.Kind, outgoing.Kind)
	}
	// The call site is not lost, and it is a byte column too.
	if !strings.Contains(incoming.Detail, ":6:9") {
		t.Errorf("incoming detail = %q, want the call site at 6:9", incoming.Detail)
	}
	if !strings.Contains(outgoing.Detail, ":6:18") {
		t.Errorf("outgoing detail = %q, want the call site at 6:18", outgoing.Detail)
	}
}

// TestCallHierarchyDirectionFlag: each direction on its own, so that
// --direction is not merely accepted but obeyed.
func TestCallHierarchyDirectionFlag(t *testing.T) {
	_, file := cjkFixture(t)
	sc := scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodPrepareCallHierarchy: prepared(file)},
		calls: map[string]any{
			"関数": map[string]any{
				"incoming": []any{incomingCall(callItemJSON("caller", 12, file, 2, 4, 6))},
				"outgoing": []any{outgoingCall(callItemJSON("callee", 12, file, 2, 4, 6))},
			},
		},
	}
	for _, tc := range []struct {
		direction string
		want      string
	}{
		{directionIncoming, "<- caller"},
		{directionOutgoing, "-> callee"},
	} {
		t.Run(tc.direction, func(t *testing.T) {
			sc.apply(t)
			code, stdout, stderr := runMain("call_hierarchy", file+":5:6",
				"--direction", tc.direction, "--settle", "20ms")
			if code != ExitOK {
				t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
			}
			got := decodeResults(t, stdout)
			if got.Count != 1 {
				t.Fatalf("count = %d, want 1 (%s)", got.Count, stdout)
			}
			if got.Results[0].Label != tc.want {
				t.Errorf("label = %q, want %q", got.Results[0].Label, tc.want)
			}
		})
	}
}

// TestCallHierarchyDepth: --depth expands the answers, and the depth
// limit is the thing that stops it. The indentation of a row is its
// level, so a caller can read the tree out of the flat list.
func TestCallHierarchyDepth(t *testing.T) {
	_, file := cjkFixture(t)
	sc := scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodPrepareCallHierarchy: prepared(file)},
		calls: map[string]any{
			"関数": map[string]any{
				"incoming": []any{incomingCall(callItemJSON("middle", 12, file, 2, 4, 6))},
			},
			"middle": map[string]any{
				"incoming": []any{incomingCall(callItemJSON("outer", 12, file, 5, 8, 10))},
			},
		},
	}

	t.Run("depth 1 stops at the direct callers", func(t *testing.T) {
		sc.apply(t)
		code, stdout, stderr := runMain("call_hierarchy", file+":5:6",
			"--direction", directionIncoming, "--settle", "20ms")
		if code != ExitOK {
			t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
		}
		got := decodeResults(t, stdout)
		if got.Count != 1 || got.Results[0].Label != "<- middle" {
			t.Fatalf("payload = %+v, want only the direct caller", got)
		}
	})

	t.Run("depth 2 expands one more level", func(t *testing.T) {
		sc.apply(t)
		code, stdout, stderr := runMain("call_hierarchy", file+":5:6",
			"--direction", directionIncoming, "--depth", "2", "--settle", "20ms")
		if code != ExitOK {
			t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
		}
		got := decodeResults(t, stdout)
		if got.Count != 2 {
			t.Fatalf("count = %d, want 2 (%s)", got.Count, stdout)
		}
		if got.Results[0].Label != "<- middle" || got.Results[1].Label != "  <- outer" {
			t.Errorf("labels = %q, %q; want %q then %q (indented)",
				got.Results[0].Label, got.Results[1].Label, "<- middle", "  <- outer")
		}
		if got.Results[1].Start.Line != 6 || got.Results[1].Start.Column != 9 {
			t.Errorf("outer at %d:%d, want 6:9", got.Results[1].Start.Line, got.Results[1].Start.Column)
		}
	})
}

// TestCallHierarchyCycle: a recursive call graph must terminate, be
// reported once, and say that it was cut.
func TestCallHierarchyCycle(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodPrepareCallHierarchy: prepared(file)},
		calls: map[string]any{
			// 関数 is called by loop, and loop is called by 関数:
			// following this naively never returns.
			"関数": map[string]any{
				"incoming": []any{incomingCall(callItemJSON("loop", 12, file, 2, 4, 6))},
			},
			"loop": map[string]any{
				"incoming": []any{incomingCall(callItemJSON("関数", 12, file, 4, 5, 7))},
			},
		},
	}.apply(t)

	code, stdout, stderr := runMain("call_hierarchy", file+":5:6",
		"--direction", directionIncoming, "--depth", "5", "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s\nstdout: %s", code, ExitOK, stderr, stdout)
	}
	got := decodeResults(t, stdout)
	if got.Count != 2 {
		t.Fatalf("count = %d, want 2 (loop, then 関数 reported but not followed): %s", got.Count, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if !hasWarning(env.Warnings, "already shown") {
		t.Errorf("warnings = %q, want one about the branch that was not expanded", env.Warnings)
	}
}

// TestCallHierarchyAmbiguousPrepare: several callable symbols at one
// position means the server could not tell either. One hierarchy is
// still more useful than none, but the answer says which symbol it is
// about and names the others — an answer about the wrong symbol has to
// be recognisable as one.
func TestCallHierarchyAmbiguousPrepare(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results: map[string]any{
			methodPrepareCallHierarchy: []any{
				callItemJSON("関数", 12, file, 4, 5, 7),
				callItemJSON("shadowed", 12, file, 2, 4, 6),
			},
		},
		calls: map[string]any{
			"関数": map[string]any{
				"incoming": []any{incomingCall(callItemJSON("caller", 12, file, 2, 4, 6))},
			},
		},
	}.apply(t)

	code, stdout, stderr := runMain("call_hierarchy", file+":5:6",
		"--direction", directionIncoming, "--settle", "20ms")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d; stderr: %s", code, ExitOK, stderr)
	}
	env := decodeEnvelope(t, stdout)
	if !hasWarning(env.Warnings, "shadowed") {
		t.Errorf("warnings = %q, want one naming the symbols that were not used", env.Warnings)
	}
	if !hasWarning(env.Warnings, "関数") {
		t.Errorf("warnings = %q, want one naming the symbol the answer is about", env.Warnings)
	}
}

// TestCallHierarchyNoSymbol: a position that is not a callable symbol
// is exit 1 with a code, not an empty result set that looks like "this
// function has no callers".
func TestCallHierarchyNoSymbol(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodPrepareCallHierarchy: []any{}},
	}.apply(t)

	code, stdout, _ := runMain("call_hierarchy", file+":1:1", "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.OK || env.Error == nil || env.Error.Code != "not_found" {
		t.Errorf("envelope = %+v, want ok:false code not_found", env)
	}
}

// TestCallHierarchyNoCallers: a symbol nobody calls is an empty result
// set and exit 1 — grep's convention, the same as `references`.
func TestCallHierarchyNoCallers(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodPrepareCallHierarchy: prepared(file)},
		calls:        map[string]any{"関数": map[string]any{"incoming": []any{}}},
	}.apply(t)

	code, stdout, _ := runMain("call_hierarchy", file+":5:6",
		"--direction", directionIncoming, "--settle", "20ms")
	if code != ExitProblems {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitProblems, stdout)
	}
	if got := decodeResults(t, stdout); got.Count != 0 {
		t.Errorf("payload = %+v, want an empty result set", got)
	}
}

// TestCallHierarchyWithoutCapability: PLAN §5.4 again. A server that
// never advertised callHierarchyProvider is exit 3 with what it can do.
func TestCallHierarchyWithoutCapability(t *testing.T) {
	_, file := cjkFixture(t)
	scenario{capabilities: m5Capabilities(map[string]any{"callHierarchyProvider": nil})}.apply(t)

	code, stdout, _ := runMain("call_hierarchy", file+":5:6", "--settle", "20ms")
	if code != ExitNoServer {
		t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitNoServer, stdout)
	}
	env := decodeEnvelope(t, stdout)
	if env.Error == nil || env.Error.Code != "unsupported_method" {
		t.Errorf("error = %+v, want code unsupported_method", env.Error)
	}
}

// TestCallHierarchyUsage: the bounds are usage errors, not clamps. A
// --depth of 40 is a request nobody can afford, and silently serving 5
// instead would misreport what was searched.
func TestCallHierarchyUsage(t *testing.T) {
	_, file := cjkFixture(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"unknown direction", []string{"call_hierarchy", file + ":5:6", "--direction", "sideways"}},
		{"depth zero", []string{"call_hierarchy", file + ":5:6", "--depth", "0"}},
		{"depth too large", []string{"call_hierarchy", file + ":5:6", "--depth", "40"}},
		{"no location", []string{"call_hierarchy"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scenario{capabilities: m5Capabilities(nil)}.apply(t)
			code, stdout, _ := runMain(tc.args...)
			if code != ExitUsage {
				t.Fatalf("exit code = %d, want %d; stdout: %s", code, ExitUsage, stdout)
			}
			if env := decodeEnvelope(t, stdout); env.OK || env.Error == nil {
				t.Errorf("envelope = %+v, want a failure", env)
			}
		})
	}
}
