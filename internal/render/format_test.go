package render

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestParseFormat(t *testing.T) {
	for _, f := range Formats() {
		got, err := ParseFormat(string(f))
		if err != nil || got != f {
			t.Errorf("ParseFormat(%q) = %q, %v", f, got, err)
		}
	}
	for _, bad := range []string{"", "JSON", "yaml", "unified"} {
		_, err := ParseFormat(bad)
		if err == nil {
			t.Errorf("ParseFormat(%q) accepted an invalid format", bad)
		}
		if got := ExitCode(err); got != ExitUsage {
			t.Errorf("ParseFormat(%q): exit = %d, want %d", bad, got, ExitUsage)
		}
	}
}

// TestDefaultFormatIsJSONForMachines pins the PLAN §4 default: json
// whenever a machine is reading, which is the case that matters.
func TestDefaultFormatIsJSONForMachines(t *testing.T) {
	if got := DefaultFormat(&bytes.Buffer{}); got != FormatJSON {
		t.Errorf("buffer: got %q, want %q", got, FormatJSON)
	}

	pipeR, pipeW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pipeR.Close()
	defer pipeW.Close()
	if got := DefaultFormat(pipeW); got != FormatJSON {
		t.Errorf("pipe: got %q, want %q", got, FormatJSON)
	}

	file, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got := DefaultFormat(file); got != FormatJSON {
		t.Errorf("regular file: got %q, want %q", got, FormatJSON)
	}

	// A character device stands in for a terminal; /dev/null is one and
	// exists everywhere this runs.
	devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no %s: %v", os.DevNull, err)
	}
	defer devNull.Close()
	if got := DefaultFormat(devNull); got != FormatText {
		t.Errorf("character device: got %q, want %q", got, FormatText)
	}
}

func TestResolveFormat(t *testing.T) {
	got, err := ResolveFormat("", &bytes.Buffer{})
	if err != nil || got != FormatJSON {
		t.Errorf("empty flag: got %q, %v", got, err)
	}
	got, err = ResolveFormat("sarif", &bytes.Buffer{})
	if err != nil || got != FormatSARIF {
		t.Errorf("explicit flag: got %q, %v", got, err)
	}
	if _, err := ResolveFormat("nope", &bytes.Buffer{}); err == nil {
		t.Error("invalid flag accepted")
	}
}

func TestDisplayPath(t *testing.T) {
	root := filepath.FromSlash("/w")
	tests := []struct {
		root, path, want string
	}{
		{"", "/w/a/b.go", "/w/a/b.go"},
		{root, "/w/a/b.go", "a/b.go"},
		{root, "/w", "."},
		{root, "/elsewhere/b.go", "/elsewhere/b.go"},
		{root, "relative.go", "relative.go"},
		{root, "", ""},
	}
	for _, tt := range tests {
		o := Options{Root: tt.root}
		want := filepath.FromSlash(tt.want)
		if got := o.displayPath(filepath.FromSlash(tt.path)); got != want {
			t.Errorf("Options{Root:%q}.displayPath(%q) = %q, want %q", tt.root, tt.path, got, want)
		}
	}
}

func TestTruncate(t *testing.T) {
	items := []int{1, 2, 3}
	for _, tt := range []struct {
		limit int
		want  int
		cut   bool
	}{{0, 3, false}, {5, 3, false}, {3, 3, false}, {2, 2, true}, {1, 1, true}} {
		got, cut := truncate(items, tt.limit)
		if len(got) != tt.want || cut != tt.cut {
			t.Errorf("truncate(limit=%d) = %v, %v; want %d items, cut=%v", tt.limit, got, cut, tt.want, tt.cut)
		}
	}
}

func TestDiffContextDefault(t *testing.T) {
	if got := (Options{}).diffContext(); got != 3 {
		t.Errorf("default diff context = %d, want 3", got)
	}
	if got := (Options{DiffContext: DiffContextLines(7)}).diffContext(); got != 7 {
		t.Errorf("explicit diff context = %d, want 7", got)
	}
	if got := (Options{DiffContext: DiffContextLines(0)}).diffContext(); got != 0 {
		t.Errorf("explicit zero diff context = %d, want 0", got)
	}
}
