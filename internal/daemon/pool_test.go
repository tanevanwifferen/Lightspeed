package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// TestPoolReusesWarmSession is the M3 acceptance criterion, expressed
// as a mechanism rather than a stopwatch: the second query must reuse
// the running server. The fake takes 300ms to answer `initialize`, so a
// second spawn would be plainly visible in the timings too — but the
// assertion is on the spawn count, which cannot flake on a loaded
// machine.
func TestPoolReusesWarmSession(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{startDelay: 300 * time.Millisecond}
	pool := newTestPool(t, fleet, PoolOptions{})
	ctx := context.Background()
	file := filepath.Join(root, "main.go")

	first, err := pool.Acquire(ctx, Target{Path: file})
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if first.Warm() {
		t.Error("first acquire reported a warm session; it paid for the spawn")
	}
	firstSession := first.Session()
	first.Release()

	second, err := pool.Acquire(ctx, Target{Path: file})
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	defer second.Release()

	if !second.Warm() {
		t.Error("second acquire reported a cold session")
	}
	if second.Session() != firstSession {
		t.Error("second acquire got a different session: the server was restarted")
	}
	if got := fleet.spawnCount(); got != 1 {
		t.Errorf("spawned %d servers, want 1", got)
	}
	if got := pool.Spawns(); got != 1 {
		t.Errorf("pool reports %d spawns, want 1", got)
	}
}

// TestPoolQueryingFromSubdirectoryReusesSession is the same property
// from the other direction: a file deeper in the tree resolves to the
// same root, so it must not start a second server.
func TestPoolQueryingFromSubdirectoryReusesSession(t *testing.T) {
	root := workspace(t, "main.go", "internal/store/user.go")
	fleet := &fakeFleet{}
	pool := newTestPool(t, fleet, PoolOptions{})
	ctx := context.Background()

	for _, rel := range []string{"main.go", "internal/store/user.go"} {
		lease, err := pool.Acquire(ctx, Target{Path: filepath.Join(root, rel)})
		if err != nil {
			t.Fatalf("acquire %s: %v", rel, err)
		}
		if lease.Root() != root {
			t.Errorf("%s resolved to root %q, want %q", rel, lease.Root(), root)
		}
		lease.Release()
	}
	if got := fleet.spawnCount(); got != 1 {
		t.Errorf("spawned %d servers for one workspace, want 1", got)
	}
}

// TestPoolSpawnsOnceUnderConcurrency is the property that makes the
// daemon worth having at all: eight simultaneous clients must not start
// eight rust-analyzers. The startup delay guarantees the requests
// overlap the spawn.
func TestPoolSpawnsOnceUnderConcurrency(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{startDelay: 200 * time.Millisecond}
	pool := newTestPool(t, fleet, PoolOptions{})
	file := filepath.Join(root, "main.go")

	const clients = 8
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		leases  []*Lease
		failure error
	)
	for range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			lease, err := pool.Acquire(ctx, Target{Path: file})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failure = err
				return
			}
			leases = append(leases, lease)
		}()
	}
	wg.Wait()
	for _, l := range leases {
		l.Release()
	}
	if failure != nil {
		t.Fatalf("concurrent acquire: %v", failure)
	}
	if len(leases) != clients {
		t.Fatalf("got %d leases, want %d", len(leases), clients)
	}
	if got := fleet.spawnCount(); got != 1 {
		t.Errorf("%d concurrent clients caused %d spawns, want 1", clients, got)
	}
	// Exactly one of them paid for the spawn.
	cold := 0
	for _, l := range leases {
		if !l.Warm() {
			cold++
		}
	}
	if cold != 1 {
		t.Errorf("%d clients reported a cold session, want 1", cold)
	}
}

// TestPoolCancelledClientDoesNotAbortSpawn: the client that happened to
// trigger the spawn giving up must not cancel it for the clients
// waiting behind it. Otherwise a Ctrl-C in one terminal would throw away
// another terminal's 90-second index.
func TestPoolCancelledClientDoesNotAbortSpawn(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{startDelay: 150 * time.Millisecond}
	pool := newTestPool(t, fleet, PoolOptions{})
	file := filepath.Join(root, "main.go")

	impatient, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()

	patientDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lease, err := pool.Acquire(ctx, Target{Path: file})
		if err == nil {
			lease.Release()
		}
		patientDone <- err
	}()

	if _, err := pool.Acquire(impatient, Target{Path: file}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("impatient acquire: got %v, want a deadline error", err)
	}
	if err := <-patientDone; err != nil {
		t.Fatalf("patient acquire: %v", err)
	}
	if got := fleet.spawnCount(); got != 1 {
		t.Errorf("spawned %d servers, want 1", got)
	}
}

