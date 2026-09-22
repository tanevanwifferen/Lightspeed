package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The git-aware commands against real git repositories built in temporary
// directories and the scripted server, whose outline (with
// LIGHTSPEED_TEST_OUTLINE_SPANS) gives each function its body as its range.

func gitIn(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	cmd := exec.Command(git, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// commitAs commits everything as author, dated daysAgo days ago.
func commitAs(t *testing.T, dir, author string, daysAgo int, msg string) {
	t.Helper()
	date := time.Now().AddDate(0, 0, -daysAgo).UTC().Format(time.RFC3339)
	env := []string{
		"GIT_AUTHOR_NAME=" + author, "GIT_AUTHOR_EMAIL=" + author + "@example.invalid", "GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_NAME=" + author, "GIT_COMMITTER_EMAIL=" + author + "@example.invalid", "GIT_COMMITTER_DATE=" + date,
	}
	gitIn(t, dir, env, "add", "-A")
	gitIn(t, dir, env, "-c", "commit.gpgsign=false", "commit", "-qm", msg)
}

func writeAll(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		write(t, filepath.Join(dir, filepath.FromSlash(name)), content)
	}
}

// The history of the fixture: three commits by two authors, the first outside a
// 30-day window. HEAD's a.go is aHead.
const (
	aV1 = "package a\n\nfunc Alpha() {\n\tprintln(\"a\")\n}\n\nfunc Beta() {\n\tprintln(\"b\")\n}\n\nfunc Gamma() {\n\tprintln(\"g\")\n}\n"
	// commit 2 changes Gamma's body; commit 3 adds a two-line header (which moves
	// everything down by two) and changes Beta's body.
	aV2   = "package a\n\nfunc Alpha() {\n\tprintln(\"a\")\n}\n\nfunc Beta() {\n\tprintln(\"b\")\n}\n\nfunc Gamma() {\n\tprintln(\"g2\")\n}\n"
	aHead = "package a\n// Package a.\n// More.\n\nfunc Alpha() {\n\tprintln(\"a\")\n}\n\nfunc Beta() {\n\tprintln(\"b3\")\n}\n\nfunc Gamma() {\n\tprintln(\"g2\")\n}\n"
	bV1   = "package a\n\nfunc Bee() {\n\tprintln(\"bee\")\n}\n"
	bV2   = "package a\n\nfunc Bee() {\n\tprintln(\"bee2\")\n}\n"
	bV3   = "package a\n\nfunc Bee() {\n\tprintln(\"bee3\")\n}\n"
	help  = "package a\n\nfunc BetaHelper() {\n}\n"
)

// gitWorkspace builds the fixture repository, works in it and points the CLI at
// the scripted server. Beta is at a.go:9.
func gitWorkspace(t *testing.T) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	dir := tree(t, nil)
	gitIn(t, dir, nil, "init", "-q")
	writeAll(t, dir, map[string]string{"a.go": aV1, "b.go": bV1, "helper.go": help})
	commitAs(t, dir, "Ann", 400, "one")
	writeAll(t, dir, map[string]string{"a.go": aV2, "b.go": bV2})
	commitAs(t, dir, "Bob", 10, "two")
	writeAll(t, dir, map[string]string{"a.go": aHead, "b.go": bV3})
	commitAs(t, dir, "Ann", 5, "three")
	t.Chdir(dir)
	scenario{textOutline: true, capabilities: m5Capabilities(nil)}.apply(t)
	t.Setenv(outlineSpansEnv, "1")
	return dir
}

// runData runs a command and decodes its envelope's data.
func runData[T any](t *testing.T, args ...string) (T, rawEnvelope, int) {
	t.Helper()
	code, stdout, stderr := runMain(args...)
	var v T
	env := decodeData(t, stdout, &v)
	if !env.OK && code == ExitOK {
		t.Fatalf("%v: ok=false but exit 0\n%s\n%s", args, stdout, stderr)
	}
	return v, env, code
}

