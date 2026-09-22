package cli

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
)

// docs/DECISIONS.md D29: a daemon started by a different lightspeed build
// is replaced, not used, and a daemon whose own executable is gone leaves
// when nobody is connected. The "different build" is real: a copy of this
// test binary with another modification time, started as `daemon serve`
// exactly as the auto-spawn starts one.

// foreignDaemon is a daemon process started from a copy of the test binary.
type foreignDaemon struct {
	exe    string
	cmd    *exec.Cmd
	exited chan struct{}
}

// startForeignDaemon starts a daemon for dir's workspace from another
// executable, and returns when it is answering on the socket.
func startForeignDaemon(t *testing.T, dir string) *foreignDaemon {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(t.TempDir(), "lightspeed-other")
	src, err := os.Open(self)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst, err := os.OpenFile(exe, os.O_CREATE|os.O_WRONLY, 0o755)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	// Another build is another modification time, whatever the copy's own.
	old := time.Now().Add(-90 * time.Minute)
	if err := os.Chtimes(exe, old, old); err != nil {
		t.Fatal(err)
	}

	opts, err := daemonOptions(&env{}, dir)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, opts.Spawn.Args(opts.Socket)...)
	logf, err := os.Create(filepath.Join(t.TempDir(), "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = logf, logf
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(logf.Name())
			t.Logf("the foreign daemon's log:\n%s", b)
		}
	})
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	fd := &foreignDaemon{exe: exe, cmd: cmd, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		close(fd.exited)
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-fd.exited
	})
	eventually(t, "the foreign daemon to answer", 10*time.Second, func() bool {
		c, err := daemon.Dial(context.Background(), daemon.ClientOptions{Socket: opts.Socket, NoSpawn: true})
		if err != nil {
			return false
		}
		_ = c.Close()
		return true
	})
	return fd
}

func TestDaemonFromAnotherBuildIsReplacedWithAWarning(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	daemonEnv(t, "60s")
	other := startForeignDaemon(t, dir)

	before := statusOf(t, "--path", dir)
	if before.PID != other.cmd.Process.Pid || before.Executable != other.exe {
		t.Fatalf("status describes pid %d (%s), want the foreign daemon %d (%s)", before.PID, before.Executable, other.cmd.Process.Pid, other.exe)
	}
	if !strings.Contains(before.StaleBuild, "different lightspeed executable") {
		t.Fatalf("daemon status stale_build = %q, want it to name a different executable", before.StaleBuild)
	}

	code, out, errOut := runMain("definition", file+":3:5")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}
	env := decodeData(t, out, nil)
	if !env.OK || len(env.Warnings) == 0 || !strings.Contains(env.Warnings[0], "replaced the daemon") ||
		!strings.Contains(env.Warnings[0], other.exe) {
		t.Errorf("warnings = %q, want the first to say the daemon was replaced and by whom it was started", env.Warnings)
	}

	select {
	case <-other.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the foreign daemon is still running after it was replaced")
	}
	self, _ := os.Executable()
	after := statusOf(t, "--path", dir)
	if after.PID == before.PID || after.Executable != self || after.StaleBuild != "" {
		t.Errorf("after: pid %d exe %s stale %q; want a new daemon running %s", after.PID, after.Executable, after.StaleBuild, self)
	}

	// The replacement is the warm one now, and says nothing.
	code, out, errOut = runMain("definition", file+":3:5")
	if code != ExitOK {
		t.Fatalf("second query: exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}
	if env := decodeData(t, out, nil); len(env.Warnings) != 0 {
		t.Errorf("second query warned: %q", env.Warnings)
	}
	if st := statusOf(t, "--path", dir); st.PID != after.PID {
		t.Errorf("the replacement was replaced again (pid %d -> %d)", after.PID, st.PID)
	}
}

// --format text has no envelope to carry a warning, so the note goes to
// stderr and stdout stays grep-compatible.
func TestReplacedDaemonNoteInTextFormatGoesToStderr(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	daemonEnv(t, "60s")
	startForeignDaemon(t, dir)

	code, out, errOut := runMain("definition", "--format", "text", file+":3:5")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}
	if !strings.Contains(errOut, "replaced the daemon") {
		t.Errorf("stderr = %q, want the note", errOut)
	}
	if strings.Contains(out, "replaced the daemon") || !strings.Contains(out, ":") {
		t.Errorf("stdout = %q, want only the result", out)
	}
}

// A daemon another command is using is not stopped under it: the mix-up is
// refused with the way out, and cleared once the other command has gone.
func TestDaemonFromAnotherBuildIsNotReplacedUnderAConnectedCommand(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	daemonEnv(t, "60s")
	other := startForeignDaemon(t, dir)
	st := statusOf(t, "--path", dir)

	busy, err := daemon.Dial(context.Background(), daemon.ClientOptions{Socket: st.Socket, NoSpawn: true})
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ := runMain("definition", file+":3:5")
	env := decodeData(t, out, nil)
	if code != ExitUsage || env.Error == nil || env.Error.Code != "daemon_stale" ||
		!strings.Contains(env.Error.Message, "another lightspeed build") || !strings.Contains(env.Error.Message, "daemon stop") {
		t.Fatalf("exit %d, error %+v; want exit 2 daemon_stale naming the other build and the way out", code, env.Error)
	}
	select {
	case <-other.exited:
		t.Fatal("the daemon was stopped under a connected command")
	default:
	}

	_ = busy.Close()
	eventually(t, "the replacement once the other command has gone", 5*time.Second, func() bool {
		code, _, _ := runMain("definition", file+":3:5")
		return code == ExitOK
	})
	select {
	case <-other.exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the foreign daemon outlived its replacement")
	}
}

// --no-daemon never meets a daemon, so it has nothing to be stale against.
func TestNoDaemonIgnoresAForeignDaemon(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	daemonEnv(t, "60s")
	other := startForeignDaemon(t, dir)

	code, out, errOut := runMain("--no-daemon", "definition", file+":3:5")
	if code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}
	if env := decodeData(t, out, nil); len(env.Warnings) != 0 {
		t.Errorf("--no-daemon warned: %q", env.Warnings)
	}
	select {
	case <-other.exited:
		t.Error("--no-daemon stopped a daemon it should not have touched")
	default:
	}
}

// A daemon whose executable was rebuilt or deleted is an older program than
// the one anybody would start. It leaves once nobody is connected, and is
// never cut off while somebody is.
func TestDaemonLeavesWhenItsExecutableIsGoneAndItIsIdle(t *testing.T) {
	dir, _ := cjkFixture(t)
	daemonEnv(t, "10m") // far longer than the test: only the executable can end it
	t.Setenv(daemonExeCheckEnv, "50ms")
	other := startForeignDaemon(t, dir)
	st := statusOf(t, "--path", dir)

	busy, err := daemon.Dial(context.Background(), daemon.ClientOptions{Socket: st.Socket, NoSpawn: true})
	if err != nil {
		t.Fatal(err)
	}
	// Dial returns when the kernel has queued the connection; a round trip is
	// what says the daemon has accepted it and counts it as a client.
	if _, err := busy.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(other.exe); err != nil {
		t.Fatal(err)
	}

	select {
	case <-other.exited:
		t.Fatal("the daemon left with a client connected")
	case <-time.After(500 * time.Millisecond): // ten checks
	}

	_ = busy.Close()
	select {
	case <-other.exited:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon whose executable was deleted is still running with no client connected")
	}
	if code := other.cmd.ProcessState.ExitCode(); code != 0 {
		t.Errorf("the daemon exited %d, want 0: leaving for this reason is not a failure", code)
	}
}
