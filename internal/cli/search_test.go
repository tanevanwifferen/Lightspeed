package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// search runs `search_text` in the working directory and decodes its payload.
func search(t *testing.T, args ...string) (searchData, rawEnvelope, int) {
	t.Helper()
	code, stdout, stderr := runMain(append([]string{"search_text"}, args...)...)
	var data searchData
	env := decodeData(t, stdout, &data)
	if !env.OK && env.Error != nil {
		t.Logf("stderr: %s", stderr)
	}
	return data, env, code
}

// hits lists the matches as `file:line:col`.
func hits(d searchData) []string {
	out := make([]string, len(d.Matches))
	for i, m := range d.Matches {
		out[i] = fmt.Sprintf("%s:%d:%d", m.File, m.Line, m.Col)
	}
	return out
}

// searchWorkspace is a workspace with the files, and the working directory in it.
func searchWorkspace(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := tree(t, files)
	t.Chdir(dir)
	return dir
}

func warned(env rawEnvelope, sub string) bool {
	return slices.ContainsFunc(env.Warnings, func(w string) bool { return strings.Contains(w, sub) })
}

// TestSearchTextSubstringIsCaseInsensitive: the default is a case-insensitive
// substring, the column is 1-based, and --case-sensitive turns case on.
func TestSearchTextSubstringIsCaseInsensitive(t *testing.T) {
	searchWorkspace(t, map[string]string{
		"a.go":      "package a\n\nvar Hello = \"World\"\n// say HELLO, hello.\n",
		"docs/b.md": "nothing here\nHeLLo there\n",
	})
	data, _, code := search(t, "hello")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	want := []string{"a.go:3:5", "a.go:4:8", "docs/b.md:2:1"}
	if got := hits(data); !slices.Equal(got, want) {
		t.Errorf("matches = %v, want %v", got, want)
	}
	if data.Matches[1].Hits != 2 || data.Matches[1].Text != "// say HELLO, hello." {
		t.Errorf("the line with two matches: %+v", data.Matches[1])
	}
	if data.Total != 3 || data.Count != 3 || data.Truncated || data.FilesMatched != 2 {
		t.Errorf("counts: %+v", data)
	}

	data, _, _ = search(t, "hello", "--case-sensitive")
	if got := hits(data); !slices.Equal(got, []string{"a.go:4:15"}) {
		t.Errorf("case-sensitive hello = %v", got)
	}
	data, _, code = search(t, "hELLO", "--case-sensitive")
	if code != ExitProblems || data.Total != 0 || len(data.Matches) != 0 {
		t.Errorf("case-sensitive hELLO: exit %d, %+v (an empty answer is exit 1, ok:true)", code, data)
	}
	data, _, _ = search(t, "Hello", "--case-sensitive")
	if got := hits(data); !slices.Equal(got, []string{"a.go:3:5"}) {
		t.Errorf("case-sensitive Hello = %v", got)
	}
}

// TestSearchTextRegex: --regex is RE2, a bad pattern is a usage error, and a
// pattern that would make a backtracking engine explode finishes at once.
func TestSearchTextRegex(t *testing.T) {
	searchWorkspace(t, map[string]string{
		"a.txt": "func Foo(a int)\nfunc Bar()\nfunc  Baz(x, y int)\n",
		"evil":  strings.Repeat("a", 40000) + "b\n",
	})
	data, _, _ := search(t, `func\s+\w+\(\w`, "--regex")
	if got := hits(data); !slices.Equal(got, []string{"a.txt:1:1", "a.txt:3:1"}) {
		t.Errorf("regex matches = %v", got)
	}
	// Without --regex the same text is literal.
	if data, _, code := search(t, `func\s+`); code != ExitProblems || data.Total != 0 {
		t.Errorf("literal backslash-s matched: exit %d, %+v", code, data)
	}

	code, stdout, _ := runMain("search_text", "--regex", "(unclosed")
	if got, _ := errorData(t, stdout); code != ExitUsage || got != "usage" {
		t.Errorf("invalid regex: exit %d, code %q\n%s", code, got, stdout)
	}
	code, stdout, _ = runMain("search_text", "")
	if got, _ := errorData(t, stdout); code != ExitUsage || got != "usage" {
		t.Errorf("empty query: exit %d, code %q", code, got)
	}

	start := time.Now()
	data, _, _ = search(t, `(a+)+$`, "--regex", "--glob", "evil")
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("(a+)+$ took %s; RE2 must not backtrack", took)
	}
	if data.Total != 0 {
		t.Errorf("(a+)+$ matched a line that ends in b: %+v", data)
	}
}