func rowsByID(rows []changedRow) map[string]changedRow {
	out := map[string]changedRow{}
	for _, r := range rows {
		k := r.ID
		if k == "" {
			k = "(file)" + r.File
		}
		out[k] = r
	}
	return out
}

func TestChangedSymbolsMapsHunksOntoSymbols(t *testing.T) {
	dir := gitWorkspace(t)

	// A clean tree changed nothing: an authoritative empty answer.
	d, _, code := runData[changedData](t, "changed_symbols")
	if code != ExitProblems || len(d.Rows) != 0 || !d.Git.Repo || d.Git.Base != "HEAD" {
		t.Fatalf("clean tree: exit %d, %+v", code, d)
	}

	// Alpha's body edited; Gamma removed; Delta added; a header line added outside
	// every declaration; Beta untouched.
	write(t, filepath.Join(dir, "a.go"),
		"package a\n// Package a.\n// More.\n// Added header.\n\nfunc Alpha() {\n\tprintln(\"a-edit\")\n}\n\nfunc Beta() {\n\tprintln(\"b3\")\n}\n\nfunc Delta() {\n\tprintln(\"d\")\n}\n")
	d, env, code := runData[changedData](t, "changed_symbols")
	if code != ExitOK {
		t.Fatalf("exit %d\n%+v", code, env)
	}
	got := rowsByID(d.Rows)
	for id, status := range map[string]string{
		"a.go::Alpha#function": "modified", "a.go::Gamma#function": "removed", "a.go::Delta#function": "added",
	} {
		if r, ok := got[id]; !ok || r.Status != status || r.Kind != "symbol" {
			t.Errorf("%s: %+v (want %s)\nall rows: %+v", id, r, status, d.Rows)
		}
	}
	if r := got["a.go::Gamma#function"]; r.OldLine != 13 || r.OldFile != "a.go" || r.Evidence != "index+old_outline" {
		t.Errorf("a removed symbol carries where it was and how it was found: %+v", r)
	}
	if r := got["a.go::Alpha#function"]; r.Line != 6 || r.EndLine != 8 || len(r.Hunks) == 0 {
		t.Errorf("Alpha = %+v", r)
	}
	if _, ok := got["a.go::Beta#function"]; ok {
		t.Errorf("Beta did not change: %+v", got["a.go::Beta#function"])
	}
	if f, ok := got["(file)a.go"]; !ok || f.Kind != "file" || f.Status != "modified" {
		t.Errorf("the header hunk belongs to no symbol and is a file-level row: %+v", d.Rows)
	}
	if d.Summary.Added != 1 || d.Summary.Modified != 1 || d.Summary.Removed != 1 || d.Summary.FileLevel != 1 || d.Summary.OldOutlined != 1 {
		t.Errorf("summary = %+v", d.Summary)
	}
	if d.Truncated || d.Total != len(d.Rows) {
		t.Errorf("truncation = %v/%d/%d", d.Truncated, d.Total, len(d.Rows))
	}

	// The text form is one grep-style line per row.
	_, out, _ := runMain("changed_symbols", "--format", "text")
	if !strings.Contains(out, "a.go:6: modified a.go::Alpha#function") {
		t.Errorf("text output:\n%s", out)
	}
}

