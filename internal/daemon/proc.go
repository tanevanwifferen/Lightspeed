package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// Waiting for a daemon to be gone.
//
// A daemon unpublishes its socket *first* when it shuts down (see
// [Server.Shutdown]: so that a client arriving during the shutdown spawns a
// fresh daemon instead of connecting to a leaving one), and only then drains
// its requests and shuts its language servers down. So "nothing answers on
// the socket" means the daemon has started to go, not that it has gone: for
// the rest of its life it is still writing to its log and its language servers
// are still writing wherever they write. A caller that stops a daemon and then
// removes a directory, or starts the next daemon, has to wait for the
// *process*, and this file is how.

// spawned tracks the daemons this process started and has not yet seen exit.
var spawned = struct {
	mu   sync.Mutex
	done map[int]chan struct{}
}{done: map[int]chan struct{}{}}

// reap collects cmd's exit status in the background and records the daemon
// until it has exited.
//
// Reaping is the fix for a leak as well as a hook for WaitChildren: a daemon
// that has been Released rather than waited for is a zombie for as long as its
// parent lives, and the parent of a daemon can be `lightspeed mcp`, which lives
// for a whole agent session and outlives every idle-exiting daemon it starts.
// For a short-lived command the goroutine simply dies with the process and the
// daemon is reparented, as before.
func reap(cmd *exec.Cmd) {
	done := make(chan struct{})
	pid := cmd.Process.Pid
	spawned.mu.Lock()
	spawned.done[pid] = done
	spawned.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		spawned.mu.Lock()
		delete(spawned.done, pid)
		spawned.mu.Unlock()
		close(done)
	}()
}

// WaitChildren blocks until every daemon this process started has exited, or
// ctx is done. It is for a process that owns the daemons it spawned — a test
// that must not leave one writing into a directory it is about to remove, or a
// long-lived server shutting down — and it waits for the processes, not for
// their sockets.
//
// The error names the daemons that were still running.
func WaitChildren(ctx context.Context) error {
	spawned.mu.Lock()
	pending := make(map[int]chan struct{}, len(spawned.done))
	for pid, done := range spawned.done {
		pending[pid] = done
	}
	spawned.mu.Unlock()

	for pid, done := range pending {
		select {
		case <-done:
			delete(pending, pid)
		case <-ctx.Done():
		}
	}
	if len(pending) == 0 {
		return nil
	}
	pids := make([]int, 0, len(pending))
	for pid := range pending {
		pids = append(pids, pid)
	}
	slices.Sort(pids)
	names := make([]string, len(pids))
	for i, pid := range pids {
		names[i] = fmt.Sprint(pid)
	}
	return fmt.Errorf("daemon: still running after %v: pid %s", ctx.Err(), strings.Join(names, ", "))
}

// WaitExit blocks until the process with the given pid has exited, or ctx is
// done. It is how `daemon stop` keeps its promise that the daemon has gone,
// for a daemon some other process started and this one cannot Wait on.
//
// A pid that is this process's own is refused: an in-process pool has no
// daemon to wait for, and waiting for oneself would only time out.
func WaitExit(ctx context.Context, pid int) error {
	if pid <= 0 {
		return nil
	}
	if pid == os.Getpid() {
		return fmt.Errorf("daemon: pid %d is this process, not a daemon", pid)
	}
	for processAlive(pid) {
		select {
		case <-ctx.Done():
			return fmt.Errorf("daemon: process %d was still running: %w", pid, ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	return nil
}
