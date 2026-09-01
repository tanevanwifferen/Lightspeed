package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// PLAN §9.4, decided: --apply refuses to write into a git worktree
// that has uncommitted changes, unless --allow-dirty says otherwise.
//
// The reason is narrow and practical. An agent's only reliable undo is
// `git checkout`, and that undo is only usable if everything the
// command finds in the worktree afterwards was put there by the
// command. Mixed with the agent's own uncommitted work, `git checkout`
// stops being an undo and becomes a second, larger mistake — so the
// safe state has to exist *before* lightspeed writes, not after.
//
// Only tracked modifications count. Untracked files survive
// `git checkout`, so their presence does not compromise the undo, and
// refusing over a stray build artefact would train callers to pass
// --allow-dirty reflexively, which costs the check its whole value.
//
// Everything that is not a clean answer — no git, no repository, a git
// that refuses to talk — is a warning in the envelope and not a
// refusal. lightspeed is not a git tool, and being unusable outside a
// repository would be a worse failure than the one this prevents.

// gitTimeout bounds a status query. git is local and fast; a git that
// is not is a git that is waiting on a lock we should not wait for.
const gitTimeout = 10 * time.Second

// dirtyPathLimit is how many changed paths the refusal names before it
// summarises. The full list is in the error's details.
const dirtyPathLimit = 10

var (
	// errNoGit reports that git is not installed.
	errNoGit = errors.New("git is not on PATH")
	// errNotARepo reports that the path is not inside a repository.
	errNotARepo = errors.New("not inside a git repository")
)

// worktreeStatus reports the repository root containing dir and the
// tracked paths in it that differ from HEAD or the index.
func worktreeStatus(dir string) (root string, dirty []string, err error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", nil, errNoGit
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	out, err := runGit(ctx, git, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		// Not a repository, an unreadable one, or one git declines to
		// touch (a "dubious ownership" refusal looks the same). All
		// three mean the same thing to us: there is no undo to
		// protect, so say so and let the caller decide.
		return "", nil, fmt.Errorf("%w: %v", errNotARepo, err)
	}
	root = strings.TrimSpace(string(out))

	out, err = runGit(ctx, git, dir, "status", "--porcelain", "-z", "--untracked-files=no")
	if err != nil {
		return root, nil, fmt.Errorf("git status: %v", err)
	}
	return root, porcelainPaths(out), nil
}

// runGit runs one git subcommand in dir and returns its stdout.
func runGit(ctx context.Context, git, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, git, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, errors.New(oneLine(msg))
	}
	return stdout.Bytes(), nil
}

// porcelainPaths extracts the changed paths from `git status
// --porcelain -z`.
//
// With -z every entry is its own NUL-terminated field, and a rename or
// copy is followed by a second field holding the source path. Reading
// that second field as another entry would report a path that is not
// what changed, so the parser consumes it deliberately.
func porcelainPaths(out []byte) []string {
	fields := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	var paths []string
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue // "XY path" is at least four bytes
		}
		paths = append(paths, entry[3:])
		if status := entry[0]; status == 'R' || status == 'C' {
			i++ // the rename/copy source, not a change of its own
		}
	}
	return paths
}

// checkWorktrees enforces the rule above for the files a mutation is
// about to write. It is called before a language server is started, so
// that a refusal costs nothing but a `git status`.
//
// The returned warnings belong in the output envelope: "this is not a
// git repository, so --apply has no undo" is exactly the sort of thing
// a caller should see and a log file should not swallow.
func checkWorktrees(paths []string, allowDirty bool) (warnings []string, err error) {
	if allowDirty {
		return nil, nil
	}
	seenDir := make(map[string]bool)
	seenRoot := make(map[string]bool)
	for _, path := range paths {
		dir := filepath.Dir(path)
		if seenDir[dir] {
			continue
		}
		seenDir[dir] = true

		root, dirty, err := worktreeStatus(dir)
		switch {
		case errors.Is(err, errNoGit):
			warnings = append(warnings, fmt.Sprintf(
				"%v, so lightspeed cannot check that %s is clean before writing; there may be no undo", err, dir))
			continue
		case errors.Is(err, errNotARepo):
			warnings = append(warnings, fmt.Sprintf(
				"%s is %v, so --apply has no undo", dir, err))
			continue
		case err != nil:
			warnings = append(warnings, fmt.Sprintf(
				"cannot check whether %s is clean: %v; there may be no undo", dir, err))
			continue
		}
		if seenRoot[root] {
			continue
		}
		seenRoot[root] = true
		if len(dirty) > 0 {
			return warnings, dirtyWorktreeError(root, dirty)
		}
	}
	return warnings, nil
}

// dirtyWorktreeError is the refusal, with the paths that caused it and
// the two ways out.
func dirtyWorktreeError(root string, dirty []string) error {
	shown := dirty
	suffix := ""
	if len(shown) > dirtyPathLimit {
		shown = shown[:dirtyPathLimit]
		suffix = fmt.Sprintf(" and %d more", len(dirty)-len(shown))
	}
	return render.Errorf(render.CodeDirtyWorktree,
		"refusing to --apply: %s has %d uncommitted change(s) (%s%s), and `git checkout` is the only undo there is; commit or stash them, or pass --allow-dirty",
		root, len(dirty), strings.Join(shown, ", "), suffix).
		WithDetails(map[string]any{
			"root":  root,
			"dirty": dirty,
		})
}

// oneLine collapses a multi-line git error so it fits one message.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
