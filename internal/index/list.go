package index

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// WalkFileLimit bounds the fallback walk, so that a directory that is not a
// repository — a home directory, say — is not read to the end.
const WalkFileLimit = 200000

// gitListTimeout bounds one `git ls-files`. git is local and fast; a git that
// is not is waiting on a lock we should not wait for.
const gitListTimeout = 30 * time.Second

var (
	// ErrNoGit reports that git is not installed.
	ErrNoGit = errors.New("git is not on PATH")
	// ErrNotARepo reports that the directory is not inside a repository.
	ErrNotARepo = errors.New("not inside a git repository")
)

// A Listing is the files under a directory, and how they were found.
type Listing struct {
	// Files are the paths relative to the directory, with forward slashes,
	// sorted.
	Files []string
	// Source is "git" when git said which files there are, "walk" when the
	// directory was walked.
	Source   string
	Warnings []string
}

// ListFiles lists the files under root: inside a git repository it is git's own
// answer — the tracked files and the untracked ones that are not ignored
// (`git ls-files --cached --others --exclude-standard`) — so .gitignore,
// .git/info/exclude and the user's global excludes all apply without
// lightspeed reimplementing them. Anywhere else, or if git will not answer, the
// directory is walked, skipping hidden and build directories (SkipDir), which
// is a guess at what an ignore file would say and is reported as one.
//
// It is the enumeration `tree`, `search_text` and the index share (D23, D26,
// D32), so a file is in the index exactly when `tree` lists it. Entries that
// are gone or are not regular files are dropped by the caller's stat, not
// here: this is one process spawn and no per-file work.
func ListFiles(ctx context.Context, root string) (Listing, error) {
	var l Listing
	names, err := GitFiles(ctx, root)
	if err == nil {
		l.Source = "git"
	} else {
		l.Source = "walk"
		if !errors.Is(err, ErrNoGit) && !errors.Is(err, ErrNotARepo) {
			l.Warnings = append(l.Warnings, "git would not list the files ("+err.Error()+"), so the directory was walked")
		} else {
			l.Warnings = append(l.Warnings,
				"not a git repository, so .gitignore was not applied; hidden and build directories were skipped instead")
		}
		var capped bool
		names, capped = WalkFiles(ctx, root)
		if capped {
			l.Warnings = append(l.Warnings, fmt.Sprintf("the walk stopped after %d files", WalkFileLimit))
		}
	}
	seen := make(map[string]bool, len(names))
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			l.Files = append(l.Files, n)
		}
	}
	sort.Strings(l.Files)
	return l, ctx.Err()
}

// GitFiles is git's list of the files under dir, relative to dir. The error is
// [ErrNoGit] or [ErrNotARepo] when git is not there or dir is not a repository.
func GitFiles(ctx context.Context, dir string) ([]string, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, ErrNoGit
	}
	ctx, cancel := context.WithTimeout(ctx, gitListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, git, "ls-files", "-z", "--cached", "--others", "--exclude-standard", "--", ".")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 128 {
			// "fatal: not a git repository", or a refusal such as dubious
			// ownership: either way there is no ignore file semantics to
			// borrow.
			return nil, fmt.Errorf("%w: %s", ErrNotARepo, strings.TrimSpace(string(ee.Stderr)))
		}
		return nil, err
	}
	var names []string
	for _, name := range strings.Split(string(out), "\x00") {
		if name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// WalkFiles is the fallback: every file under dir except in hidden and build
// directories, relative to dir with forward slashes.
func WalkFiles(ctx context.Context, dir string) (names []string, capped bool) {
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if p != dir && SkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if len(names) >= WalkFileLimit {
			capped = true
			return filepath.SkipAll
		}
		if rel, err := filepath.Rel(dir, p); err == nil {
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	return names, capped
}

// SkipDir reports whether a directory is skipped by the fallback walk: hidden
// directories and the usual dependency and build output.
func SkipDir(name string) bool {
	switch name {
	case "node_modules", "vendor", "target", "dist", "build", "__pycache__":
		return true
	}
	return strings.HasPrefix(name, ".") && name != "."
}

// statRegular is os.Lstat for a workspace file: ok is false, with the reason,
// when the path is gone, is a directory, or is not a regular file. A symlink is
// deliberately not followed: the index never reads through a link, so a link
// that points out of the workspace cannot make it read what is out there.
func statRegular(path string) (fs.FileInfo, string) {
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
		if errors.Is(err, fs.ErrNotExist) {
			return nil, "gone"
		}
		return nil, "unreadable"
	case fi.Mode()&fs.ModeSymlink != 0:
		return nil, "symlink"
	case fi.IsDir():
		return nil, "directory"
	case !fi.Mode().IsRegular():
		return nil, "not_regular"
	}
	return fi, ""
}
