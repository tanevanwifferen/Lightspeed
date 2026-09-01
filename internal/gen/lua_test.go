package gen

import (
	"strings"
	"testing"
)

func TestReturnedTableLiterals(t *testing.T) {
	src := `
-- a comment
--[[ a long
     comment ]]
local x = 1
return {
  cmd = { 'server', '--stdio' },
  filetypes = { "a", 'b' },
  nested = { deep = { flag = true, off = false, none = nil } },
  count = 3,
  ratio = 1.5,
  ['quoted-key'] = 'v',
  positional,
  [[long string]],
}
`
	tbl, err := ReturnedTable([]byte(src))
	if err != nil {
		t.Fatalf("ReturnedTable() = %v", err)
	}
	cmd, ok := tbl.Get("cmd")
	if !ok {
		t.Fatal("cmd is missing")
	}
	got, err := cmd.(*Table).Strings()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, " ") != "server --stdio" {
		t.Errorf("cmd = %v", got)
	}

	nested, _ := tbl.Get("nested")
	table, dropped := nested.(*Table).Map()
	if len(dropped) != 0 {
		t.Errorf("dropped = %v, want none", dropped)
	}
	deep := table["deep"].(map[string]any)
	if deep["flag"] != true || deep["off"] != false {
		t.Errorf("deep = %#v", deep)
	}
	if v, ok := deep["none"]; !ok || v != nil {
		t.Errorf("deep.none = %#v, want a present nil", v)
	}

	if v, _ := tbl.Get("count"); v != int64(3) {
		t.Errorf("count = %#v, want int64(3)", v)
	}
	if v, _ := tbl.Get("ratio"); v != 1.5 {
		t.Errorf("ratio = %#v, want 1.5", v)
	}
	if v, _ := tbl.Get("quoted-key"); v != "v" {
		t.Errorf("quoted-key = %#v", v)
	}
	if got, want := len(tbl.List()), 2; got != want {
		t.Errorf("%d positional entries, want %d", got, want)
	}
}

// TestReturnedTableSkipsCode is the contract that keeps this a reader
// and not an interpreter: code is recorded, never run, never guessed at.
func TestReturnedTableSkipsCode(t *testing.T) {
	src := `
return {
  cmd = { 'x' },
  root_dir = function(bufnr, on_dir)
    if bufnr then
      for i = 1, 3 do
        while true do
          do return end
        end
      end
    end
    repeat until true
    on_dir(vim.fs.root(bufnr, { 'go.mod' }))
  end,
  markers = vim.fn.has('nvim-0.11.3') == 1 and { 'a' } or { 'b' },
  concat = 'a' .. 'b',
  call = vim.env.HOME,
  after = { 'still', 'parsed' },
}
`
	tbl, err := ReturnedTable([]byte(src))
	if err != nil {
		t.Fatalf("ReturnedTable() = %v", err)
	}
	for _, key := range []string{"root_dir", "markers", "concat", "call"} {
		v, ok := tbl.Get(key)
		if !ok {
			t.Errorf("%s is missing", key)
			continue
		}
		u, ok := v.(Unsupported)
		if !ok {
			t.Errorf("%s = %#v, want Unsupported", key, v)
			continue
		}
		if u.Reason == "" {
			t.Errorf("%s has no reason", key)
		}
		if !strings.Contains(u.String(), "unsupported") {
			t.Errorf("%s String() = %q", key, u.String())
		}
	}
	if u, _ := tbl.Get("root_dir"); !strings.Contains(u.(Unsupported).Reason, "Lua function") {
		t.Errorf("root_dir reason = %q, want it to say it is a function", u.(Unsupported).Reason)
	}
	// Parsing continued past the skipped entries.
	after, ok := tbl.Get("after")
	if !ok {
		t.Fatal("the entry after the skipped code was not parsed")
	}
	if got, _ := after.(*Table).Strings(); strings.Join(got, " ") != "still parsed" {
		t.Errorf("after = %v", got)
	}
}

