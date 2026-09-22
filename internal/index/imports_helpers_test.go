package index

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// impWS is an in-memory Workspace.
type impWS struct {
	files map[string]string
}

func newImpWS(files map[string]string) *impWS { return &impWS{files: files} }

func (w *impWS) Has(p string) bool { _, ok := w.files[p]; return ok }
func (w *impWS) Files() []string {
	out := make([]string, 0, len(w.files))
	for f := range w.files {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
func (w *impWS) Read(p string) ([]byte, error) {
	s, ok := w.files[p]
	if !ok {
		return nil, fmt.Errorf("no %s", p)
	}
	return []byte(s), nil
}

// impSpecs renders refs as "kind:spec" (or just spec when kind is empty or
// the default), for compact table expectations.
func impSpecs(refs []ImportRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Spec)
	}
	return out
}

func impKinds(refs []ImportRef) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, r.Kind+":"+r.Spec)
	}
	return out
}

func impEqual(t *testing.T, name string, got, want []string) {
	t.Helper()
	if len(got) == 0 && len(want) == 0 {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s:\n got  %q\n want %q", name, got, want)
	}
}

// impExtract runs the extractor for a language.
func impExtract(t *testing.T, lang, path, src string) []ImportRef {
	t.Helper()
	refs, covered := ExtractImports(lang, path, []byte(src))
	if !covered {
		t.Fatalf("no extractor for %s", lang)
	}
	return refs
}

// impFile builds a scanned File by extracting its imports from src.
func impFile(path, lang, src string) *File {
	refs, covered := ExtractImports(lang, path, []byte(src))
	return &File{Path: path, Language: lang, HasImports: covered, Imports: refs}
}

// impFiles turns path->source into scanned files, guessing the language from
// the extension, and returns the files with a workspace holding the sources
// (plus extra non-source files).
func impFiles(sources map[string]string, extra map[string]string) ([]*File, *impWS) {
	ws := map[string]string{}
	var files []*File
	paths := make([]string, 0, len(sources))
	for p := range sources {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		ws[p] = sources[p]
		files = append(files, impFile(p, impLang(p), sources[p]))
	}
	for p, s := range extra {
		ws[p] = s
	}
	return files, newImpWS(ws)
}

func impLang(p string) string {
	switch {
	case strings.HasSuffix(p, ".go"):
		return "go"
	case strings.HasSuffix(p, ".py"):
		return "python"
	case strings.HasSuffix(p, ".ts"):
		return "typescript"
	case strings.HasSuffix(p, ".tsx"):
		return "typescriptreact"
	case strings.HasSuffix(p, ".js"):
		return "javascript"
	case strings.HasSuffix(p, ".rs"):
		return "rust"
	case strings.HasSuffix(p, ".c"), strings.HasSuffix(p, ".h"):
		return "c"
	case strings.HasSuffix(p, ".cpp"), strings.HasSuffix(p, ".hpp"):
		return "cpp"
	case strings.HasSuffix(p, ".lua"):
		return "lua"
	case strings.HasSuffix(p, ".zig"):
		return "zig"
	}
	return ""
}
