package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
)

// newTestService builds an in-process service — the --no-daemon path —
// over a fake fleet, and closes it when the test ends.
func newTestService(t *testing.T, fleet *fakeFleet, opts PoolOptions) *Service {
	t.Helper()
	pool := newTestPool(t, fleet, opts)
	return NewService(pool, "")
}

// TestServiceQueryUnderGate runs a real gated query end to end: the
// readiness gate accepts the answer, and the response carries the
// evidence for it (PLAN §5.2).
func TestServiceQueryUnderGate(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{}
	svc := newTestService(t, fleet, PoolOptions{})
	file := filepath.Join(root, "main.go")

	resp, err := svc.Query(context.Background(), Request{
		Path:   file,
		Method: "textDocument/references",
		Params: json.RawMessage(`{"textDocument":{"uri":"file://` + file + `"}}`),
		Open:   true,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if resp.Server != "fake-go" || resp.ServerName != "fake-fake-go" {
		t.Errorf("answered by %q/%q", resp.Server, resp.ServerName)
	}
	if resp.Root != root {
		t.Errorf("root %q, want %q", resp.Root, root)
	}
	if client.IsEmptyResult(resp.Result) {
		t.Errorf("empty result: %s", resp.Result)
	}
	// The fake never sends progress, so authority can only be
	// inferred from stability — and the response has to say so.
	if resp.Ready != client.ReadyNoProgress {
		t.Errorf("ready %q, want %q", resp.Ready, client.ReadyNoProgress)
	}
	if len(resp.Warnings) == 0 {
		t.Error("no warning on a result whose readiness was inferred")
	}
	if resp.Warm {
		t.Error("first query reported warm")
	}
	if resp.Spawns != 1 {
		t.Errorf("spawns %d, want 1", resp.Spawns)
	}

	// The document was opened, and stays open for the next query:
	// that is the other half of what makes a warm query cheap.
	second, err := svc.Query(context.Background(), Request{
		Path:   file,
		Method: "textDocument/references",
		Params: json.RawMessage(`{"textDocument":{"uri":"file://` + file + `"}}`),
		Open:   true,
	})
	if err != nil {
		t.Fatalf("second Query: %v", err)
	}
	if !second.Warm {
		t.Error("second query reported cold")
	}
	if second.Spawns != 1 {
		t.Errorf("spawns %d after two queries, want 1", second.Spawns)
	}

	opened := 0
	for _, note := range fleet.all()[0].script.Notifications() {
		if note == "textDocument/didOpen" {
			opened++
		}
	}
	if opened != 1 {
		t.Errorf("sent %d didOpen notifications for one document, want 1", opened)
	}
}

// TestServiceRefreshesChangedDocument: an agent edits, then asks. A
// warm daemon that answered from the copy it opened ten minutes ago
// would return positions into a file that no longer exists.
func TestServiceRefreshesChangedDocument(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{}
	svc := newTestService(t, fleet, PoolOptions{})
	file := filepath.Join(root, "main.go")
	req := Request{
		Path:   file,
		Method: "textDocument/references",
		Params: json.RawMessage(`{"textDocument":{"uri":"file://` + file + `"}}`),
		Open:   true,
	}

	if _, err := svc.Query(context.Background(), req); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if err := os.WriteFile(file, []byte("package fixture\n\nfunc Renamed() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Query(context.Background(), req); err != nil {
		t.Fatalf("Query after edit: %v", err)
	}

	notes := fleet.all()[0].script.Notifications()
	if !contains(notes, "textDocument/didChange") {
		t.Errorf("the edited file was never pushed to the server: %v", notes)
	}
}

// TestServiceRawSkipsTheGate: the raw escape hatch asks for one request
// and no readiness evidence, and must not be held by the gate.
func TestServiceRawSkipsTheGate(t *testing.T) {
	root := workspace(t)
	svc := newTestService(t, &fakeFleet{}, PoolOptions{})

	resp, err := svc.Query(context.Background(), Request{
		Path:   filepath.Join(root, "main.go"),
		Method: "fake/echo",
		Params: json.RawMessage(`{"hello":"world"}`),
		Raw:    true,
	})
	if err != nil {
		t.Fatalf("raw Query: %v", err)
	}
	if string(resp.Result) != `{"hello":"world"}` {
		t.Errorf("result %s", resp.Result)
	}
	if resp.Ready != "" || len(resp.Warnings) != 0 {
		t.Errorf("raw request came back with readiness evidence: %q %v", resp.Ready, resp.Warnings)
	}
}

// TestServiceIndexingQueryExitsFive is the failure mode PLAN §5.2 exists
// for: a server that is still indexing answers "no references", and
// lightspeed must refuse to pass that off as an answer.
func TestServiceIndexingQueryExitsFive(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{scenario: indexingScenario}
	svc := newTestService(t, fleet, PoolOptions{
		Gate: client.GateOptions{
			Settle:          20 * time.Millisecond,
			NoProgressGrace: 10 * time.Millisecond,
			PollInterval:    5 * time.Millisecond,
			Timeout:         200 * time.Millisecond,
		},
	})
	file := filepath.Join(root, "main.go")

	_, err := svc.Query(context.Background(), Request{
		Path:   file,
		Method: "textDocument/references",
		Params: json.RawMessage(`{"textDocument":{"uri":"file://` + file + `"}}`),
	})
	if err == nil {
		t.Fatal("a query against an indexing server returned a result")
	}
	if !errors.Is(err, client.ErrNotReady) {
		t.Errorf("got %v, want a not-ready error", err)
	}
	if got := exitCodeOf(err); got != 5 {
		t.Errorf("exit code %d, want 5", got)
	}

	// And the daemon reports the state, so `daemon status` can
	// explain why.
	st, err := svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Sessions) != 1 || !st.Sessions[0].Indexing {
		t.Errorf("status does not report the session as indexing: %+v", st.Sessions)
	}
	if len(st.Sessions[0].Progress) == 0 {
		t.Error("status does not list the outstanding progress token")
	}
}

// TestServiceUncapabilitiedMethodIsExitThree: PLAN §5.4, never call a
// method the server did not advertise.
func TestServiceUncapabilitiedMethodIsExitThree(t *testing.T) {
	root := workspace(t)
	svc := newTestService(t, &fakeFleet{}, PoolOptions{})

	_, err := svc.Query(context.Background(), Request{
		Path:   filepath.Join(root, "main.go"),
		Method: "textDocument/rename",
		Params: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Fatal("called a method the server never advertised")
	}
	if !errors.Is(err, client.ErrUnsupportedMethod) {
		t.Errorf("got %v, want an unsupported-method error", err)
	}
	if got := exitCodeOf(err); got != 3 {
		t.Errorf("exit code %d, want 3", got)
	}
}

// TestServiceUsageErrors: a request with no method or no path is a
// usage error (exit 2) and never reaches a server.
func TestServiceUsageErrors(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{}
	svc := newTestService(t, fleet, PoolOptions{})

	for name, req := range map[string]Request{
		"no method": {Path: filepath.Join(root, "main.go")},
		"no path":   {Method: "textDocument/references"},
	} {
		_, err := svc.Query(context.Background(), req)
		if err == nil {
			t.Errorf("%s: Query succeeded", name)
			continue
		}
		if got := exitCodeOf(err); got != 2 {
			t.Errorf("%s: exit code %d, want 2 (%v)", name, got, err)
		}
	}
	if got := fleet.spawnCount(); got != 0 {
		t.Errorf("a malformed request started %d servers", got)
	}
}

// TestServiceOpenMissingFile: opening a file that is not there is the
// caller's mistake, and must not be reported as a server crash.
func TestServiceOpenMissingFile(t *testing.T) {
	root := workspace(t)
	svc := newTestService(t, &fakeFleet{}, PoolOptions{})

	_, err := svc.Query(context.Background(), Request{
		Path:   filepath.Join(root, "gone.go"),
		Method: "textDocument/references",
		Params: json.RawMessage(`{}`),
		Open:   true,
	})
	if err == nil {
		t.Fatal("query on a missing file succeeded")
	}
	if got := codeOf(err); got != CodeNoSuchFile {
		t.Errorf("code %q, want %q (%v)", got, CodeNoSuchFile, err)
	}
	if got := exitCodeOf(err); got != 2 {
		t.Errorf("exit code %d, want 2", got)
	}
}

// TestServiceStopClosesThePool: --no-daemon has no daemon to stop, so
// Stop means "shut the servers down", and Close must be idempotent with
// it (the test helper closes the pool too).
func TestServiceStopClosesThePool(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{}
	svc := newTestService(t, fleet, PoolOptions{})

	if _, err := svc.Query(context.Background(), Request{
		Path:   filepath.Join(root, "main.go"),
		Method: "fake/echo",
		Raw:    true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	fleet.assertAllExited(t)
	if err := svc.Close(); err != nil {
		t.Errorf("Close after Stop: %v", err)
	}
}

// TestServiceStatusInProcess: the in-process mode says so, so that
// `daemon status --no-daemon` cannot be mistaken for a report about a
// running daemon.
func TestServiceStatusInProcess(t *testing.T) {
	svc := newTestService(t, &fakeFleet{}, PoolOptions{})
	st, err := svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !st.InProcess {
		t.Error("status does not report the in-process mode")
	}
	if st.Socket != "" {
		t.Errorf("in-process status reports socket %q", st.Socket)
	}
	if st.PID != os.Getpid() {
		t.Errorf("pid %d, want this process (%d)", st.PID, os.Getpid())
	}
	if svc.Remote() {
		t.Error("an in-process service claims to be remote")
	}
}