func TestChangedSymbolsStagedBaseAndUntracked(t *testing.T) {
	dir := gitWorkspace(t)

	// Staged: a.go's Alpha. Unstaged: b.go's Bee. Untracked: c.go.
	write(t, filepath.Join(dir, "a.go"), strings.Replace(aHead, `println("a")`, `println("a-staged")`, 1))
	gitIn(t, dir, nil, "add", "a.go")
	write(t, filepath.Join(dir, "b.go"), strings.Replace(bV3, "bee3", "bee-unstaged", 1))
	write(t, filepath.Join(dir, "c.go"), "package a\n\nfunc One() {\n}\n\nfunc Two() {\n}\n")

	d, _, _ := runData[changedData](t, "changed_symbols", "--staged")
	got := rowsByID(d.Rows)
	if len(got) != 1 || got["a.go::Alpha#function"].Status != "modified" || !d.Git.Staged {
		t.Errorf("--staged should see only the staged Alpha: %+v", d.Rows)
	}

	d, _, _ = runData[changedData](t, "changed_symbols")
	got = rowsByID(d.Rows)
	for id, status := range map[string]string{
		"a.go::Alpha#function": "modified", "b.go::Bee#function": "modified",
		"c.go::One#function": "added", "c.go::Two#function": "added",
	} {
		if r, ok := got[id]; !ok || r.Status != status {
			t.Errorf("%s: %+v, want %s\nrows: %+v", id, r, status, d.Rows)
		}
	}

	// --base HEAD~1: what commit three did (Beta, Bee and the header) as well.
	d, _, _ = runData[changedData](t, "changed_symbols", "--base", "HEAD~1", "--path", "a.go")
	got = rowsByID(d.Rows)
	if got["a.go::Beta#function"].Status != "modified" || got["a.go::Alpha#function"].Status != "modified" {
		t.Errorf("--base HEAD~1: %+v", d.Rows)
	}
	if d.Git.Base != "HEAD~1" || len(d.Git.BaseSHA) < 40 {
		t.Errorf("git = %+v", d.Git)
	}
	for _, r := range d.Rows {
		if r.File != "a.go" {
			t.Errorf("--path a.go returned %+v", r)
		}
	}

	// A bad revision is a usage error, not an empty answer.
	code, stdout, _ := runMain("changed_symbols", "--base", "no-such-rev")
	if code != ExitUsage {
		t.Errorf("bad --base: exit %d\n%s", code, stdout)
	}
	if code, _, _ := runMain("changed_symbols", "--base", "--output=x"); code != ExitUsage {
		t.Errorf("an option-looking --base must be refused, exit %d", code)
	}
}

func TestChangedSymbolsWithoutAnOldOutlineSaysSo(t *testing.T) {
	dir := gitWorkspace(t)
	// A language no server claims: only hunks, and the answer says why.
	write(t, filepath.Join(dir, "notes.md"), "one\n")
	gitIn(t, dir, nil, "add", "-A")
	commitAs(t, dir, "Ann", 1, "notes")
	write(t, filepath.Join(dir, "notes.md"), "one\ntwo\n")
	// Delete Gamma from a.go too, but stop the old-side outline by making the
	// server unavailable for it: --server names a server nothing defines.
	write(t, filepath.Join(dir, "a.go"), strings.Replace(aHead, "func Gamma() {\n\tprintln(\"g2\")\n}\n", "", 1))

	d, env, _ := runData[changedData](t, "changed_symbols", "--server", "nonesuch")
	if d.Summary.OldOutlined != 0 || d.Summary.OldUnavailable == 0 {
		t.Errorf("summary = %+v", d.Summary)
	}
	if !hasWarning(env.Warnings, "old version") || !hasWarning(env.Warnings, "removed from them is not listed") {
		t.Errorf("hunk-only must be a warning: %v", env.Warnings)
	}
	for _, r := range d.Rows {
		if r.Status == "removed" && r.Kind == "symbol" {
			t.Errorf("a removed symbol named without an old outline: %+v", r)
		}
		if r.Kind == "symbol" && r.Evidence != "index+hunks" {
			t.Errorf("evidence = %q", r.Evidence)
		}
	}
	md := rowsByID(d.Rows)["(file)notes.md"]
	if md.Kind != "file" || md.Status != "modified" || len(md.Hunks) == 0 {
		t.Errorf("notes.md = %+v", md)
	}
}

