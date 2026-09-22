package symbols

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

func TestLeadingDoc(t *testing.T) {
	cases := []struct {
		name, src string
		decl      int // 0-based line of the declaration
		want      string
	}{
		{"go line comments", "package a\n\n// Run starts it. It never stops.\n// More words.\nfunc Run() {}\n", 4, "Run starts it."},
		{"first sentence over lines", "// Run starts the\n// server for good. Then more.\nfunc Run() {}\n", 2, "Run starts the server for good."},
		{"blank line separates", "// A stray note.\n\nfunc Run() {}\n", 2, ""},
		{"directive skipped", "// Run does it.\n//go:noinline\nfunc Run() {}\n", 2, "Run does it."},
		{"block comment", "/**\n * Run does it.\n * Really.\n */\nfunc Run() {}\n", 4, "Run does it."},
		{"rust doc and attribute", "/// Adds two numbers.\n#[inline]\nfn add() {}\n", 2, "Adds two numbers."},
		{"python hash comment and decorator", "# Handles it.\n@route('/')\ndef handle(): pass\n", 2, "Handles it."},
		{"lua dashes", "-- Loads the module.\nlocal function load() end\n", 1, "Loads the module."},
		{"no comment", "package a\n\nfunc Run() {}\n", 2, ""},
		{"preprocessor line is code", "#include <x.h>\nint f();\n", 1, ""},
		{"first line", "func Run() {}\n", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewLineIndex([]byte(tc.src)).LeadingDoc(tc.decl); got != tc.want {
				t.Errorf("LeadingDoc = %q, want %q", got, tc.want)
			}
		})
	}
	long := "// " + strings.Repeat("word ", 100) + "\nfunc Run() {}\n"
	if got := NewLineIndex([]byte(long)).LeadingDoc(1); len([]rune(got)) > maxDocRunes+1 || !strings.HasSuffix(got, "…") {
		t.Errorf("a long sentence is capped: %d runes", len([]rune(got)))
	}
}

func TestPythonDocstring(t *testing.T) {
	src := "def handle(x):\n    \"\"\"Handle one request.\n\n    More detail here.\n    \"\"\"\n    return x\n\ndef bare():\n    return 1\n\ndef one():\n    '''One liner.'''\n"
	x := NewLineIndex([]byte(src))
	if got := x.PythonDocstring(0, 5); got != "Handle one request." {
		t.Errorf("multi-line docstring = %q", got)
	}
	if got := x.PythonDocstring(7, 8); got != "" {
		t.Errorf("no docstring = %q", got)
	}
	if got := x.PythonDocstring(10, 11); got != "One liner." {
		t.Errorf("one-line docstring = %q", got)
	}
}

// Both shapes of documentSymbol give the same ids: that is what lets an id the
// index recorded from one server resolve in `source`.
func TestDecodeFileIDsDoNotDependOnTheShape(t *testing.T) {
	src := "package a\n\ntype S struct{}\n\nfunc (s *S) Run() {}\n"
	uri := protocol.DocumentURI("file:///w/a.go")
	m := protocol.NewMapper(uri, []byte(src))
	tree := `[{"name":"S","kind":23,"range":{"start":{"line":2,"character":0},"end":{"line":2,"character":15}},"selectionRange":{"start":{"line":2,"character":5},"end":{"line":2,"character":6}}},
	{"name":"(*S).Run","kind":6,"range":{"start":{"line":4,"character":0},"end":{"line":4,"character":20}},"selectionRange":{"start":{"line":4,"character":10},"end":{"line":4,"character":13}}}]`
	flat := `[{"name":"S","kind":23,"location":{"uri":"file:///w/a.go","range":{"start":{"line":2,"character":0},"end":{"line":2,"character":15}}}},
	{"name":"Run","kind":6,"containerName":"S","location":{"uri":"file:///w/a.go","range":{"start":{"line":4,"character":0},"end":{"line":4,"character":20}}}}]`
	var ids [2][]string
	for i, raw := range []string{tree, flat} {
		syms, _, err := DecodeFile(json.RawMessage(raw), uri, m)
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = IDs("a.go", syms)
	}
	want := []string{"a.go::S#struct", "a.go::S.Run#method"}
	for i := range ids {
		if strings.Join(ids[i], ",") != strings.Join(want, ",") {
			t.Errorf("shape %d ids = %v, want %v", i, ids[i], want)
		}
	}
}