// TestPoolKeysSessionsByServerAndRoot is the heterogeneous half of PLAN
// §1 item 2: two roots are two sessions of one server, and two servers
// over one root are two sessions too. gopls's daemon has no such
// distinction to make.
func TestPoolKeysSessionsByServerAndRoot(t *testing.T) {
	goRoot := workspace(t)
	otherRoot := workspace(t)

	rust := fakeDef("fake-rs", "rust", ".rs", 40)
	pool := newTestPool(t, &fakeFleet{}, PoolOptions{
		Router: testRouter(t, goDef(), rust),
	})
	ctx := context.Background()

	acquire := func(path string) *Lease {
		t.Helper()
		lease, err := pool.Acquire(ctx, Target{Path: path})
		if err != nil {
			t.Fatalf("acquire %s: %v", path, err)
		}
		return lease
	}

	a := acquire(filepath.Join(goRoot, "main.go"))
	b := acquire(filepath.Join(otherRoot, "main.go"))
	c := acquire(filepath.Join(goRoot, "lib.rs"))

	if a.Session() == b.Session() {
		t.Error("two workspace roots shared one session")
	}
	if a.Session() == c.Session() {
		t.Error("two servers over one root shared one session")
	}
	if a.Server().Name == c.Server().Name {
		t.Errorf("both files went to server %q", a.Server().Name)
	}
	for _, l := range []*Lease{a, b, c} {
		l.Release()
	}

	sessions := pool.Sessions()
	if len(sessions) != 3 {
		t.Fatalf("pool holds %d sessions, want 3", len(sessions))
	}
	if sessions[0].Server != "fake-go" || sessions[2].Server != "fake-rs" {
		t.Errorf("sessions not in a stable order: %v", []string{sessions[0].Server, sessions[1].Server, sessions[2].Server})
	}
	if sessions[0].ServerName != "fake-fake-go" {
		t.Errorf("session reports server name %q, want the name from the handshake", sessions[0].ServerName)
	}
}

// TestPoolResolutionFailures: a file no server claims, and a server
// asked for by name that does not handle the file. Both must carry
// exit code 3 — "no server, and configuring one would fix it".
func TestPoolResolutionFailures(t *testing.T) {
	root := workspace(t, "main.go", "notes.md")
	pool := newTestPool(t, &fakeFleet{}, PoolOptions{})
	ctx := context.Background()

	if _, err := pool.Acquire(ctx, Target{Path: filepath.Join(root, "notes.md")}); err == nil {
		t.Fatal("acquire on an unclaimed file succeeded")
	} else if !errors.Is(err, router.ErrNoServer) {
		t.Errorf("got %v, want a router no-server error", err)
	}

	_, err := pool.Acquire(ctx, Target{Path: filepath.Join(root, "main.go"), Server: "not-installed"})
	if err == nil {
		t.Fatal("acquire with an unknown server name succeeded")
	}
	if got := exitCodeOf(err); got != 3 {
		t.Errorf("exit code %d, want 3", got)
	}
	if got := codeOf(err); got != CodeNoServer {
		t.Errorf("code %q, want %q", got, CodeNoServer)
	}
	if _, err := pool.Acquire(ctx, Target{}); err == nil {
		t.Fatal("acquire without a path succeeded")
	}
}

// TestPoolReapIdleLeavesOtherServersAlone: reaping is per server, as
// PLAN's M3 requires. One session goes, its neighbour and the pool stay.
func TestPoolReapIdleLeavesOtherServersAlone(t *testing.T) {
	oldRoot := workspace(t)
	freshRoot := workspace(t)
	fleet := &fakeFleet{}
	// Reaping disabled in the background: this test drives ReapIdle
	// itself, so the assertions do not depend on a timer.
	pool := newTestPool(t, fleet, PoolOptions{SessionIdleTimeout: -1})
	ctx := context.Background()

	stale, err := pool.Acquire(ctx, Target{Path: filepath.Join(oldRoot, "main.go")})
	if err != nil {
		t.Fatal(err)
	}
	staleInstance := fleet.all()[0]
	stale.Release()

	// Make the first session look idle, then take a lease on the
	// second so that "recently used" and "in use" are both covered.
	time.Sleep(30 * time.Millisecond)
	fresh, err := pool.Acquire(ctx, Target{Path: filepath.Join(freshRoot, "main.go")})
	if err != nil {
		t.Fatal(err)
	}

	if n := pool.ReapIdle(ctx, 20*time.Millisecond); n != 1 {
		t.Fatalf("reaped %d sessions, want 1", n)
	}
	select {
	case <-staleInstance.exited:
	case <-time.After(2 * time.Second):
		t.Error("the reaped server did not exit")
	}

	// The leased session survives regardless of its idle time...
	if n := pool.ReapIdle(ctx, 0); n != 0 {
		t.Errorf("reaped %d sessions while one was in flight, want 0", n)
	}
	fresh.Release()

	// ...and is still usable, which is the "without killing the
	// daemon" half of the requirement.
	again, err := pool.Acquire(ctx, Target{Path: filepath.Join(freshRoot, "main.go")})
	if err != nil {
		t.Fatalf("acquire after reaping a neighbour: %v", err)
	}
	if !again.Warm() {
		t.Error("the surviving session was restarted")
	}
	again.Release()
	if got := fleet.spawnCount(); got != 2 {
		t.Errorf("spawned %d servers, want 2", got)
	}

	// ...and the reaped one comes back on demand.
	revived, err := pool.Acquire(ctx, Target{Path: filepath.Join(oldRoot, "main.go")})
	if err != nil {
		t.Fatalf("acquire after reaping: %v", err)
	}
	if revived.Warm() {
		t.Error("the reaped session was reported warm")
	}
	revived.Release()
	if got := fleet.spawnCount(); got != 3 {
		t.Errorf("spawned %d servers, want 3", got)
	}
}