func TestGitCommandsOutsideARepositoryDegradeWithAWarning(t *testing.T) {
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	dir := tree(t, map[string]string{"a.go": aV1})
	t.Chdir(dir)
	scenario{textOutline: true, capabilities: m5Capabilities(nil)}.apply(t)

	for _, cmd := range []string{"changed_symbols", "churn", "hotspots"} {
		code, stdout, stderr := runMain(cmd)
		if code != ExitOK {
			t.Errorf("%s: exit %d\n%s\n%s", cmd, code, stdout, stderr)
			continue
		}
		var d struct {
			Git changedGit `json:"git"`
		}
		env := decodeData(t, stdout, &d)
		if !env.OK || d.Git.Repo || !strings.Contains(d.Git.Reason, "not inside a git repository") || !hasWarning(env.Warnings, "not inside a git repository") {
			t.Errorf("%s: %s", cmd, stdout)
		}
	}

	// related still answers, from the other three kinds of evidence, and says the
	// fourth is missing.
	d, env, code := runData[relatedData](t, "related", "a.go:7:6")
	if code != ExitOK || !hasWarning(env.Warnings, "co-change evidence unavailable") {
		t.Fatalf("related: exit %d %v", code, env.Warnings)
	}
	if len(d.Rows) == 0 || !relatedHas(d.Rows, evSibling) {
		t.Errorf("related outside a repository still has siblings: %+v", d.Rows)
	}
}

func TestChurnPerFileAndPerSymbol(t *testing.T) {
	gitWorkspace(t)

	d, env, code := runData[churnData](t, "churn", "--since", "30 days")
	if code != ExitOK {
		t.Fatalf("exit %d %v", code, env.Warnings)
	}
	if d.Commits != 2 || d.Since != "30 days" || !d.Git.Repo {
		t.Errorf("window: %+v", d)
	}
	files := map[string]churnFile{}
	for _, f := range d.Files {
		files[f.File] = f
	}
	if _, in := files["helper.go"]; in {
		t.Errorf("helper.go was only in the commit outside the window: %+v", d.Files)
	}
	a, b := files["a.go"], files["b.go"]
	if a.Commits != 2 || a.Authors != 2 || a.Added != 4 || a.Removed != 2 {
		t.Errorf("a.go = %+v", a) // g→g2 and b→b3 replace a line each (+1 -1 each); the header adds two: +4 -2
	}
	if b.Commits != 2 || b.Authors != 2 || b.Added != 2 || b.Removed != 2 {
		t.Errorf("b.go = %+v", b)
	}
	if len(d.Files) < 2 || d.Files[0].Commits < d.Files[1].Commits {
		t.Errorf("sorted by commits: %+v", d.Files)
	}

	// Per symbol, with the lines carried through the header that moved everything.
	syms := map[string]churnSymbol{}
	for _, s := range d.Symbols {
		syms[s.ID] = s
	}
	if g := syms["a.go::Gamma#function"]; g.Commits != 1 || g.Authors != 1 {
		t.Errorf("Gamma was changed once, by Bob, before the header moved it: %+v (all: %+v)", g, d.Symbols)
	}
	if be := syms["a.go::Beta#function"]; be.Commits != 1 {
		t.Errorf("Beta = %+v", be)
	}
	if _, in := syms["a.go::Alpha#function"]; in {
		t.Errorf("Alpha never changed in the window: %+v", syms["a.go::Alpha#function"])
	}
	if bee := syms["b.go::Bee#function"]; bee.Commits != 2 || bee.Authors != 2 {
		t.Errorf("Bee = %+v", bee)
	}
	if !strings.Contains(d.Approximate, "approximations") {
		t.Errorf("per-symbol figures must say what they are: %q", d.Approximate)
	}

	// Files only, restricted to one path.
	d, _, _ = runData[churnData](t, "churn", "b.go", "--since", "30 days", "--symbol-files", "0")
	if len(d.Files) != 1 || d.Files[0].File != "b.go" || len(d.Symbols) != 0 || d.Path != "b.go" {
		t.Errorf("churn b.go = %+v", d)
	}

	// The window really is a window: a year out, the first commit is in.
	d, _, _ = runData[churnData](t, "churn", "--since", "2 years ago", "--symbol-files", "0")
	if d.Commits != 3 {
		t.Errorf("commits = %d", d.Commits)
	}
	// And nothing in it is an empty answer.
	if code, _, _ := runMain("churn", "--since", "1 minute ago"); code != ExitProblems {
		t.Errorf("an empty window: exit %d", code)
	}
	if code, _, _ := runMain("churn", "--since", "--all"); code != ExitUsage {
		t.Errorf("an option-looking --since must be refused, exit %d", code)
	}
}