// TestSearchTextWord: --word wants a boundary on each side, with letters of
// any script counting as word characters.
func TestSearchTextWord(t *testing.T) {
	searchWorkspace(t, map[string]string{
		"w.txt": "concatenate\ncategory\na cat.\ncat_sat\nthe (cat)\n名前空間\nその名前を見る\n変数名前\nx 名前 y\n",
	})
	data, _, _ := search(t, "cat", "--word")
	if got := hits(data); !slices.Equal(got, []string{"w.txt:3:3", "w.txt:5:6"}) {
		t.Errorf("--word cat = %v", got)
	}
	data, _, _ = search(t, "名前", "--word")
	// 名前空間 and 変数名前 continue into more letters; その名前を見る is joined
	// to its neighbours too (see D26: a script with no spaces has no word edges
	// for this rule); only `x 名前 y`, set off by spaces, is a whole word.
	if got := hits(data); !slices.Equal(got, []string{"w.txt:9:3"}) {
		t.Errorf("--word 名前 = %v", got)
	}
	data, _, _ = search(t, `c.t`, "--regex", "--word")
	if got := hits(data); !slices.Equal(got, []string{"w.txt:3:3", "w.txt:5:6"}) {
		t.Errorf("--regex --word c.t = %v", got)
	}
	data, _, _ = search(t, "cat")
	if data.Total != 5 {
		t.Errorf("without --word cat matches %d lines, want 5", data.Total)
	}
}

// TestSearchTextGlobs: --glob includes and (with !) excludes, repeatably; a
// pattern with no slash matches at any depth, and a directory matches what is
// below it.
func TestSearchTextGlobs(t *testing.T) {
	searchWorkspace(t, map[string]string{
		"main.go":            "needle\n",
		"internal/a.go":      "needle\n",
		"internal/a_test.go": "needle\n",
		"internal/gen/g.go":  "needle\n",
		"README.md":          "needle\n",
		"web/app.ts":         "needle\n",
	})
	paths := func(args ...string) []string {
		data, _, _ := search(t, append([]string{"needle"}, args...)...)
		var out []string
		for _, m := range data.Matches {
			out = append(out, m.File)
		}
		return out
	}
	for name, tc := range map[string]struct {
		args []string
		want []string
	}{
		"extension at any depth": {[]string{"--glob", "*.go"}, []string{"internal/a.go", "internal/a_test.go", "internal/gen/g.go", "main.go"}},
		"two includes":           {[]string{"--glob", "*.md", "--glob", "*.ts"}, []string{"README.md", "web/app.ts"}},
		"include and exclude":    {[]string{"--glob", "*.go", "--glob", "!*_test.go", "--glob", "!internal/gen"}, []string{"internal/a.go", "main.go"}},
		"only excludes":          {[]string{"--glob", "!**/*.go", "--glob", "!go.mod"}, []string{"README.md", "web/app.ts"}},
		"a directory":            {[]string{"--glob", "internal"}, []string{"internal/a.go", "internal/a_test.go", "internal/gen/g.go"}},
		"anchored":               {[]string{"--glob", "internal/*.go"}, []string{"internal/a.go", "internal/a_test.go"}},
		"braces":                 {[]string{"--glob", "**/*.{md,ts}"}, []string{"README.md", "web/app.ts"}},
		"a scope":                {[]string{"--path", "internal", "--glob", "!*_test.go"}, []string{"internal/a.go", "internal/gen/g.go"}},
	} {
		if got := paths(tc.args...); !slices.Equal(got, tc.want) {
			t.Errorf("%s: %v = %v, want %v", name, tc.args, got, tc.want)
		}
	}
	code, stdout, _ := runMain("search_text", "needle", "--glob", "[")
	if got, _ := errorData(t, stdout); code != ExitUsage || got != "usage" {
		t.Errorf("a bad glob: exit %d, code %q", code, got)
	}
}

