package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// The git side of the composed commands (changed_symbols, churn, hotspots,
// related; docs/DECISIONS.md D40). Like D8's dirty-worktree check it shells out
// to git and nothing else, reads and never writes, and treats "no git" and "not
// a repository" as a smaller answer with a warning, never a failure.

const (
	// gitOutputCap bounds what one git call may hand back. A history query over a
	// large repository can be enormous; a cut answer is reported, not swallowed.
	gitOutputCap = 32 << 20
	// gitHistoryTimeout bounds a query over history (log, a whole-tree diff),
	// which is slower than the status query gitTimeout was sized for.
	gitHistoryTimeout = 30 * time.Second
	// gitBlobCap bounds one file's old content, fetched to be outlined.
	gitBlobCap = 2 << 20
)

// errGitOutputCapped says a git answer was cut at gitOutputCap.
var errGitOutputCapped = errors.New("git's answer was larger than the cap and was cut")

// A gitRepo is a repository, found from the workspace root.
type gitRepo struct {
	git string
	// dir is the workspace root every command runs in: paths git reports are
	// made relative to it (`--relative`), so a workspace that is a subdirectory
	// of the repository sees its own paths, and nothing outside it.
	dir string
	// top is the repository's top-level directory.
	top string
}

// openGitRepo finds the repository that contains dir: errNoGit or errNotARepo,
// wrapped, when there is none.
func openGitRepo(dir string) (*gitRepo, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, errNoGit
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := runGit(ctx, git, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNotARepo, err)
	}
	return &gitRepo{git: git, dir: dir, top: strings.TrimSpace(string(out))}, nil
}

// gitDegraded is what a command reports about git when it has none, or none it
// can use: the reason, in the data and in the warnings.
func gitDegraded(err error) string {
	switch {
	case errors.Is(err, errNoGit):
		return "git is not installed, so history and diffs are unavailable"
	case errors.Is(err, errNotARepo):
		return "the workspace is not inside a git repository, so history and diffs are unavailable"
	}
	return "git is unavailable: " + err.Error()
}

// capWriter is a bounded buffer: at the cap it stops the git process that is
// filling it and remembers that it did.
type capWriter struct {
	buf    bytes.Buffer
	max    int
	over   bool
	cancel context.CancelFunc
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := w.max - w.buf.Len(); len(p) > room {
		w.buf.Write(p[:room])
		w.over = true
		w.cancel()
		return 0, errGitOutputCapped
	}
	return w.buf.Write(p)
}

// run runs `git <args>` in the workspace: no pager, no colour, no quoting of
// paths, no optional index refresh (which would be a write), no prompt, and
// stdout bounded. A cut answer is returned with errGitOutputCapped so that a
// caller can use the prefix and say so.
func (g *gitRepo) run(ctx context.Context, timeout time.Duration, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	full := append([]string{"--no-pager", "-c", "core.quotepath=off", "-c", "color.ui=false", "-c", "diff.external="}, args...)
	cmd := exec.CommandContext(ctx, g.git, full...)
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	stdout := &capWriter{max: gitOutputCap, cancel: cancel}
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = stdout, &stderr
	err := cmd.Run()
	if stdout.over {
		return stdout.buf.Bytes(), errGitOutputCapped
	}
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(oneLine(msg))
	}
	return stdout.buf.Bytes(), nil
}

// validRev rejects what cannot be a revision before it reaches git, where a
// leading dash would be an option.
func validRev(rev string) error {
	if rev == "" || strings.HasPrefix(rev, "-") || strings.ContainsAny(rev, "\x00\n") {
		return render.Errorf(render.CodeUsage, "%q is not a revision", rev)
	}
	return nil
}

// verifyRev resolves rev to a commit, or says it is not one.
func (g *gitRepo) verifyRev(ctx context.Context, rev string) (string, error) {
	out, err := g.run(ctx, gitTimeout, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return "", render.Errorf(render.CodeUsage, "%q is not a commit in this repository", rev)
	}
	return strings.TrimSpace(string(out)), nil
}

// hasHead reports whether the repository has a commit yet.
func (g *gitRepo) hasHead(ctx context.Context) bool {
	_, err := g.run(ctx, gitTimeout, "rev-parse", "--verify", "--quiet", "HEAD^{commit}")
	return err == nil
}

// --- diffs ---