func TestHotspotsRanksChurnTimesSize(t *testing.T) {
	dir := gitWorkspace(t)
	// A big file changed once, and a small one changed as often as a.go: with the
	// formula, a.go (14 lines, 2 commits) outranks the small one (5 lines, 2 commits)
	// and the big one's single commit ranks by its size.
	var big strings.Builder
	big.WriteString("package a\n")
	for i := 0; i < 300; i++ {
		big.WriteString("// filler\n")
	}
	writeAll(t, dir, map[string]string{"big.go": big.String()})
	commitAs(t, dir, "Ann", 2, "big")

	d, env, code := runData[churnData](t, "hotspots", "--since", "30 days")
	if code != ExitOK {
		t.Fatalf("exit %d %v", code, env.Warnings)
	}
	if !strings.Contains(d.Formula, "commits × (1 + ln(1 + loc))") {
		t.Errorf("the formula must be in the answer: %q", d.Formula)
	}
	var order []string
	scores := map[string]float64{}
	for _, f := range d.Files {
		order = append(order, f.File)
		scores[f.File] = f.Score
		if f.LOC == 0 {
			t.Errorf("hotspot without a size: %+v", f)
		}
	}
	if !slices.Equal(order[:3], []string{"big.go", "a.go", "b.go"}) {
		// big: 1 × (1+ln 302)=6.7; a.go: 2 × (1+ln 16)=7.5 — a.go first.
		if !slices.Equal(order[:3], []string{"a.go", "big.go", "b.go"}) {
			t.Errorf("order = %v (%v)", order, scores)
		}
	}
	if scores["a.go"] <= scores["b.go"] {
		t.Errorf("the same churn on a bigger file scores higher: %v", scores)
	}
	for _, f := range d.Files {
		if f.File == "a.go" && f.Symbols != 3 {
			t.Errorf("a.go's symbol count = %d", f.Symbols)
		}
	}
}

// A decision log changes with every commit and is long: churn × size puts it
// above every source file, which is noise in a ranking of code to look at. Docs,
// data and config are left out unless --all, and the answer says how many.
func TestHotspotsRanksCodeNotDocsUnlessAll(t *testing.T) {
	dir := gitWorkspace(t)
	doc := strings.Repeat("A line of prose.\n", 400)
	writeAll(t, dir, map[string]string{"DECISIONS.md": doc, "config.yaml": "a: 1\n", "LICENSE": "free\n"})
	commitAs(t, dir, "Ann", 3, "docs")
	writeAll(t, dir, map[string]string{"DECISIONS.md": doc + "More.\n"})
	commitAs(t, dir, "Ann", 2, "more docs")

	d, env, code := runData[churnData](t, "hotspots", "--since", "30 days", "--symbol-files", "0")
	if code != ExitOK {
		t.Fatalf("exit %d %v", code, env.Warnings)
	}
	for _, f := range d.Files {
		if !strings.HasSuffix(f.File, ".go") {
			t.Errorf("hotspots ranks %s, which is not code", f.File)
		}
	}
	if len(d.Files) == 0 || d.Files[0].File != "a.go" || d.NonCode != 3 || d.Total != 2 {
		t.Errorf("files = %+v, left out %d, total %d; want a.go first, 3 left out of the 5", d.Files, d.NonCode, d.Total)
	}
	if !slices.ContainsFunc(env.Warnings, func(w string) bool {
		return strings.Contains(w, "3 changed files that are not code were left out") && strings.Contains(w, "DECISIONS.md") && strings.Contains(w, "--all")
	}) {
		t.Errorf("what was left out must be said: %v", env.Warnings)
	}

	all, _, _ := runData[churnData](t, "hotspots", "--since", "30 days", "--symbol-files", "0", "--all")
	if len(all.Files) != 5 || all.Files[0].File != "DECISIONS.md" || all.NonCode != 0 {
		t.Errorf("--all ranks every file: %+v", all.Files)
	}
	// Only docs in scope: an empty ranking that says why, not a silent one.
	only, oenv, ocode := runData[churnData](t, "hotspots", "--since", "30 days", "--path", "DECISIONS.md")
	if ocode != ExitProblems || len(only.Files) != 0 || only.NonCode != 1 || len(oenv.Warnings) == 0 {
		t.Errorf("exit %d, files %+v, warnings %v", ocode, only.Files, oenv.Warnings)
	}
	// churn is history, not a ranking of code: it keeps every file.
	ch, _, _ := runData[churnData](t, "churn", "--since", "30 days", "--symbol-files", "0")
	if !slices.ContainsFunc(ch.Files, func(f churnFile) bool { return f.File == "DECISIONS.md" }) {
		t.Errorf("churn dropped the docs: %+v", ch.Files)
	}
}

