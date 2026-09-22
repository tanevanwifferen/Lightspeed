package daemon

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// A process that exits on its own must be waited for as a process: these
// tests use a short-lived `sleep` where a real daemon would be, because what
// is under test is the waiting, not the daemon.
func sleeper(t *testing.T, seconds string) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sleep(1)")
	}
	path, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep(1)")
	}
	cmd := exec.Command(path, seconds)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestWaitChildrenWaitsForTheProcessNotTheSocket(t *testing.T) {
	cmd := sleeper(t, "0.3")
	reap(cmd)

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := WaitChildren(short); err == nil {
		t.Fatal("WaitChildren returned while the child was still running")
	}

	long, cancelLong := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelLong()
	if err := WaitChildren(long); err != nil {
		t.Fatalf("WaitChildren: %v", err)
	}
	// Reaped, not left a zombie: the pid is gone, not merely finished.
	if processAlive(cmd.Process.Pid) {
		t.Errorf("pid %d is still there after WaitChildren", cmd.Process.Pid)
	}
	if err := WaitChildren(context.Background()); err != nil {
		t.Errorf("nothing is running, but WaitChildren says: %v", err)
	}
}

func TestWaitExit(t *testing.T) {
	cmd := sleeper(t, "0.3")
	reap(cmd)
	pid := cmd.Process.Pid

	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := WaitExit(short, pid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitExit on a running process = %v, want the deadline", err)
	}
	long, cancelLong := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelLong()
	if err := WaitExit(long, pid); err != nil {
		t.Fatalf("WaitExit: %v", err)
	}
	if err := WaitExit(context.Background(), 0); err != nil {
		t.Errorf("WaitExit(0) = %v, want nil: there is no process to wait for", err)
	}
	if err := WaitExit(context.Background(), os.Getpid()); err == nil {
		t.Error("WaitExit on this very process must be refused, not wait forever")
	}
}
