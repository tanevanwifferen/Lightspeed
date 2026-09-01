package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RuntimeDirName is the directory sockets live in, under
// $XDG_RUNTIME_DIR (PLAN §3).
const RuntimeDirName = "lightspeed"

// WorkspaceMarkers are the files and directories whose presence marks
// the boundary of a workspace for the purpose of *addressing a
// daemon*. The nearest ancestor holding any of them wins.
//
// This is not the question internal/router answers. The router asks
// "where is this server's project root", per server and marker-major,
// because that is what goes in `rootUri`. Here the question is "which
// repository am I in", because that decides which daemon a command
// talks to — and the answer has to be the same for every file in the
// tree, or `cd internal/store` would start a second daemon and pay the
// indexing cost twice. A repository with a nested Go module therefore
// gets one daemon holding two gopls sessions.
var WorkspaceMarkers = []string{
	".lightspeed.toml", // the one file PLAN §6 asks users to write
	".git",
	".hg",
	".jj",
	".svn",
	"go.work",
}

// Workspace resolves the workspace root that keys a daemon for path.
// The path need not exist. If no marker is found anywhere above it,
// the path's own directory is the workspace: a lone file outside any
// repository is a legitimate query, and it gets its own daemon.
func Workspace(path string) (string, error) {
	abs, err := canonical(path)
	if err != nil {
		return "", err
	}
	start := abs
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		start = filepath.Dir(abs)
	}
	for dir := start; ; {
		for _, marker := range WorkspaceMarkers {
			if _, err := os.Lstat(filepath.Join(dir, marker)); err == nil {
				return dir, nil
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return start, nil
		}
		dir = parent
	}
}

// WorkspaceHash is the <workspace-hash> of the socket name: a short
// hash of the resolved workspace root. Short because a unix socket
// path is limited to about a hundred bytes, and stable across
// processes because two invocations have to agree on the address
// without talking to each other first.
func WorkspaceHash(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:])[:16]
}

// RuntimeDir reports the directory daemon sockets live in, creating it
// with 0700 if needed: $XDG_RUNTIME_DIR/lightspeed per PLAN §3,
// falling back to a per-user directory under os.TempDir() when
// XDG_RUNTIME_DIR is unset — macOS, containers and cron all manage
// that.
//
// A directory that exists but belongs to another user is an error
// rather than somewhere to put a socket.
func RuntimeDir() (string, error) {
	var dir string
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		dir = filepath.Join(xdg, RuntimeDirName)
	} else {
		dir = filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", RuntimeDirName, os.Getuid()))
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("daemon: runtime directory %s: %w", dir, err)
	}
	if ours, err := ownedByUs(dir); err == nil && !ours {
		return "", fmt.Errorf("daemon: runtime directory %s is owned by a different user", dir)
	}
	return dir, nil
}

// SocketPath returns the socket address for a workspace root, in the
// runtime directory, creating that directory if needed.
func SocketPath(root string) (string, error) {
	dir, err := RuntimeDir()
	if err != nil {
		return "", err
	}
	return SocketPathIn(dir, root), nil
}

// SocketPathIn is [SocketPath] with the directory supplied, for tests
// and for a caller that keeps sockets somewhere else.
func SocketPathIn(dir, root string) string {
	return filepath.Join(dir, WorkspaceHash(root)+".sock")
}

// canonical makes a path absolute and resolves symlinks, so that two
// spellings of one directory hash to one socket. A path that does not
// exist is resolved as far as its deepest existing ancestor.
//
// internal/router has the same function, unexported. Duplicating a
// dozen lines is the lesser evil against exporting a second spelling
// of it from a package another agent owns; if it ever becomes three
// copies it belongs in a shared pathutil.
func canonical(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", errors.New("daemon: empty path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("daemon: %w", err)
	}
	return evalDeepest(filepath.Clean(abs)), nil
}

func evalDeepest(abs string) string {
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return abs
	}
	return filepath.Join(evalDeepest(parent), filepath.Base(abs))
}