// A hunk is one `@@ -a,b +c,d @@` header of a zero-context diff: b lines from a
// on the old side became d lines from c on the new side. A count of 0 is an
// insertion (old side) or a deletion (new side) *after* the line named.
type hunk struct {
	OldStart int `json:"old_start"`
	OldCount int `json:"old_count"`
	NewStart int `json:"new_start"`
	NewCount int `json:"new_count"`
}

// newRange is the new-side lines the hunk added or replaced, ok false for a
// pure deletion.
func (h hunk) newRange() (lo, hi int, ok bool) {
	if h.NewCount == 0 {
		return 0, 0, false
	}
	return h.NewStart, h.NewStart + h.NewCount - 1, true
}

// oldRange is the old-side lines the hunk removed or replaced, ok false for a
// pure insertion.
func (h hunk) oldRange() (lo, hi int, ok bool) {
	if h.OldCount == 0 {
		return 0, 0, false
	}
	return h.OldStart, h.OldStart + h.OldCount - 1, true
}

// A fileDiff is the change to one file.
type fileDiff struct {
	// OldPath and NewPath are workspace-relative; "" for the side that does not
	// exist (an added file has no old path, a deleted one no new path).
	OldPath, NewPath string
	// Status is A, D, M or R.
	Status string
	Binary bool
	Hunks  []hunk
}

var hunkHeader = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+(\d+)(?:,(\d+))? @@`)

func atoiOr(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

// unquotePath undoes git's C-style quoting of a path that has a quote or a
// control character in it, and drops the tab git appends to a name with a space.
func unquotePath(s string) string {
	s = strings.TrimRight(s, "\t")
	if strings.HasPrefix(s, `"`) {
		if u, err := strconv.Unquote(s); err == nil {
			return u
		}
	}
	return s
}

// parseDiff reads the output of `git diff -U0 --no-prefix -M`. It is tolerant of
// a cut answer: a partial last hunk header is dropped.
func parseDiff(out []byte) []fileDiff {
	var files []fileDiff
	var cur *fileDiff
	flush := func() {
		if cur != nil && (cur.NewPath != "" || cur.OldPath != "") {
			files = append(files, *cur)
		}
		cur = nil
	}
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
			cur = &fileDiff{Status: "M"}
			// With --no-prefix an unrenamed file is "diff --git p p"; a rename
			// says its paths on its own lines.
			s := strings.TrimPrefix(line, "diff --git ")
			if len(s)%2 == 1 && s[:len(s)/2] == s[len(s)/2+1:] {
				cur.OldPath, cur.NewPath = unquotePath(s[:len(s)/2]), unquotePath(s[:len(s)/2])
			}
		case cur == nil:
		case strings.HasPrefix(line, "new file mode"):
			cur.Status = "A"
		case strings.HasPrefix(line, "deleted file mode"):
			cur.Status = "D"
		case strings.HasPrefix(line, "rename from "):
			cur.Status, cur.OldPath = "R", unquotePath(strings.TrimPrefix(line, "rename from "))
		case strings.HasPrefix(line, "rename to "):
			cur.Status, cur.NewPath = "R", unquotePath(strings.TrimPrefix(line, "rename to "))
		case strings.HasPrefix(line, "Binary files "), strings.HasPrefix(line, "GIT binary patch"):
			cur.Binary = true
		case strings.HasPrefix(line, "--- "):
			if p := unquotePath(strings.TrimPrefix(line, "--- ")); p == "/dev/null" {
				cur.OldPath, cur.Status = "", "A"
			} else {
				cur.OldPath = p
			}
		case strings.HasPrefix(line, "+++ "):
			if p := unquotePath(strings.TrimPrefix(line, "+++ ")); p == "/dev/null" {
				cur.NewPath, cur.Status = "", "D"
			} else {
				cur.NewPath = p
			}
		default:
			if m := hunkHeader.FindStringSubmatch(line); m != nil {
				cur.Hunks = append(cur.Hunks, hunk{
					OldStart: atoiOr(m[1], 0), OldCount: atoiOr(m[2], 1),
					NewStart: atoiOr(m[3], 0), NewCount: atoiOr(m[4], 1),
				})
			}
		}
	}
	flush()
	return files
}

// --- history ---

// logHeader is what a commit record starts with; see parseLog.
const logFormat = "--format=%x01%H%x1f%aN%x1f%aI%x02"

// A commit is one commit of a log query.
type commit struct {
	SHA, Author, Date string
	// Files are the numstat rows (churn) or the bare names (co-change: Added
	// and Removed are then -1).
	Files []commitFile
	// Diff is the patch text of a `-p` query.
	Diff []byte
}

