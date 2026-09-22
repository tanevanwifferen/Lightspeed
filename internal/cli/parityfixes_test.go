package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Regression tests for docs/DECISIONS.md D42: the fixes of the parity review that
// are not the limit audit (limitcases_test.go). Each was checked to fail without
// its fix.

var testOnlyFiles = map[string]string{
	"util/util.go":      "package util\n\nfunc Helper() {}\n",
	"util/util_test.go": "package util_test\n\nimport \"fixture/util\"\n\nvar _ = util.Helper\n",
	"pa/x.go":           "package pa\n\nimport \"fixture/util\"\n\nvar _ = util.Helper\n",
	"pa/x_test.go":      "package pa\n\nimport \"fixture/util\"\n\nvar _ = util.Helper\n",
	"pz/z_test.go":      "package pz\n\nimport \"fixture/util\"\n\nvar _ = util.Helper\n",
}

func TestFindImportersSaysWhichTestOnlyImportersItLeftOut(t *testing.T) {
	idxWorkspace(t, testOnlyFiles)

	code, out, stderr := runMain("find_importers", "util")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	var d importersData
	env := decodeData(t, out, &d)
	if len(d.Importers) != 1 || d.Importers[0].File != "pa/x.go" || d.Total != 1 {
		t.Errorf("importers = %+v, want only pa/x.go (tests are not graph edges)", d.Importers)
	}
	// pz is imported by tests alone; pa's tests add nothing to what pa/x.go says,
	// and util's own external tests are not importers of it in any useful sense.
	if d.TestOnly == nil || d.TestOnly.Count != 1 || len(d.TestOnly.Files) != 1 || d.TestOnly.Files[0].File != "pz/z_test.go" || !d.TestOnly.Files[0].Test {
		t.Errorf("test_only_importers = %+v, want exactly pz/z_test.go", d.TestOnly)
	}
	if !envWarns(env, "test-only importer") || !envWarns(env, "pz/z_test.go") || !envWarns(env, "--include-tests") {
		t.Errorf("warnings = %q, want the test-only importer named, and the flag that lists it", env.Warnings)
	}

	// --limit bounds the named test-only importers too, and the count stays whole.
	code, out, _ = runMain("find_importers", "util", "--limit", "1")
	if code != ExitOK {
		t.Fatalf("--limit 1: exit %d\n%s", code, out)
	}
	var lim importersData
	decodeData(t, out, &lim)
	if lim.TestOnly == nil || lim.TestOnly.Count != 1 {
		t.Errorf("under --limit the test-only count must stay whole: %+v", lim.TestOnly)
	}

	code, out, _ = runMain("find_importers", "util", "--include-tests")
	if code != ExitOK {
		t.Fatalf("--include-tests: exit %d\n%s", code, out)
	}
	var inc importersData
	env = decodeData(t, out, &inc)
	files := []string{}
	for _, im := range inc.Importers {
		files = append(files, im.File)
	}
	if !slices.Equal(files, []string{"pa/x.go", "pz/z_test.go"}) || inc.Total != 2 {
		t.Errorf("with --include-tests the importers are %v, want pa/x.go and pz/z_test.go", files)
	}
	if inc.TestOnly == nil || !inc.TestOnly.Included || envWarns(env, "left out") {
		t.Errorf("merged test-only importers must say so and not claim to be left out: %+v %q", inc.TestOnly, env.Warnings)
	}

	code, out, _ = runMain("find_importers", "util", "--format", "text")
	if code != ExitOK || !strings.Contains(out, "pa/x.go:3:") || !strings.Contains(out, "# 1 test-only importer(s) left out") {
		t.Errorf("text: exit %d\n%s", code, out)
	}
}

func TestFindImportersDefaultBoundIsReported(t *testing.T) {
	files := map[string]string{"util/util.go": "package util\n\nfunc Helper() {}\n"}
	for i := 0; i < defaultImportersLimit+5; i++ {
		name := "p" + string(rune('a'+i/26)) + string(rune('a'+i%26))
		files[name+"/x.go"] = "package " + name + "\n\nimport \"fixture/util\"\n\nvar _ = util.Helper\n"
	}
	idxWorkspace(t, files)
	code, out, _ := runMain("find_importers", "util")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	var d importersData
	env := decodeData(t, out, &d)
	if d.Count != defaultImportersLimit || d.Total != defaultImportersLimit+5 || !d.Truncated || !envWarns(env, "raise --limit") {
		t.Errorf("count %d total %d truncated %v warnings %q: want the default bound of %d, reported",
			d.Count, d.Total, d.Truncated, env.Warnings, defaultImportersLimit)
	}
	code, out, _ = runMain("find_importers", "util", "--limit", "0")
	decodeData(t, out, &d)
	if code != ExitOK || d.Count != defaultImportersLimit+5 || d.Truncated {
		t.Errorf("--limit 0 must lift the bound: count %d truncated %v", d.Count, d.Truncated)
	}
}