func TestTableStringsFlattensGroups(t *testing.T) {
	// The shape nvim-lspconfig ≥ 0.11.3 uses for equal-priority marker
	// groups.
	tbl, err := ReturnedTable([]byte("return { root_markers = { { 'a', 'b' }, { 'c' }, { '.git' } } }"))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := tbl.Get("root_markers")
	got, err := v.(*Table).Strings()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "a,b,c,.git" {
		t.Errorf("Strings() = %v, want the groups flattened in order", got)
	}
}

func TestTableStringsRejectsMixedLists(t *testing.T) {
	tbl, err := ReturnedTable([]byte("return { cmd = { 'x', 3 } }"))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := tbl.Get("cmd")
	if _, err := v.(*Table).Strings(); err == nil {
		t.Error("Strings() accepted a non-string entry; a half-imported list is worse than none")
	}
}

func TestMapDropsUnsupported(t *testing.T) {
	tbl, err := ReturnedTable([]byte("return { settings = { good = 1, bad = f(), sub = { worse = g() } } }"))
	if err != nil {
		t.Fatal(err)
	}
	v, _ := tbl.Get("settings")
	table, dropped := v.(*Table).Map()
	if _, ok := table["good"]; !ok {
		t.Error("the literal entry was dropped")
	}
	if _, ok := table["bad"]; ok {
		t.Error("an unsupported entry was imported")
	}
	if len(dropped) != 2 {
		t.Errorf("dropped = %v, want bad and sub.worse", dropped)
	}
}

func TestReturnedTableErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"no return", "local x = { a = 1 }\n", "does not return a table"},
		{"returns a value", "return M\n", "does not return a table"},
		{"code after the table", "return { a = 1 }\nlocal y = 2\n", "followed by more code"},
		{"unterminated table", "return { a = 1,\n", "unterminated table"},
		{"unterminated string", "return { a = 'x\n }\n", "unterminated string"},
		{"unterminated long comment", "--[[ oops\nreturn { a = 1 }\n", "unterminated long comment"},
		{"bad escape", `return { a = '\q' }`, "unsupported string escape"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tbl, err := ReturnedTable([]byte(tt.src))
			if err == nil {
				t.Fatalf("ReturnedTable() = %+v, want an error mentioning %q", tbl, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestMalformedLuaDegradesRatherThanLies: input that is not valid Lua
// at all is not this reader's problem to diagnose, but it must never
// turn into a plausible-looking literal.
func TestMalformedLuaDegradesRatherThanLies(t *testing.T) {
	tbl, err := ReturnedTable([]byte("return { a = 1 b = 2 }\n"))
	if err != nil {
		return // an error is a fine outcome too
	}
	if v, _ := tbl.Get("a"); !isUnsupported(v) {
		t.Errorf("a = %#v, want Unsupported rather than a value read out of malformed source", v)
	}
}

func TestStringEscapes(t *testing.T) {
	tbl, err := ReturnedTable([]byte(`return { a = "tab\there", b = 'quote\'s', c = "back\\slash", d = "nl\n" }`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"a": "tab\there", "b": "quote's", "c": `back\slash`, "d": "nl\n"}
	for key, value := range want {
		if got, _ := tbl.Get(key); got != value {
			t.Errorf("%s = %q, want %q", key, got, value)
		}
	}
}

func TestNumberLexing(t *testing.T) {
	// `1-2` is three tokens, so it is a compound expression, not a
	// malformed number; `1e-5` is one.
	tbl, err := ReturnedTable([]byte("return { a = 1-2, b = 1e-5, c = 0xff }"))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := tbl.Get("a"); !isUnsupported(v) {
		t.Errorf("a = %#v, want Unsupported", v)
	}
	if v, _ := tbl.Get("b"); v != 1e-5 {
		t.Errorf("b = %#v, want 1e-5", v)
	}
	if v, _ := tbl.Get("c"); v != int64(255) {
		t.Errorf("c = %#v, want 255", v)
	}
}

func isUnsupported(v Value) bool {
	_, ok := v.(Unsupported)
	return ok
}

func TestNilTableAccessors(t *testing.T) {
	var tbl *Table
	if _, ok := tbl.Get("x"); ok {
		t.Error("Get on a nil table found something")
	}
	if tbl.Keys() != nil || tbl.List() != nil {
		t.Error("Keys/List on a nil table are not nil")
	}
}