// TestSearchTextSkipsIgnoredBinaryAndOversizeFiles: the file set is git's
// (tracked plus untracked-not-ignored), binary files and files over the cap are
// left out, and every skip but the ignored ones is in the warnings.
func TestSearchTextSkipsIgnoredBinaryAndOversizeFiles(t *testing.T) {
	dir := repoWithFiles(t, map[string]string{
		".gitignore":    "build/\n*.log\n",
		"keep.txt":      "needle in keep\n",
		"build/out.txt": "needle in build\n",
		"debug.log":     "needle in log\n",
		"blob.bin":      "needle\x00\x01\x02 binary\n",
	})
	write(t, filepath.Join(dir, "untracked.txt"), "needle untracked\n")
	write(t, filepath.Join(dir, "later.log"), "needle ignored untracked\n")
	write(t, filepath.Join(dir, "huge.txt"), "needle "+strings.Repeat("x", defaultMaxFileBytes)+"\n")

	data, env, code := search(t, "needle")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	if got, want := hits(data), []string{"keep.txt:1:1", "untracked.txt:1:1"}; !slices.Equal(got, want) {
		t.Errorf("matches = %v, want %v", got, want)
	}
	if data.Source != "git" || data.FilesSkipped != 2 {
		t.Errorf("source %q, skipped %d, want git and 2 (blob.bin, huge.txt)", data.Source, data.FilesSkipped)
	}
	if !warned(env, "skipped 1 binary file(s)") || !warned(env, "blob.bin") {
		t.Errorf("no binary warning: %q", env.Warnings)
	}
	if !warned(env, "skipped 1 file(s) over --max-file-bytes") || !warned(env, "huge.txt") {
		t.Errorf("no oversize warning: %q", env.Warnings)
	}

	// The cap is adjustable, and raising it searches the big file too.
	data, env, _ = search(t, "needle", "--max-file-bytes", fmt.Sprint(defaultMaxFileBytes*2))
	if !slices.Contains(hits(data), "huge.txt:1:1") || warned(env, "over --max-file-bytes") {
		t.Errorf("with a bigger cap: %v %q", hits(data), env.Warnings)
	}
}