// TestPoolBackgroundReaper checks the wiring the previous test bypasses:
// that something actually calls ReapIdle on a timer.
func TestPoolBackgroundReaper(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{}
	pool := newTestPool(t, fleet, PoolOptions{SessionIdleTimeout: 60 * time.Millisecond})
	ctx := context.Background()

	lease, err := pool.Acquire(ctx, Target{Path: filepath.Join(root, "main.go")})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	waitFor(t, "the background reaper to shut down an idle server", func() bool {
		return len(pool.Sessions()) == 0
	})
	fleet.assertAllExited(t)
}

// TestPoolRespawnsAfterServerDeath: a server that crashes must be
// noticed and replaced, not handed to the next request as a dead pipe.
// One server crashing is also not a reason to disturb the others.
func TestPoolRespawnsAfterServerDeath(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{}
	pool := newTestPool(t, fleet, PoolOptions{SessionIdleTimeout: -1})
	ctx := context.Background()
	file := filepath.Join(root, "main.go")

	lease, err := pool.Acquire(ctx, Target{Path: file})
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()

	fleet.all()[0].kill()
	waitFor(t, "the pool to notice the dead server", func() bool {
		return len(pool.Sessions()) == 0
	})

	replacement, err := pool.Acquire(ctx, Target{Path: file})
	if err != nil {
		t.Fatalf("acquire after a crash: %v", err)
	}
	defer replacement.Release()
	if replacement.Warm() {
		t.Error("the replacement session was reported warm")
	}
	if got := fleet.spawnCount(); got != 2 {
		t.Errorf("spawned %d servers, want 2", got)
	}
}

// TestPoolLaunchFailureIsExit3: a server that is not installed is a
// configuration problem (exit 3, PLAN §4), not a crash, and the pool
// must not keep a broken entry around.
func TestPoolLaunchFailureIsExit3(t *testing.T) {
	root := workspace(t)
	pool, err := NewPool(PoolOptions{
		Router:   testRouter(t),
		Launcher: ExecLauncher(nil),
		Logf:     t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close(context.Background()) })

	_, err = pool.Acquire(context.Background(), Target{Path: filepath.Join(root, "main.go")})
	if err == nil {
		t.Fatal("acquiring a session for a server that is not installed succeeded")
	}
	if got := exitCodeOf(err); got != 3 {
		t.Errorf("exit code %d, want 3 (%v)", got, err)
	}
	if got := codeOf(err); got != CodeServerNotInstalled {
		t.Errorf("code %q, want %q", got, CodeServerNotInstalled)
	}
	if n := len(pool.Sessions()); n != 0 {
		t.Errorf("pool kept %d sessions after a failed launch, want 0", n)
	}
}

// TestPoolDrainWaitsForLeases: a graceful shutdown must not pull a
// server out from under a request that is still running.
func TestPoolDrainWaitsForLeases(t *testing.T) {
	root := workspace(t)
	pool := newTestPool(t, &fakeFleet{}, PoolOptions{})
	ctx := context.Background()

	lease, err := pool.Acquire(ctx, Target{Path: filepath.Join(root, "main.go")})
	if err != nil {
		t.Fatal(err)
	}

	quick, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := pool.Drain(quick); err == nil {
		t.Error("Drain returned while a lease was outstanding")
	}

	released := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		lease.Release()
		close(released)
	}()
	patient, cancel2 := context.WithTimeout(ctx, 3*time.Second)
	defer cancel2()
	if err := pool.Drain(patient); err != nil {
		t.Errorf("Drain after release: %v", err)
	}
	<-released
}