type commitFile struct {
	Path           string
	Added, Removed int
	Binary         bool
}

// parseLog reads `git log -z <logFormat> --numstat|--name-only` output (with -z
// each record is NUL-terminated and paths are unmunged), or the same without -z
// and with -p (Diff holds the patch). numstat true reads `added\tremoved\tpath`
// records, false bare names.
func parseLog(out []byte, numstat, patch bool) []commit {
	var commits []commit
	for _, chunk := range bytes.Split(out, []byte{0x01}) {
		head, rest, ok := bytes.Cut(chunk, []byte{0x02})
		if !ok {
			continue
		}
		f := strings.Split(string(head), "\x1f")
		if len(f) != 3 {
			continue
		}
		c := commit{SHA: f[0], Author: f[1], Date: f[2]}
		if patch {
			c.Diff = rest
			commits = append(commits, c)
			continue
		}
		for _, rec := range bytes.Split(rest, []byte{0}) {
			rec = bytes.TrimLeft(rec, "\n")
			if len(rec) == 0 {
				continue
			}
			if !numstat {
				c.Files = append(c.Files, commitFile{Path: string(rec), Added: -1, Removed: -1})
				continue
			}
			parts := strings.SplitN(string(rec), "\t", 3)
			if len(parts) != 3 {
				continue
			}
			cf := commitFile{Path: parts[2]}
			if parts[0] == "-" || parts[1] == "-" {
				cf.Binary = true
			} else {
				cf.Added, cf.Removed = atoiOr(parts[0], 0), atoiOr(parts[1], 0)
			}
			c.Files = append(c.Files, cf)
		}
		commits = append(commits, c)
	}
	return commits
}

// validSince rejects a --since that is empty or could be read as an option.
func validSince(since string) error {
	if strings.TrimSpace(since) == "" || strings.HasPrefix(since, "-") || strings.ContainsAny(since, "\x00\n") {
		return render.Errorf(render.CodeUsage, "--since %q is not a date git can read; try 30d, '30 days', '2 weeks ago' or 2026-01-31", since)
	}
	return nil
}

// shortSince is the compact window every developer writes, `30d`, `2w`, `12h`,
// `6m`, `1y`, which git's date parser does not read: it takes `--since=30d` to
// be no date at all and answers with an empty log, which would be reported as
// "no commit in the window".
var shortSince = regexp.MustCompile(`^(\d+)\s*([hdwmy])$`)

// gitSince turns a --since value into what `git log --since=` reads: the
// compact forms become "N units ago", anything else is git's to read.
func gitSince(since string) string {
	m := shortSince.FindStringSubmatch(strings.ToLower(strings.TrimSpace(since)))
	if m == nil {
		return since
	}
	unit := map[string]string{"h": "hours", "d": "days", "w": "weeks", "m": "months", "y": "years"}[m[2]]
	return m[1] + " " + unit + " ago"
}

// --- mapping lines through later history ---

// A shifter maps a line number as it was after some commit onto the line it is
// now, through the zero-context hunks of every later commit and of the
// uncommitted changes. Layers are pushed newest first (the worktree diff, then
// HEAD's hunks, then the one before), which is the order `git log` walks; a
// coordinate is carried through them oldest to newest.
type shifter struct {
	layers [][]hunk
}

func (s *shifter) push(hs []hunk) { s.layers = append(s.layers, hs) }

// carry maps n, a line after the most recent pushed layer's *old* side, to now.
// lost reports that the line was itself replaced or removed by a later change,
// in which case the position returned is where its replacement starts.
func (s *shifter) carry(n int) (int, bool) {
	lost := false
	for i := len(s.layers) - 1; i >= 0; i-- {
		var l bool
		n, l = shiftLine(n, s.layers[i])
		lost = lost || l
	}
	return n, lost
}

// shiftLine maps an old-side line through one commit's hunks (sorted by old
// position, as git emits them) to the new side.
func shiftLine(n int, hunks []hunk) (int, bool) {
	shift := 0
	for _, h := range hunks {
		if h.OldCount > 0 {
			switch {
			case n < h.OldStart:
				return n + shift, false
			case n < h.OldStart+h.OldCount:
				return h.NewStart, true
			}
		} else if n <= h.OldStart {
			return n + shift, false
		}
		shift += h.NewCount - h.OldCount
	}
	return n + shift, false
}
