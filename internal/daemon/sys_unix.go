//go:build unix

package daemon

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
)

// ownedByUs reports whether path belongs to the current user. It is
// gopls's verifyRemoteOwnershipPosix: a socket in a shared directory
// that someone else owns must not be dialled, because on the other
// end of it is someone else's process with our workspace's file
// contents flowing through it.
//
// A path that does not exist is "ours" — there is nothing to hijack
// yet, and the caller is about to create it.
func ownedByUs(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, fmt.Errorf("checking owner of %s: %w", path, err)
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false, errors.New("stat: not a syscall.Stat_t")
	}
	u, err := user.Current()
	if err != nil {
		return false, fmt.Errorf("checking current user: %w", err)
	}
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil {
		return false, fmt.Errorf("parsing current uid: %w", err)
	}
	return stat.Uid == uint32(uid), nil
}

// inode identifies the file at path, so that a daemon shutting down
// can tell its own socket from a successor's that was renamed over it.
func inode(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("stat: not a syscall.Stat_t")
	}
	return uint64(stat.Ino), nil
}

// daemonize detaches the spawned daemon from the client's session, so
// that the terminal closing, or a Ctrl-C in the shell that happened to
// start it, does not take the daemon with it. gopls does exactly this.
func daemonize(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// processAlive reports whether a process with this pid is still running.
// Signal 0 checks for existence without sending anything; EPERM means it
// exists and belongs to someone else. A zombie — exited, not yet reaped by its
// parent — still answers signal 0, and is not running, so where /proc says so
// it is counted as gone: a daemon whose parent is an older, non-reaping
// process must not make `daemon stop` wait out its whole timeout.
func processAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		// "pid (comm) S ...": the state is the field after the last ')',
		// which is the only reliable place to look, comm being free text.
		if i := bytes.LastIndexByte(stat, ')'); i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z' {
			return false
		}
	}
	return true
}