// TestPoolCloseShutsDownEverySession proves the shutdown is polite and
// complete: every fake server saw `shutdown` and `exit` and returned,
// and the pool refuses further work.
func TestPoolCloseShutsDownEverySession(t *testing.T) {
	rootA := workspace(t)
	rootB := workspace(t)
	fleet := &fakeFleet{}
	pool, err := NewPool(PoolOptions{
		Router:          testRouter(t),
		Launcher:        fleet.launcher(),
		Gate:            fastGate(),
		ShutdownTimeout: 2 * time.Second,
		Logf:            t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, root := range []string{rootA, rootB} {
		lease, err := pool.Acquire(ctx, Target{Path: filepath.Join(root, "main.go")})
		if err != nil {
			t.Fatal(err)
		}
		lease.Release()
	}

	closeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Close(closeCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	fleet.assertAllExited(t)

	for _, inst := range fleet.all() {
		notes := inst.script.Notifications()
		if !contains(notes, "exit") {
			t.Errorf("server for %s never received `exit`, got %v", inst.root, notes)
		}
	}
	if n := len(pool.Sessions()); n != 0 {
		t.Errorf("pool still holds %d sessions after Close", n)
	}
	if _, err := pool.Acquire(ctx, Target{Path: filepath.Join(rootA, "main.go")}); !errors.Is(err, ErrDaemonClosed) {
		t.Errorf("acquire after Close: got %v, want ErrDaemonClosed", err)
	}
	if err := pool.Close(closeCtx); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestPoolSessionStatus checks what `daemon status` will show.
func TestPoolSessionStatus(t *testing.T) {
	root := workspace(t)
	pool := newTestPool(t, &fakeFleet{}, PoolOptions{SessionIdleTimeout: -1})
	ctx := context.Background()

	lease, err := pool.Acquire(ctx, Target{Path: filepath.Join(root, "main.go")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Docs().Open(filepath.Join(root, "main.go")); err != nil {
		t.Fatalf("opening a document: %v", err)
	}

	inFlight := pool.Sessions()
	if len(inFlight) != 1 {
		t.Fatalf("got %d sessions, want 1", len(inFlight))
	}
	got := inFlight[0]
	if got.InFlight != 1 {
		t.Errorf("in flight %d, want 1", got.InFlight)
	}
	if got.Requests != 1 {
		t.Errorf("requests %d, want 1", got.Requests)
	}
	if got.OpenDocuments != 1 {
		t.Errorf("open documents %d, want 1", got.OpenDocuments)
	}
	if got.Root != root {
		t.Errorf("root %q, want %q", got.Root, root)
	}
	if got.RootMarker != "go.mod" {
		t.Errorf("root marker %q, want go.mod", got.RootMarker)
	}
	if got.Indexing {
		t.Error("session reported as indexing; the fake never sent progress")
	}
	if got.Starting {
		t.Error("session reported as starting after the handshake")
	}
	lease.Release()

	if idle := pool.Sessions()[0].InFlight; idle != 0 {
		t.Errorf("in flight %d after release, want 0", idle)
	}
}

// TestPoolNeedsRouter: a pool without a router cannot resolve anything,
// and saying so at construction beats failing on the first query.
func TestPoolNeedsRouter(t *testing.T) {
	if _, err := NewPool(PoolOptions{}); err == nil {
		t.Fatal("NewPool without a router succeeded")
	}
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// TestPoolCloseDuringColdStart: a server that is halfway through its
// handshake when the pool closes must still be shut down. Getting this
// wrong leaks a language server that nothing owns — on a real machine,
// a rust-analyzer that indexes a repository for nobody and is never
// reaped.
func TestPoolCloseDuringColdStart(t *testing.T) {
	root := workspace(t)
	fleet := &fakeFleet{startDelay: 300 * time.Millisecond}
	pool, err := NewPool(PoolOptions{
		Router:          testRouter(t),
		Launcher:        fleet.launcher(),
		Gate:            fastGate(),
		ShutdownTimeout: 2 * time.Second,
		Logf:            t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}

	acquired := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		lease, err := pool.Acquire(ctx, Target{Path: filepath.Join(root, "main.go")})
		if err == nil {
			lease.Release()
		}
		acquired <- err
	}()

	// Close while the handshake is still in flight.
	waitFor(t, "the server to start starting", func() bool { return fleet.spawnCount() == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := pool.Close(ctx); err != nil {
		t.Errorf("Close during a cold start: %v", err)
	}

	// The client either got a session (and released it) or was told
	// the pool is closed. Both are fine; a leaked server is not.
	if err := <-acquired; err != nil && !errors.Is(err, ErrDaemonClosed) {
		t.Errorf("acquire during a closing pool: %v", err)
	}
	fleet.assertAllExited(t)
	if n := len(pool.Sessions()); n != 0 {
		t.Errorf("pool still holds %d sessions", n)
	}
}
