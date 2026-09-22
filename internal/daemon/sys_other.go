//go:build !unix

package daemon

import (
	"os"
	"os/exec"
)

// ownedByUs cannot be answered portably; fail open, as gopls's
// verifyRemoteOwnershipDefault does.
func ownedByUs(path string) (bool, error) { return true, nil }

// inode has no portable equivalent. Returning 0 makes the socket
// identity check in the server's shutdown path a no-op, which only
// costs us the ability to distinguish our own socket from a
// successor's.
func inode(path string) (uint64, error) {
	if _, err := os.Stat(path); err != nil {
		return 0, err
	}
	return 0, nil
}

// daemonize has no portable equivalent.
func daemonize(cmd *exec.Cmd) {}

// processAlive cannot be answered portably. Reporting the process as gone
// makes WaitExit return at once, which costs only the guarantee that the
// daemon has finished shutting down, not correctness.
func processAlive(pid int) bool { return false }
