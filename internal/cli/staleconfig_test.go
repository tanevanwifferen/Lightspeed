package cli

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// docs/DECISIONS.md D17: a warm daemon holds sessions started under the
// server definitions of the moment it started them. A changed
// configuration must not be answered from those. An idle daemon is
// restarted; one another command is using is refused, with the way out
// named. Both are real daemons, this test binary re-executed, and the
// process that answers is counted by the fake server's own spawn log.

// editConfig rewrites the workspace's .lightspeed.toml so that gopls's
// settings differ: a change to the effective definition that no
// query-level behaviour could hide.
func editConfig(t *testing.T, dir, marker string) {
	t.Helper()
	write(t, filepath.Join(dir, serverdef.WorkspaceFile),
		"schema_version = 1\nname = \"gopls\"\n\n[server]\nsettings = { marker = \""+marker+"\" }\n")
}

func TestChangedConfigRestartsAnIdleDaemon(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	daemonEnv(t, "60s")
	spawns, exits := spawnLog(t)
	query := func() (int, string, string) { return runMain("definition", file+":3:5") }

	if code, out, errOut := query(); code != ExitOK {
		t.Fatalf("first query: exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}
	first := statusOf(t, "--path", dir)
	if !first.Running || spawns() != 1 {
		t.Fatalf("first query left running=%v, %d servers started; want a daemon and one server", first.Running, spawns())
	}

	// Unchanged configuration: the warm daemon answers, nothing restarts.
	if code, _, errOut := query(); code != ExitOK || strings.Contains(errOut, "restarting") {
		t.Fatalf("unchanged config: exit %d, stderr %q; want a quiet warm answer", code, errOut)
	}
	if st := statusOf(t, "--path", dir); st.PID != first.PID || spawns() != 1 {
		t.Fatalf("an unchanged configuration restarted the daemon (pid %d -> %d, %d servers started)", first.PID, st.PID, spawns())
	}

	// Changed configuration: the daemon that was started under the old
	// one is not the one to answer.
	editConfig(t, dir, "one")
	code, out, errOut := query()
	if code != ExitOK {
		t.Fatalf("after the edit: exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}
	if !strings.Contains(errOut, "restarting") {
		t.Errorf("stderr = %q, want a note that the daemon was restarted", errOut)
	}
	second := statusOf(t, "--path", dir)
	if second.PID == first.PID {
		t.Fatalf("the same daemon (pid %d) answered under a changed configuration", first.PID)
	}
	if spawns() != 2 {
		t.Errorf("%d language servers started, want 2: the warm one must not answer for the new definition", spawns())
	}
	eventually(t, "the old language server to be shut down with its daemon", 5*time.Second, func() bool { return exits() >= 1 })

	// And the new daemon is now the warm one.
	if code, _, _ := query(); code != ExitOK {
		t.Fatalf("query on the restarted daemon: exit %d", code)
	}
	if st := statusOf(t, "--path", dir); st.PID != second.PID || spawns() != 2 {
		t.Errorf("the restarted daemon was not reused (pid %d -> %d, %d servers started)", second.PID, st.PID, spawns())
	}
}

func TestChangedConfigIsRefusedWhileAnotherCommandIsConnected(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	daemonEnv(t, "60s")
	query := func() (int, string, string) { return runMain("definition", file+":3:5") }

	if code, out, errOut := query(); code != ExitOK {
		t.Fatalf("first query: exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}
	first := statusOf(t, "--path", dir)

	// Another command, holding a connection.
	other, err := daemon.Dial(context.Background(), daemon.ClientOptions{Socket: first.Socket, NoSpawn: true})
	if err != nil {
		t.Fatal(err)
	}
	editConfig(t, dir, "two")

	code, out, _ := query()
	env := decodeData(t, out, nil)
	if code != ExitUsage || env.Error == nil || env.Error.Code != "daemon_stale" {
		t.Fatalf("exit %d, error %+v; want exit 2 daemon_stale", code, env.Error)
	}
	if !strings.Contains(env.Error.Message, "daemon stop") {
		t.Errorf("message %q does not say how to clear it", env.Error.Message)
	}
	if st := statusOf(t, "--path", dir); st.PID != first.PID {
		t.Errorf("the daemon was restarted (pid %d -> %d) under a command that was using it", first.PID, st.PID)
	}

	// --no-daemon does not care: it never met the daemon.
	if code, out, errOut := runMain("--no-daemon", "definition", file+":3:5"); code != ExitOK {
		t.Errorf("--no-daemon: exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}

	// Once the other command has gone the same invocation succeeds.
	_ = other.Close()
	eventually(t, "the restart once the daemon is idle", 5*time.Second, func() bool {
		code, _, _ := query()
		return code == ExitOK
	})
	if st := statusOf(t, "--path", dir); st.PID == first.PID {
		t.Errorf("still the old daemon (pid %d) after the config changed and the daemon went idle", st.PID)
	}
}

// TestDaemonStopWorksWithABrokenConfig: the management commands read no
// server definitions, so the way out of a bad configuration is not itself
// blocked by it.
func TestDaemonStopWorksWithABrokenConfig(t *testing.T) {
	dir, file := cjkFixture(t)
	scenario{results: map[string]any{methodDefinition: []any{loc(file, 2, 4, 6)}}}.apply(t)
	daemonEnv(t, "60s")
	if code, out, errOut := runMain("definition", file+":3:5"); code != ExitOK {
		t.Fatalf("exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}
	write(t, filepath.Join(dir, serverdef.WorkspaceFile), "name = \n")

	if code, out, _ := runMain("definition", file+":3:5"); code != ExitUsage {
		t.Errorf("a query in a broken workspace: exit %d, want %d; %s", code, ExitUsage, out)
	}
	if code, out, errOut := runMain("daemon", "stop", "--path", dir); code != ExitOK || !strings.Contains(out, `"stopped":true`) {
		t.Errorf("daemon stop: exit %d; stderr: %s\nstdout: %s", code, errOut, out)
	}
}