func relatedHas(rows []relatedRow, evidence string) bool {
	for _, r := range rows {
		if slices.Contains(r.Evidence, evidence) {
			return true
		}
	}
	return false
}

func relatedFixture(t *testing.T) string {
	t.Helper()
	dir := gitWorkspace(t)
	file := filepath.Join(dir, "a.go")
	scenario{
		textOutline:  true,
		capabilities: m5Capabilities(nil),
		results:      map[string]any{methodPrepareCallHierarchy: []any{callItemJSON("Beta", 12, file, 8, 5, 9)}},
		calls: map[string]any{
			"Beta": map[string]any{
				"incoming": []any{incomingCall(callItemJSON("Alpha", 12, file, 4, 5, 10), callRange(5, 1, 6))},
				"outgoing": []any{outgoingCall(callItemJSON("Gamma", 12, file, 12, 5, 10), callRange(9, 1, 6))},
			},
		},
	}.apply(t)
	t.Setenv(outlineSpansEnv, "1")
	return dir
}

func TestRelatedCombinesFourKindsOfEvidence(t *testing.T) {
	relatedFixture(t)
	d, env, code := runData[relatedData](t, "related", "a.go:9:6")
	if code != ExitOK {
		t.Fatalf("exit %d\n%v", code, env.Warnings)
	}
	if d.Subject.ID != "a.go::Beta#function" || d.Subject.Line != 9 {
		t.Errorf("subject = %+v", d.Subject)
	}
	byID := map[string]relatedRow{}
	for _, r := range d.Rows {
		k := r.ID
		if k == "" {
			k = r.File
		}
		byID[k] = r
	}
	// Alpha calls Beta and is next to it: two kinds of evidence, one row.
	alpha := byID["a.go::Alpha#function"]
	if !slices.Contains(alpha.Evidence, evCall) || !slices.Contains(alpha.Evidence, evSibling) {
		t.Errorf("Alpha = %+v", alpha)
	}
	gamma := byID["a.go::Gamma#function"]
	if !slices.Contains(gamma.Evidence, evCall) || !slices.Contains(gamma.Evidence, evSibling) || !strings.Contains(gamma.Detail, "callee") {
		t.Errorf("Gamma = %+v", gamma)
	}
	if b := byID["b.go"]; b.Kind != "file" || !slices.Equal(b.Evidence, []string{evCoChange}) || !strings.Contains(b.Detail, "3 of the last 3") {
		t.Errorf("b.go = %+v (rows: %+v)", b, d.Rows)
	}
	if h := byID["helper.go::BetaHelper#function"]; !slices.Equal(h.Evidence, []string{evSimilar}) {
		t.Errorf("BetaHelper = %+v", h)
	}
	if _, in := byID["a.go::Beta#function"]; in {
		t.Errorf("the subject is not related to itself")
	}
	// Ranked: what calls it comes before what merely has a similar name.
	rank := map[string]int{}
	for i, r := range d.Rows {
		k := r.ID
		if k == "" {
			k = r.File
		}
		rank[k] = i
	}
	if rank["a.go::Alpha#function"] > rank["helper.go::BetaHelper#function"] {
		t.Errorf("order = %+v", d.Rows)
	}
	kinds := map[string]bool{}
	for _, s := range d.Sources {
		kinds[s.Evidence] = true
	}
	if len(kinds) != 4 || d.History != 3 {
		t.Errorf("sources = %+v history = %d", d.Sources, d.History)
	}

	// --symbol and --id name the subject as everywhere else.
	d2, _, code := runData[relatedData](t, "related", "--id", "a.go::Beta#function")
	if code != ExitOK || d2.Subject.ID != "a.go::Beta#function" {
		t.Errorf("--id: exit %d, %+v", code, d2.Subject)
	}

	// Bounded and honest about it.
	d3, env3, _ := runData[relatedData](t, "related", "a.go:9:6", "--limit", "2")
	if !d3.Truncated || len(d3.Rows) != 2 || d3.Total <= 2 || !hasWarning(env3.Warnings, "listed 2 of") {
		t.Errorf("--limit 2: %+v %v", d3, env3.Warnings)
	}
}