// TestSearchTextTextFormatReportsSkips: --format text carries the same
// warnings as the envelope, as `# ` lines after the matches, so a skipped file
// is never silent in either format.
func TestSearchTextTextFormatReportsSkips(t *testing.T) {
	dir := repoWithFiles(t, map[string]string{
		"keep.txt": "needle in keep\n",
		"blob.bin": "needle\x00\x01\x02 binary\n",
	})
	write(t, filepath.Join(dir, "huge.txt"), "needle "+strings.Repeat("x", defaultMaxFileBytes)+"\n")
	outside := t.TempDir()
	write(t, filepath.Join(outside, "extra.txt"), "needle outside\n")
	if err := os.Symlink(filepath.Join(outside, "extra.txt"), filepath.Join(dir, "leak.txt")); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := runMain("search_text", "needle", "--format", "text")
	if code != ExitOK {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
	lines := strings.Split(strings.TrimRight(stdout, "\n"), "\n")
	if lines[0] != "keep.txt:1:1: needle in keep" {
		t.Errorf("first line %q", lines[0])
	}
	var notices []string
	for _, l := range lines[1:] {
		if !strings.HasPrefix(l, "# ") {
			t.Errorf("a line after the matches that is not a notice: %q", l)
		}
		notices = append(notices, l)
	}
	joined := strings.Join(notices, "\n")
	for _, want := range []string{"# skipped 1 binary file(s)", "blob.bin", "# skipped 1 file(s) over --max-file-bytes", "huge.txt",
		"# skipped 1 symlink(s) that lead outside the workspace", "leak.txt"} {
		if !strings.Contains(joined, want) {
			t.Errorf("text output lacks %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "needle outside") {
		t.Errorf("read through an escaping symlink:\n%s", stdout)
	}
}

// TestSearchTextTextFormatReportsMissingGitignore: outside a repository the
// text format says .gitignore was not applied.
func TestSearchTextTextFormatReportsMissingGitignore(t *testing.T) {
	searchWorkspace(t, map[string]string{"a.txt": "needle\n"})
	_, stdout, _ := runMain("search_text", "needle", "--format", "text")
	if !strings.Contains(stdout, "# ") || !strings.Contains(stdout, ".gitignore was not applied") {
		t.Errorf("text output:\n%s", stdout)
	}
}

// TestSearchTextWithoutGitWalks: outside a repository the tree is walked, hidden
// and build directories are skipped and the warning says .gitignore was not used.
func TestSearchTextWithoutGitWalks(t *testing.T) {
	searchWorkspace(t, map[string]string{
		"a.txt":             "needle\n",
		".hidden/b.txt":     "needle\n",
		"node_modules/c.js": "needle\n",
	})
	data, env, _ := search(t, "needle")
	if got := hits(data); !slices.Equal(got, []string{"a.txt:1:1"}) || data.Source != "walk" {
		t.Errorf("matches = %v (%s)", got, data.Source)
	}
	if !warned(env, ".gitignore was not applied") {
		t.Errorf("warnings: %q", env.Warnings)
	}
}

// TestSearchTextColumnsAreBytes: the column is a 1-based byte offset, so CJK
// and emoji before the match count as their UTF-8 bytes, and CRLF does not
// leak into the text.
func TestSearchTextColumnsAreBytes(t *testing.T) {
	searchWorkspace(t, map[string]string{
		"cjk.txt": "こんにちは target\n🎉🎉x\n日本語の名前 = 1\r\nplain\r\n",
	})
	data, _, _ := search(t, "target")
	if len(data.Matches) != 1 || data.Matches[0].Col != 17 {
		t.Errorf("target: %+v", data.Matches)
	}
	data, _, _ = search(t, "x", "--glob", "cjk.txt")
	if len(data.Matches) != 1 || data.Matches[0].Col != 9 {
		t.Errorf("x: %+v", data.Matches)
	}
	data, _, _ = search(t, "名前")
	if len(data.Matches) != 1 || data.Matches[0].Col != len("日本語の")+1 || data.Matches[0].Text != "日本語の名前 = 1" {
		t.Errorf("名前: %+v", data.Matches)
	}
	// A regex reports its own start, in bytes, too.
	data, _, _ = search(t, `\p{Han}+`, "--regex")
	if len(data.Matches) != 1 || data.Matches[0].Col != 1 || data.Matches[0].Line != 3 {
		t.Errorf(`\p{Han}+: %+v`, data.Matches)
	}
	data, _, _ = search(t, "plain")
	if data.Matches[0].Text != "plain" {
		t.Errorf("CRLF leaked into the text: %q", data.Matches[0].Text)
	}
}

// TestSearchTextTruncatesAndCountsEverything: at the limit the output stops,
// says truncated and still reports how many lines matched in all.
func TestSearchTextTruncatesAndCountsEverything(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 12; i++ {
		var b strings.Builder
		for j := 0; j < 5; j++ {
			fmt.Fprintf(&b, "line %d needle\nother\n", j)
		}
		files[fmt.Sprintf("f%02d.txt", i)] = b.String()
	}
	searchWorkspace(t, files)

	data, env, code := search(t, "needle")
	if code != ExitOK || data.Count != 50 || data.Total != 60 || !data.Truncated || data.Limit != 50 {
		t.Fatalf("default limit: exit %d, count %d, total %d, truncated %v, limit %d", code, data.Count, data.Total, data.Truncated, data.Limit)
	}
	if !warned(env, "listed 50 of 60") {
		t.Errorf("no truncation warning: %q", env.Warnings)
	}
	// The listed matches are the first ones, in path and line order.
	if data.Matches[0].File != "f00.txt" || data.Matches[0].Line != 1 || data.Matches[49].File != "f09.txt" || data.Matches[49].Line != 9 {
		t.Errorf("first %+v last %+v", data.Matches[0], data.Matches[49])
	}
	data, _, _ = search(t, "needle", "--limit", "7")
	if data.Count != 7 || data.Total != 60 || !data.Truncated {
		t.Errorf("--limit 7: %d of %d", data.Count, data.Total)
	}
	data, env, _ = search(t, "needle", "--limit", "0")
	if data.Count != 60 || data.Truncated || warned(env, "listed") {
		t.Errorf("--limit 0: %d of %d truncated %v", data.Count, data.Total, data.Truncated)
	}
	// Text output says so too.
	_, stdout, _ := runMain("search_text", "needle", "--limit", "2", "--format", "text")
	if !strings.HasSuffix(stdout, "# listed 2 of 60 matching lines; narrow it with --path or --glob, or raise --limit\n") || !strings.HasPrefix(stdout, "f00.txt:1:8: line 0 needle\n") {
		t.Errorf("text output:\n%s", stdout)
	}
}

// TestSearchTextOrderIsDeterministic: many files searched in parallel come out
// in path and line order, run after run.
func TestSearchTextOrderIsDeterministic(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 80; i++ {
		files[fmt.Sprintf("d%d/f%02d.txt", i%7, i)] = strings.Repeat("x\nneedle\n", 3)
	}
	searchWorkspace(t, files)
	first, _, _ := search(t, "needle", "--limit", "0")
	if first.Total != 240 {
		t.Fatalf("total %d", first.Total)
	}
	sorted := slices.IsSortedFunc(first.Matches, func(a, b searchMatch) int {
		if c := strings.Compare(a.File, b.File); c != 0 {
			return c
		}
		return a.Line - b.Line
	})
	if !sorted {
		t.Errorf("matches are not in (path, line) order")
	}
	for i := 0; i < 5; i++ {
		again, _, _ := search(t, "needle", "--limit", "0")
		if !slices.Equal(hits(again), hits(first)) {
			t.Fatalf("run %d differs", i)
		}
	}
}

// TestSearchTextClipsLongLines: a long line is cut to a window around the match,
// with a marker that says how much was dropped from each end, and the column is
// still the one in the file.
func TestSearchTextClipsLongLines(t *testing.T) {
	long := strings.Repeat("あ", 300) + " needle " + strings.Repeat("い", 300)
	searchWorkspace(t, map[string]string{
		"min.js": long + "\n",
		"ok.txt": "short needle line\n",
	})
	data, _, _ := search(t, "needle", "--glob", "min.js")
	if len(data.Matches) != 1 {
		t.Fatalf("matches: %+v", data.Matches)
	}
	m := data.Matches[0]
	if !m.Clipped || !strings.Contains(m.Text, "needle") || !strings.HasPrefix(m.Text, "[+") || !strings.Contains(m.Text, "B]…") ||
		!strings.Contains(m.Text, "…[+") || len(m.Text) > searchLineBytes+40 {
		t.Errorf("clipped text (%d bytes) = %q, clipped=%v", len(m.Text), m.Text, m.Clipped)
	}
	if m.Col != 300*3+2 {
		t.Errorf("col = %d, want the byte column in the file, %d", m.Col, 300*3+2)
	}
	if !utf8Valid(m.Text) {
		t.Errorf("the window split a character: %q", m.Text)
	}
	data, _, _ = search(t, "needle", "--glob", "ok.txt")
	if data.Matches[0].Clipped || data.Matches[0].Text != "short needle line" {
		t.Errorf("a short line was clipped: %+v", data.Matches[0])
	}
}

func utf8Valid(s string) bool { return strings.ToValidUTF8(s, "�") == s }

// TestSearchTextContext: --context adds the lines around each match, in JSON and
// in text (grep style, with `--` between groups that are not adjacent).
func TestSearchTextContext(t *testing.T) {
	searchWorkspace(t, map[string]string{
		"c.txt": "1\n2\n3 needle\n4\n5\n6\n7\n8\n9 needle\n10\n",
	})
	data, _, _ := search(t, "needle", "--context", "1")
	if len(data.Matches) != 2 ||
		!slices.Equal(data.Matches[0].Before, []string{"2"}) || !slices.Equal(data.Matches[0].After, []string{"4"}) ||
		!slices.Equal(data.Matches[1].Before, []string{"8"}) || !slices.Equal(data.Matches[1].After, []string{"10"}) {
		t.Errorf("context: %+v", data.Matches)
	}
	// Context at the edge of the file is what there is.
	data, _, _ = search(t, "1", "--context", "2", "--regex", "--glob", "c.txt")
	if len(data.Matches) == 0 || len(data.Matches[0].Before) != 0 {
		t.Errorf("edge: %+v", data.Matches[0])
	}

	_, stdout, _ := runMain("search_text", "needle", "--context", "1", "--format", "text")
	want := "c.txt-2- 2\nc.txt:3:3: 3 needle\nc.txt-4- 4\n--\nc.txt-8- 8\nc.txt:9:3: 9 needle\nc.txt-10- 10\n"
	if stdout != want+"# not a git repository, so .gitignore was not applied; hidden and build directories were skipped instead\n" {
		t.Errorf("text with context:\n%s\nwant:\n%s", stdout, want)
	}
	// Adjacent matches share their context lines and do not repeat them.
	_, stdout, _ = runMain("search_text", "needle", "--context", "6", "--format", "text")
	if strings.Count(stdout, "c.txt-5- 5\n") != 1 || strings.Contains(stdout, "--\n") {
		t.Errorf("overlapping context:\n%s", stdout)
	}
}

// TestSearchTextScopesToAPath: --path names a subdirectory or one file; paths
// stay relative to the workspace root, not to the scope.
func TestSearchTextScopesToAPath(t *testing.T) {
	dir := searchWorkspace(t, map[string]string{
		"a.txt":         "needle\n",
		"pkg/b.txt":     "needle\n",
		"pkg/sub/c.txt": "needle\n",
	})
	data, _, _ := search(t, "needle", "--path", "pkg")
	if got := hits(data); !slices.Equal(got, []string{"pkg/b.txt:1:1", "pkg/sub/c.txt:1:1"}) || data.Dir != "pkg" {
		t.Errorf("--path pkg: %v (dir %q)", got, data.Dir)
	}
	data, _, _ = search(t, "needle", "--path", filepath.Join(dir, "pkg", "sub", "c.txt"))
	if got := hits(data); !slices.Equal(got, []string{"pkg/sub/c.txt:1:1"}) || data.Source != "file" {
		t.Errorf("--path file: %v", got)
	}
	// From a subdirectory the workspace root is still the workspace's, given
	// the marker that makes it one.
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(dir, "pkg"))
	data, _, _ = search(t, "needle")
	if got := hits(data); !slices.Equal(got, []string{"pkg/b.txt:1:1", "pkg/sub/c.txt:1:1"}) {
		t.Errorf("from pkg: %v", got)
	}
	if data.Root != canonPath(dir) {
		t.Errorf("root = %q, want %q", data.Root, canonPath(dir))
	}
}

// TestSearchTextIsConfinedToTheWorkspace: a --path that leaves the workspace by
// `..`, an absolute path or a symlink is refused before anything is read, and
// so is a symlink inside the tree that points out of it.
func TestSearchTextIsConfinedToTheWorkspace(t *testing.T) {
	ws, outside := outsideWorkspace(t)
	write(t, filepath.Join(outside, "extra.txt"), "needle outside\n")
	write(t, filepath.Join(ws, "in.txt"), "needle inside\n")
	// A file symlink to a file out there, and one to a file in here.
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(ws, "leak.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ws, "in.txt"), filepath.Join(ws, "ok-link.txt")); err != nil {
		t.Fatal(err)
	}

	for name, dir := range outsideDirs(t, ws, outside) {
		code, stdout, _ := runMain("search_text", "needle", "--path", dir)
		got, _ := errorData(t, stdout)
		if code != ExitUsage || got != "outside_workspace" || strings.Contains(stdout, "outside") && strings.Contains(stdout, "needle outside") {
			t.Errorf("--path %s: exit %d, code %q\n%s", name, code, got, stdout)
		}
	}
	file := filepath.Join(outside, "extra.txt")
	if code, stdout, _ := runMain("search_text", "needle", "--path", file); code != ExitUsage || strings.Contains(stdout, "needle outside") {
		t.Errorf("--path <file outside>: exit %d\n%s", code, stdout)
	}

	// A symlink in the tree that leads out is not read, and is said.
	data, env, _ := search(t, "do not list")
	if data.Total != 0 || !warned(env, "symlink(s) that lead outside the workspace") || !warned(env, "leak.txt") {
		t.Errorf("the escaping symlink: %+v %q", data.Matches, env.Warnings)
	}
	// One that stays inside is read as a file of its own.
	data, _, _ = search(t, "needle inside")
	if !slices.Contains(hits(data), "ok-link.txt:1:1") {
		t.Errorf("an inside symlink: %v", hits(data))
	}
	// --root is confined the same way as tree's --path: the scope must be inside it.
	if code, stdout, _ := runMain("search_text", "needle", "--root", ws, "--path", outside); code != ExitUsage || strings.Contains(stdout, "needle outside") {
		t.Errorf("--root --path outside: exit %d\n%s", code, stdout)
	}
}

// TestSearchTextWithSymbol: with a server, each match carries the id of the
// symbol that contains it; a file no server handles has none and the answer
// says so once; without the flag no server is asked.
func TestSearchTextWithSymbol(t *testing.T) {
	dir, _ := symWorkspace(t, map[string]string{"notes.txt": "a ToUpper note\n"})
	symScenario(t, symHierarchical(), nil)

	data, env, code := search(t, "ToUpper", "--with-symbol")
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	bySymbol := map[string]string{}
	for _, m := range data.Matches {
		bySymbol[m.File] = m.Symbol
	}
	if bySymbol["sym.go"] != "sym.go::Greeter.Greet#method" {
		t.Errorf("ToUpper is inside Greet: %v", bySymbol)
	}
	if bySymbol["notes.txt"] != "" || !warned(env, "--with-symbol: no symbol ids") || !warned(env, ".txt") {
		t.Errorf("a file with no server: %v %q", bySymbol, env.Warnings)
	}
	// A multi-byte prefix: the UTF-16 column is not the byte column.
	data, _, _ = search(t, "名前", "--with-symbol", "--glob", "sym.go")
	if len(data.Matches) != 1 || data.Matches[0].Symbol != "sym.go::名前#variable" {
		t.Errorf("名前: %+v", data.Matches)
	}
	// A match outside every symbol has none.
	data, _, _ = search(t, "import", "--with-symbol", "--glob", "sym.go")
	if len(data.Matches) != 1 || data.Matches[0].Symbol != "" {
		t.Errorf("import: %+v", data.Matches)
	}
	// Text output gives the symbol's full stable id, path and kind included:
	// it is what `source` takes, so it must not be a name that has to be
	// reassembled.
	_, stdout, _ := runMain("search_text", "ToUpper", "--with-symbol", "--glob", "sym.go", "--format", "text")
	if !strings.Contains(stdout, "[in sym.go::Greeter.Greet#method]") {
		t.Errorf("text output: %s", stdout)
	}
	// Without the flag nothing asks a server, and there is no symbol field.
	data, env, _ = search(t, "ToUpper")
	for _, m := range data.Matches {
		if m.Symbol != "" {
			t.Errorf("a symbol without --with-symbol: %+v", m)
		}
	}
	if warned(env, "--with-symbol") {
		t.Errorf("warnings: %q", env.Warnings)
	}
	_ = dir
}

// TestSearchTextNeedsNoServer: with no server definition matching anything and
// no server started, the search still answers.
func TestSearchTextNeedsNoServer(t *testing.T) {
	searchWorkspace(t, map[string]string{"a.go": "package a // needle\n"})
	t.Setenv("PATH", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	data, _, code := search(t, "needle", "--no-daemon")
	if code != ExitOK || data.Total != 1 {
		t.Errorf("exit %d: %+v", code, data)
	}
}

// TestMCPSearchText: search_text is a tool with the CLI's envelope, `glob` a
// list, and the same confinement, with the workspace from the call.
func TestMCPSearchText(t *testing.T) {
	ws := tree(t, map[string]string{
		"a.go":     "package a\n// Needle\n",
		"b.md":     "needle\n",
		"pkg/c.go": "needle\n",
	})
	outside := t.TempDir()
	write(t, filepath.Join(outside, "o.txt"), "needle outside\n")
	// The server's own directory is elsewhere: the workspace can only come from the call.
	cs := mcpSession(t, t.TempDir())

	res := callTool(t, cs, "search_text", map[string]any{
		"workspace": ws, "query": "needle", "glob": []string{"*.go", "!pkg"}, "limit": 10,
	})
	if res.IsError {
		t.Fatalf("error result: %s", resultText(t, res))
	}
	var data searchData
	env := decodeData(t, resultText(t, res), &data)
	if !env.OK || !slices.Equal(hits(data), []string{"a.go:2:4"}) || data.Root != canonPath(ws) {
		t.Errorf("matches %v root %q", hits(data), data.Root)
	}

	res = callTool(t, cs, "search_text", map[string]any{"workspace": ws, "query": "needle", "path": "pkg", "case_sensitive": true, "word": true})
	decodeData(t, resultText(t, res), &data)
	if res.IsError || !slices.Equal(hits(data), []string{"pkg/c.go:1:1"}) {
		t.Errorf("--path pkg: %v", hits(data))
	}

	// No match is an answer, not a failed tool.
	res = callTool(t, cs, "search_text", map[string]any{"workspace": ws, "query": "absent"})
	if res.IsError || resultExit(t, res) != ExitProblems {
		t.Errorf("no match: isError=%v exit %d", res.IsError, resultExit(t, res))
	}

	for _, tc := range []map[string]any{
		{"workspace": ws, "query": "needle", "path": outside},
		{"workspace": ws, "query": "needle", "path": "../"},
		{"workspace": ws, "query": "needle", "root": outside, "path": ws},
	} {
		res = callTool(t, cs, "search_text", tc)
		text := resultText(t, res)
		env := mcpEnvelope(t, text)
		if !res.IsError || env.OK || env.Error == nil || env.Error.Code != "outside_workspace" || strings.Contains(text, "needle outside") {
			t.Errorf("%v: isError=%v\n%s", tc, res.IsError, text)
		}
	}
	res = callTool(t, cs, "search_text", map[string]any{"workspace": ws, "query": "(", "regex": true})
	if env := mcpEnvelope(t, resultText(t, res)); !res.IsError || env.Error == nil || env.Error.Code != "usage" {
		t.Errorf("bad regex is a usage error: %s", resultText(t, res))
	}
}