func TestTreeAndRepoOutlineLimitIsTheirBound(t *testing.T) {
	limitWorkspace(t)
	for _, tc := range []struct{ cmd, own, rows string }{
		{"tree", "--max-files", "files"},
		{"repo_outline", "--max-dirs", "directories"},
	} {
		code, out, _ := runMain(tc.cmd, tc.own, "3", "--limit", "2")
		if code != ExitOK {
			t.Fatalf("%s: exit %d\n%s", tc.cmd, code, out)
		}
		if n := rowCount(limitData(t, out), tc.rows); n != 2 {
			t.Errorf("%s %s 3 --limit 2 listed %d rows, want the limit (2) to win", tc.cmd, tc.own, n)
		}
	}
}

func TestSearchSymbolsExposesMatchQualityAtEveryDetail(t *testing.T) {
	idxWorkspace(t, idxFiles)
	for _, detail := range []string{"compact", "standard", "full"} {
		code, out, stderr := runMain("search_symbols", "ParseConfig", "--detail", detail)
		if code != ExitOK {
			t.Fatalf("%s: exit %d\n%s%s", detail, code, out, stderr)
		}
		d, _ := decodeSearch(t, out)
		if len(d.Results) < 2 {
			t.Fatalf("%s: results = %+v", detail, d.Results)
		}
		for _, r := range d.Results {
			if r.Match == "" || r.Score <= 0 {
				t.Errorf("%s: row %s has match %q score %v; every row carries both", detail, r.ID, r.Match, r.Score)
			}
		}
		if d.Results[0].Match != "exact" || d.Results[1].Match != "prefix" {
			t.Errorf("%s: matches = %s, %s; want exact then prefix", detail, d.Results[0].Match, d.Results[1].Match)
		}
	}

	// Text: the exact row is unmarked, the weaker one says what it is.
	code, out, _ := runMain("search_symbols", "ParseConfig", "--format", "text")
	if code != ExitOK {
		t.Fatalf("text: exit %d\n%s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if strings.Contains(lines[0], "[~") || !strings.Contains(lines[1], "[~prefix]") {
		t.Errorf("text rows:\n%s\nwant the exact row unmarked and the prefix row marked [~prefix]", out)
	}
}

func TestSearchSymbolsSaysUpFrontThatAnIdentifierHasNoNameMatch(t *testing.T) {
	idxWorkspace(t, idxFiles)

	// `Config` is a word of ParseConfig and parseConfigFile, not their name or
	// the start of it: token matches, and the first thing said is that.
	code, out, _ := runMain("search_symbols", "Config")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	d, env := decodeSearch(t, out)
	if len(d.Results) == 0 || d.Results[0].Match != "token" {
		t.Fatalf("results = %+v, want token matches", d.Results)
	}
	if len(env.Warnings) == 0 || !strings.HasPrefix(env.Warnings[0], "no symbol named Config (exact or prefix); these are token matches") || d.Notice != env.Warnings[0] {
		t.Errorf("warnings = %q notice = %q, want the notice first", env.Warnings, d.Notice)
	}

	code, out, _ = runMain("search_symbols", "Config", "--format", "text")
	if code != ExitOK {
		t.Fatalf("text: exit %d\n%s", code, out)
	}
	first, _, _ := strings.Cut(out, "\n")
	if !strings.HasPrefix(first, "# no symbol named Config (exact or prefix); these are token matches") {
		t.Errorf("first line of text = %q", first)
	}
	if strings.Count(out, "no symbol named Config") != 1 {
		t.Errorf("the notice must appear once, first, not again as a trailer:\n%s", out)
	}
	if !strings.Contains(out, "[~token]") {
		t.Errorf("token rows are marked:\n%s", out)
	}

	// Nothing at all matching is the authoritative empty answer, and also first.
	code, out, _ = runMain("search_symbols", "Zzyzx")
	if code != ExitProblems {
		t.Fatalf("empty: exit %d\n%s", code, out)
	}
	_, env = decodeSearch(t, out)
	if len(env.Warnings) == 0 || !strings.HasPrefix(env.Warnings[0], `no symbol matches "Zzyzx"`) {
		t.Errorf("empty answer: warnings = %q, want the verdict first", env.Warnings)
	}
	_, out, _ = runMain("search_symbols", "Zzyzx", "--format", "text")
	if first, _, _ := strings.Cut(out, "\n"); !strings.HasPrefix(first, `# no symbol matches "Zzyzx"`) || strings.Count(out, "no symbol matches") != 1 {
		t.Errorf("empty answer in text:\n%s", out)
	}

	// Nothing to say when the name matches, or when the query is words.
	for _, q := range []string{"ParseConfig", "Parse", "parse config"} {
		_, out, _ = runMain("search_symbols", q)
		d, env = decodeSearch(t, out)
		if d.Notice != "" || envWarns(env, "no symbol named") {
			t.Errorf("query %q: unexpected notice %q", q, d.Notice)
		}
	}
}

func TestIdentifierNoticeQueryShapes(t *testing.T) {
	for q, want := range map[string]bool{
		"ParseConfig": true, "parse_config": true, "pkg.Type.Method": true, "a::b": true, "変数": true,
		"parse config": false, "Parse-Config": false, "": false, "func()": false,
	} {
		if got := identifierRE.MatchString(q); got != want {
			t.Errorf("identifier-like(%q) = %v, want %v", q, got, want)
		}
	}
}

func TestDependencyGraphOutsideTheWorkspaceIsRefused(t *testing.T) {
	dir := idxWorkspace(t, idxFiles)
	// Nothing there, and not in the workspace either: it is not "no such node".
	for _, args := range [][]string{
		{"dependency_graph", "../nosuch"},
		{"dependency_graph", "../../../../nosuch/x.go"},
		{"dependency_graph", "/nonexistent/elsewhere"},
		{"find_importers", "../nosuch.go"},
	} {
		code, out, _ := runMain(args...)
		env := decodeEnvelope(t, out)
		if code != ExitUsage || env.OK || env.Error == nil || string(env.Error.Code) != "outside_workspace" {
			t.Errorf("%v: exit %d, %s; want outside_workspace, exit 2", args, code, out)
		}
	}
	// From a subdirectory, `..` reaches back into the workspace: a file that is
	// not there is that, not an escape.
	if err := os.MkdirAll(filepath.Join(dir, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "deep"))
	code, out, _ := runMain("dependency_graph", "../nosuch.go")
	env := decodeEnvelope(t, out)
	if env.OK || env.Error == nil || string(env.Error.Code) != "no_such_file" || code != ExitUsage {
		t.Errorf("../nosuch.go from a subdirectory: exit %d, %s; want no_such_file", code, out)
	}
	code, out, _ = runMain("dependency_graph", "../a.go")
	if code != ExitOK {
		t.Errorf("../a.go from a subdirectory is inside the workspace: exit %d\n%s", code, out)
	}
}

func TestItemErrorWarningDoesNotSayTheTargetTwice(t *testing.T) {
	for _, tc := range []struct{ target, message, want string }{
		{"x.go", "x.go: no such file", "x.go: no such file"},
		{"x.go", "no such file", "x.go: no such file"},
		{"x.go", "x.go.bak: unreadable", "x.go: x.go.bak: unreadable"},
	} {
		if got := (itemError{Target: tc.target, Message: tc.message}).warning(); got != tc.want {
			t.Errorf("warning(%q, %q) = %q, want %q", tc.target, tc.message, got, tc.want)
		}
	}

	idxWorkspace(t, idxFiles)
	code, out, _ := runMain("outline", "a.go", "nosuch.go")
	if code != ExitProblems && code != ExitOK {
		t.Fatalf("exit %d\n%s", code, out)
	}
	_, env := decodeSearch(t, out)
	if !envWarns(env, "nosuch.go: no such file") || envWarns(env, "nosuch.go: nosuch.go") {
		t.Errorf("warnings = %q, want the target once", env.Warnings)
	}
	_, out, _ = runMain("outline", "a.go", "nosuch.go", "--format", "text")
	if strings.Contains(out, "nosuch.go: nosuch.go") {
		t.Errorf("text repeats the target:\n%s", out)
	}
}

func TestRepoOutlineTextShowsSymbolCountsWhenWarm(t *testing.T) {
	idxWorkspace(t, idxFiles)
	_, cold, _ := runMain("repo_outline", "--format", "text")
	if strings.Contains(cold, " sym") {
		t.Errorf("a cold index has no counts to show:\n%s", cold)
	}
	if code, out, stderr := runMain("index", "build"); code != ExitOK {
		t.Fatalf("index build: exit %d\n%s%s", code, out, stderr)
	}
	code, warm, _ := runMain("repo_outline", "--format", "text")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, warm)
	}
	header, _, _ := strings.Cut(warm, "\n")
	if !strings.Contains(header, "symbols in") {
		t.Errorf("header = %q, want the workspace's symbol count", header)
	}
	var sub string
	for _, l := range strings.Split(warm, "\n") {
		if strings.Contains(l, "sub/") {
			sub = l
		}
	}
	if !strings.Contains(sub, "1 sym") {
		t.Errorf("the sub/ line = %q, want its one symbol counted", sub)
	}
}

func TestExplicitIndexBuildIsNotLazyInTextEither(t *testing.T) {
	idxWorkspace(t, idxFiles)
	code, out, stderr := runMain("index", "build", "--format", "text")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s%s", code, out, stderr)
	}
	if strings.Contains(out+stderr, "lazily") {
		t.Errorf("an explicit build was reported as lazy:\n%s%s", out, stderr)
	}
	if !strings.Contains(out, "built") {
		t.Errorf("a cold build says what it built:\n%s", out)
	}
}