func TestRelatedWithoutCallHierarchyNamesTheGap(t *testing.T) {
	gitWorkspace(t) // its server advertises no callHierarchyProvider once we drop it
	scenario{textOutline: true, capabilities: m5Capabilities(map[string]any{"callHierarchyProvider": nil})}.apply(t)
	t.Setenv(outlineSpansEnv, "1")
	d, env, code := runData[relatedData](t, "related", "a.go:9:6")
	if code != ExitOK || !hasWarning(env.Warnings, "callers and callees unavailable") {
		t.Fatalf("exit %d %v", code, env.Warnings)
	}
	if relatedHas(d.Rows, evCall) {
		t.Errorf("no call evidence without a call hierarchy: %+v", d.Rows)
	}
	for _, s := range d.Sources {
		if s.Evidence == evCall && s.Note == "" {
			t.Errorf("the missing source must say why: %+v", s)
		}
	}
	if !relatedHas(d.Rows, evSibling) || !relatedHas(d.Rows, evCoChange) {
		t.Errorf("the other evidence stands: %+v", d.Rows)
	}
}

func TestGitToolsAreMCPTools(t *testing.T) {
	dir := relatedFixture(t)
	cs := mcpSession(t, dir)
	tools, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]bool{}
	for _, tool := range tools.Tools {
		have[tool.Name] = true
	}
	for _, name := range []string{"changed_symbols", "churn", "hotspots", "related"} {
		if !have[name] {
			t.Errorf("tool %s is missing", name)
		}
	}

	write(t, filepath.Join(dir, "a.go"), strings.Replace(aHead, `println("a")`, `println("mcp")`, 1))
	res := callTool(t, cs, "changed_symbols", map[string]any{})
	if res.IsError {
		t.Fatalf("changed_symbols: %s", resultText(t, res))
	}
	var env struct {
		Data changedData `json:"data"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &env); err != nil {
		t.Fatal(err)
	}
	if got := rowsByID(env.Data.Rows); got["a.go::Alpha#function"].Status != "modified" {
		t.Errorf("rows = %+v", env.Data.Rows)
	}

	res = callTool(t, cs, "related", map[string]any{"location": "a.go:9:6", "limit": 3})
	if res.IsError {
		t.Fatalf("related: %s", resultText(t, res))
	}
	var rel struct {
		Data relatedData `json:"data"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &rel); err != nil || len(rel.Data.Rows) != 3 || !rel.Data.Truncated {
		t.Errorf("related over MCP: %v %+v", err, rel.Data)
	}

	res = callTool(t, cs, "churn", map[string]any{"since": "30 days", "symbol_files": 0})
	res2 := callTool(t, cs, "hotspots", map[string]any{"since": "30 days", "path": "a.go"})
	if res.IsError || res2.IsError {
		t.Errorf("churn/hotspots over MCP: %s / %s", resultText(t, res), resultText(t, res2))
	}
}

