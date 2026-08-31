package serverdef

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseTOMLValues(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want map[string]any
	}{{
		name: "empty",
		src:  "",
		want: map[string]any{},
	}, {
		name: "comments and blank lines only",
		src:  "# a comment\n\n   # indented\r\n",
		want: map[string]any{},
	}, {
		name: "scalars",
		src: `s = "text"
lit = 'no \escapes here'
i = 42
neg = -7
plus = +7
under = 1_000_000
hex = 0xff
oct = 0o755
bin = 0b1011
f = 3.5
exp = 1e3
yes = true
no = false
`,
		want: map[string]any{
			"s":     "text",
			"lit":   `no \escapes here`,
			"i":     int64(42),
			"neg":   int64(-7),
			"plus":  int64(7),
			"under": int64(1000000),
			"hex":   int64(255),
			"oct":   int64(493),
			"bin":   int64(11),
			"f":     3.5,
			"exp":   float64(1000),
			"yes":   true,
			"no":    false,
		},
	}, {
		name: "string escapes",
		src:  `s = "tab:\t nl:\n quote:\" back:\\ u:\u00e9 U:\U0001F600"`,
		want: map[string]any{"s": "tab:\t nl:\n quote:\" back:\\ u:é U:\U0001F600"},
	}, {
		name: "empty strings",
		src:  "a = \"\"\nb = ''\n",
		want: map[string]any{"a": "", "b": ""},
	}, {
		name: "trailing comment after value",
		src:  "a = 1 # why not\nb = \"# not a comment\"\n",
		want: map[string]any{"a": int64(1), "b": "# not a comment"},
	}, {
		name: "arrays",
		src: `empty = []
one = ["a"]
multi = [
  "a",   # first
  "b",
]
nested = [["a"], []]
mixed = [1, "two", true]
`,
		want: map[string]any{
			"empty":  []any{},
			"one":    []any{"a"},
			"multi":  []any{"a", "b"},
			"nested": []any{[]any{"a"}, []any{}},
			"mixed":  []any{int64(1), "two", true},
		},
	}, {
		name: "tables and dotted keys",
		src: `top = 1
[a]
x = 1
[a.b]
y = 2
[c]
d.e.f = 3
`,
		want: map[string]any{
			"top": int64(1),
			"a": map[string]any{
				"x": int64(1),
				"b": map[string]any{"y": int64(2)},
			},
			"c": map[string]any{
				"d": map[string]any{"e": map[string]any{"f": int64(3)}},
			},
		},
	}, {
		name: "quoted and spaced keys",
		src: `"quoted key" = 1
'literal key' = 2
spaced . key = 3
[ "t" . 'u' ]
v = 4
`,
		want: map[string]any{
			"quoted key":  int64(1),
			"literal key": int64(2),
			"spaced":      map[string]any{"key": int64(3)},
			"t":           map[string]any{"u": map[string]any{"v": int64(4)}},
		},
	}, {
		name: "inline tables",
		src: `empty = {}
flat = { a = 1, b = "two" }
nested = { a = { b = { c = true } } }
dotted = { a.b = 1 }
overlong = {
  a = 1,
  b = 2,
}
`,
		want: map[string]any{
			"empty": map[string]any{},
			"flat":  map[string]any{"a": int64(1), "b": "two"},
			"nested": map[string]any{
				"a": map[string]any{"b": map[string]any{"c": true}},
			},
			"dotted":   map[string]any{"a": map[string]any{"b": int64(1)}},
			"overlong": map[string]any{"a": int64(1), "b": int64(2)},
		},
	}, {
		name: "crlf line endings",
		src:  "a = 1\r\n[t]\r\nb = 2\r\n",
		want: map[string]any{"a": int64(1), "t": map[string]any{"b": int64(2)}},
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTOML([]byte(tt.src))
			if err != nil {
				t.Fatalf("parseTOML() = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("parseTOML() =\n\t%#v\nwant\n\t%#v", got, tt.want)
			}
		})
	}
}

func TestParseTOMLSyntaxErrors(t *testing.T) {
	tests := []struct{ name, src, want string }{
		{"bare bracket", "[", "expected a key"},
		{"unterminated literal", "a = 'x\n", "line 1: unterminated literal string"},
		{"multi-line literal", "a = '''x'''\n", "multi-line strings are not supported"},
		{"unknown escape", `a = "\q"`, `unknown escape sequence \q`},
		{"truncated unicode escape", `a = "\u00"`, "unicode escape"},
		{"surrogate escape", `a = "\ud800"`, "not a valid code point"},
		{"no value", "a =\n", "expected a value, found end of line"},
		{"eof after equals", "a =", "expected a value, found end of input"},
		{"unclosed inline table", "a = { b = 1\n", "expected ',' or '}' in inline table"},
		{"unmatched brace value", "a = }\n", "expected a value, found '}'"},
		{"local date", "a = 1979-05-27\n", "date and time values are not supported"},
		{"local time", "a = 07:32:00\n", "date and time values are not supported"},
		{"bad number", "a = 1.2.3\n", `invalid value "1.2.3"`},
		{"line number after newlines", "\n\n\na = @\n", `line 4: invalid value "@"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseTOML([]byte(tt.src))
			if err == nil {
				t.Fatalf("parseTOML() = %#v, want an error mentioning %q", got, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("parseTOML() error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

// TestParseTOMLLineNumbers checks that a multi-line array does not
// desynchronise the line counter, since a wrong line number in a
// config error is worse than none.
func TestParseTOMLLineNumbers(t *testing.T) {
	src := "a = [\n 1,\n 2,\n]\nb = @\n"
	_, err := parseTOML([]byte(src))
	if err == nil {
		t.Fatal("parseTOML() = nil error, want a failure on line 5")
	}
	if want := "line 5:"; !strings.Contains(err.Error(), want) {
		t.Errorf("parseTOML() error = %q, want it to mention %q", err, want)
	}
}