// --- --limit ---

func init() {
	registerLimitCase("changed_symbols", limitCase{Rows: "rows", Invoke: func(t *testing.T, extra ...string) string {
		dir := gitWorkspace(t)
		write(t, filepath.Join(dir, "c.go"), "package a\n\nfunc One() {\n}\n\nfunc Two() {\n}\n\nfunc Three() {\n}\n\nfunc Four() {\n}\n")
		_, out, _ := runMain(append([]string{"changed_symbols"}, extra...)...)
		return out
	}})
	registerLimitCase("churn", limitCase{Rows: "files", Invoke: func(t *testing.T, extra ...string) string {
		dir := gitWorkspace(t)
		writeAll(t, dir, map[string]string{"d.go": "package a\n", "e.go": "package a\n"})
		commitAs(t, dir, "Ann", 1, "more")
		_, out, _ := runMain(append([]string{"churn", "--since", "2 years ago", "--symbol-files", "0"}, extra...)...)
		return out
	}})
	registerLimitCase("hotspots", limitCase{Rows: "files", Invoke: func(t *testing.T, extra ...string) string {
		dir := gitWorkspace(t)
		writeAll(t, dir, map[string]string{"d.go": "package a\n", "e.go": "package a\n"})
		commitAs(t, dir, "Ann", 1, "more")
		_, out, _ := runMain(append([]string{"hotspots", "--since", "2 years ago", "--symbol-files", "0"}, extra...)...)
		return out
	}})
	registerLimitCase("related", limitCase{Rows: "rows", Invoke: func(t *testing.T, extra ...string) string {
		relatedFixture(t)
		_, out, _ := runMain(append([]string{"related", "a.go:9:6"}, extra...)...)
		return out
	}})
}

// `30d` is what people type and git's date parser does not read it: passed
// through, the log is empty and the answer "no commit in the window" is false.
func TestChurnCompactSinceFormsMatchTheLongForm(t *testing.T) {
	gitWorkspace(t)
	want, _, _ := runData[churnData](t, "churn", "--since", "30 days", "--symbol-files", "0")
	if want.Commits == 0 {
		t.Fatalf("fixture has no commit in the window: %+v", want)
	}
	for _, since := range []string{"30d", "4w", "720h", "1m", "1y", "30D", "30 d"} {
		got, env, code := runData[churnData](t, "churn", "--since", since, "--symbol-files", "0")
		if code != ExitOK || got.Since != since || len(got.Files) == 0 || got.Commits < want.Commits {
			t.Errorf("--since %s: exit %d, %d commits, %d files (want at least %d commits); warnings %v", since, code, got.Commits, len(got.Files), want.Commits, env.Warnings)
		}
	}
	hot, _, code := runData[churnData](t, "hotspots", "--since", "30d")
	if code != ExitOK || len(hot.Files) == 0 {
		t.Errorf("hotspots --since 30d: exit %d, %+v", code, hot)
	}
}

func TestGitSince(t *testing.T) {
	for in, want := range map[string]string{
		"30d": "30 days ago", "2w": "2 weeks ago", "12h": "12 hours ago", "6m": "6 months ago", "1y": "1 years ago",
		"30 days": "30 days", "2 weeks ago": "2 weeks ago", "2026-01-31": "2026-01-31", "2.weeks": "2.weeks",
	} {
		if got := gitSince(in); got != want {
			t.Errorf("gitSince(%q) = %q, want %q", in, got, want)
		}
	}
}
