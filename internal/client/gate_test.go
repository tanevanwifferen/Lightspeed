package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"
)

// PLAN §5.2 is the one genuinely new problem in the project and the
// worst failure mode available to us: a server that answers "0
// references" while still indexing looks authoritative and will make
// an agent delete live code. These tests pin down every rule the gate
// applies, on a virtual clock so none of them sleeps.

// gateFixture is a gate wired to a virtual clock and a real progress
// tracker, so the tests exercise the actual $/progress parsing too.
type gateFixture struct {
	gate     *Gate
	progress *ProgressTracker
	clock    *testClock
}

func newGateFixture(opts GateOptions) *gateFixture {
	clock := newTestClock()
	opts.Clock = clock
	if opts.Timeout == 0 {
		opts.Timeout = 5 * time.Second
	}
	progress := NewProgressTracker(clock)
	return &gateFixture{gate: NewGate(progress, opts), progress: progress, clock: clock}
}

// progress helpers feed the tracker the way a server would.
func (f *gateFixture) begin(token string) {
	f.progress.HandleNotification(methodProgress, json.RawMessage(
		fmt.Sprintf(`{"token":%q,"value":{"kind":"begin","title":"Indexing"}}`, token)))
}

func (f *gateFixture) end(token string) {
	f.progress.HandleNotification(methodProgress, json.RawMessage(
		fmt.Sprintf(`{"token":%q,"value":{"kind":"end"}}`, token)))
}

func (f *gateFixture) report(token string) {
	f.progress.HandleNotification(methodProgress, json.RawMessage(
		fmt.Sprintf(`{"token":%q,"value":{"kind":"report","message":"still going"}}`, token)))
}

func (f *gateFixture) create(token string) {
	f.progress.HandleCreate(methodWorkDoneProgressCreate, json.RawMessage(
		fmt.Sprintf(`{"token":%q}`, token)))
}

// constant answers every attempt with the same bytes.
func constant(s string) callFunc {
	return func(context.Context) (json.RawMessage, error) {
		return json.RawMessage(s), nil
	}
}

// counted wraps a callFunc and counts attempts.
func counted(fn callFunc, n *int) callFunc {
	return func(ctx context.Context) (json.RawMessage, error) {
		*n++
		return fn(ctx)
	}
}

// assertNotReady checks the error is the typed not-ready error with
// PLAN §4's exit code 5, which is the contract the CLI layer relies
// on (it type-asserts an anonymous interface{ ExitCode() int }).
func assertNotReady(t *testing.T, err error, wantReason string) *NotReadyError {
	t.Helper()
	if err == nil {
		t.Fatal("want a not-ready error, got nil")
	}
	if !errors.Is(err, ErrNotReady) {
		t.Errorf("errors.Is(err, ErrNotReady) = false, err = %v", err)
	}
	var nre *NotReadyError
	if !errors.As(err, &nre) {
		t.Fatalf("error is %T, want *NotReadyError: %v", err, err)
	}
	if ec, ok := err.(interface{ ExitCode() int }); !ok {
		t.Error("error does not implement ExitCode() int")
	} else if got := ec.ExitCode(); got != 5 {
		t.Errorf("ExitCode() = %d, want 5", got)
	}
	if wantReason != "" && nre.Reason != wantReason {
		t.Errorf("Reason = %q, want %q", nre.Reason, wantReason)
	}
	return nre
}

// TestGateEmptyWhileIndexingIsNotReady is the headline case: the
// server answers an empty list while a progress token is still open.
// The gate must refuse to hand that back.
func TestGateEmptyWhileIndexingIsNotReady(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: 5 * time.Second})
	f.begin("gopls/indexing")

	attempts := 0
	realStart := time.Now()
	res, err := f.gate.Query(context.Background(), "textDocument/references",
		counted(constant(`[]`), &attempts))

	nre := assertNotReady(t, err, NotReadyIndexing)
	if res.Result != nil {
		t.Errorf("result = %s, want nil: an empty answer of unknown authority must never be returned", res.Result)
	}
	if len(nre.Active) != 1 || nre.Active[0] != "gopls/indexing" {
		t.Errorf("Active = %v, want [gopls/indexing]", nre.Active)
	}
	if got, want := f.clock.Elapsed(), 5*time.Second; got < want {
		t.Errorf("virtual elapsed = %v, want at least the %v timeout", got, want)
	}
	if real := time.Since(realStart); real > 250*time.Millisecond {
		t.Errorf("test took %v of real time; the clock is not being injected", real)
	}
	if attempts == 0 {
		t.Error("the request was never issued")
	}
}

// TestGateNonEmptyAfterDrain: once every announced token has ended, a
// non-empty answer is authoritative on the first attempt (rule R1).
func TestGateNonEmptyAfterDrain(t *testing.T) {
	f := newGateFixture(GateOptions{})
	f.begin("gopls/indexing")
	f.end("gopls/indexing")

	attempts := 0
	res, err := f.gate.Query(context.Background(), "textDocument/references",
		counted(constant(`[{"uri":"file:///a.go"}]`), &attempts))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Ready != ReadyDrained {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyDrained)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1: a drained server needs no retries", attempts)
	}
	if len(res.Warnings) != 0 {
		t.Errorf("warnings = %v, want none for an observed-ready workspace", res.Warnings)
	}
	if f.clock.Elapsed() != 0 {
		t.Errorf("virtual elapsed = %v, want 0", f.clock.Elapsed())
	}
}

// TestGateEmptyAfterDrainNeedsStability: an empty answer is believed
// only after it has also held still for the settle window (rule R2).
// This is the "0 references is a real answer" path, and it must cost
// at least 750ms of evidence.
func TestGateEmptyAfterDrainNeedsStability(t *testing.T) {
	f := newGateFixture(GateOptions{})
	f.begin("gopls/indexing")
	f.end("gopls/indexing")

	res, err := f.gate.Query(context.Background(), "textDocument/references", constant(`[]`))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Ready != ReadyDrainedStable {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyDrainedStable)
	}
	if !res.Empty() {
		t.Errorf("result = %s, want the empty list", res.Result)
	}
	if got := f.clock.Elapsed(); got < DefaultSettle {
		t.Errorf("virtual elapsed = %v, want at least the %v settle window", got, DefaultSettle)
	}
}

// TestGateNoProgressAtAll covers the server that never speaks the
// progress protocol (rule R3): readiness can only be inferred from
// stability, so the answer comes back with a warning attached.
func TestGateNoProgressAtAll(t *testing.T) {
	f := newGateFixture(GateOptions{})

	res, err := f.gate.Query(context.Background(), "textDocument/references", constant(`[]`))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Ready != ReadyNoProgress {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyNoProgress)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != warnNoProgress {
		t.Errorf("warnings = %v, want the no-progress warning", res.Warnings)
	}
	if got := f.clock.Elapsed(); got < DefaultNoProgressGrace+DefaultSettle {
		t.Errorf("virtual elapsed = %v, want at least grace+settle (%v)",
			got, DefaultNoProgressGrace+DefaultSettle)
	}
}

// TestGateFlappingResult: with no progress protocol to lean on, the
// gate has nothing but stability to go by, so a result that keeps
// changing must keep it asking. Only the value that finally holds
// still for the settle window comes back.
func TestGateFlappingResult(t *testing.T) {
	f := newGateFixture(GateOptions{})

	answers := []string{`[]`, `[1]`, `[1,2]`}
	attempts := 0
	call := func(context.Context) (json.RawMessage, error) {
		i := attempts
		attempts++
		if i >= len(answers) {
			i = len(answers) - 1
		}
		return json.RawMessage(answers[i]), nil
	}

	res, err := f.gate.Query(context.Background(), "textDocument/references", call)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if string(res.Result) != `[1,2]` {
		t.Errorf("result = %s, want the settled value [1,2]", res.Result)
	}
	if res.Ready != ReadyNoProgress {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyNoProgress)
	}
	// Settling takes at least one full window of unchanged answers on
	// top of the three distinct ones.
	if want := 3 + int(DefaultSettle/DefaultPollInterval); attempts < want {
		t.Errorf("attempts = %d, want at least %d", attempts, want)
	}
}

// TestGateFlapAcceptedImmediatelyWhenDrained is the counterpart: once
// the server reports its work finished, its first non-empty answer is
// authoritative even though the next one would have differed. Rule R1
// is the fast path, and it is deliberate — waiting 750ms on a server
// that has told us it is done would tax every query in the project.
func TestGateFlapAcceptedImmediatelyWhenDrained(t *testing.T) {
	f := newGateFixture(GateOptions{})
	f.begin("indexing")
	f.end("indexing")

	attempts := 0
	call := func(context.Context) (json.RawMessage, error) {
		attempts++
		return json.RawMessage(fmt.Sprintf(`[%d]`, attempts)), nil
	}
	res, err := f.gate.Query(context.Background(), "textDocument/references", call)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if string(res.Result) != `[1]` || attempts != 1 {
		t.Errorf("result = %s after %d attempts, want [1] after 1", res.Result, attempts)
	}
}

// TestGateFlapNeverSettles: a result that never stops changing is
// never authoritative, however non-empty it looks.
func TestGateFlapNeverSettles(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: 2 * time.Second})

	n := 0
	call := func(context.Context) (json.RawMessage, error) {
		n++
		return json.RawMessage(fmt.Sprintf(`[%d]`, n)), nil
	}
	_, err := f.gate.Query(context.Background(), "textDocument/references", call)
	assertNotReady(t, err, NotReadyUnstable)
}

// TestGateStuckProgressNonEmpty covers rule R4: a token the server
// announced but never ends (a real rust-analyzer failure mode). Once
// progress has been silent for the settle window and a non-empty
// answer has held still, the answer is returned — with a warning, so
// the caller can see the authority was inferred.
func TestGateStuckProgressNonEmpty(t *testing.T) {
	f := newGateFixture(GateOptions{})
	f.begin("rustAnalyzer/Indexing")

	res, err := f.gate.Query(context.Background(), "textDocument/references",
		constant(`[{"uri":"file:///a.rs"}]`))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Ready != ReadyProgressStuck {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyProgressStuck)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != warnStuck {
		t.Errorf("warnings = %v, want the stuck-progress warning", res.Warnings)
	}
}

// TestGateStuckProgressEmpty is the other half of R4, and the reason
// it exists: an *empty* answer from a server with unfinished work is
// exactly the dangerous case, so it never qualifies.
func TestGateStuckProgressEmpty(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: 3 * time.Second})
	f.begin("rustAnalyzer/Indexing")

	res, err := f.gate.Query(context.Background(), "textDocument/references", constant(`[]`))
	assertNotReady(t, err, NotReadyIndexing)
	if res.Result != nil {
		t.Errorf("result = %s, want nil", res.Result)
	}
}

// TestGateCreatedTokenCountsAsWork: a token the server created with
// window/workDoneProgress/create but never began is still announced
// work. Treating it as finished is the optimism the gate exists to
// prevent.
func TestGateCreatedTokenCountsAsWork(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: 2 * time.Second})
	f.create("rustAnalyzer/Roots")

	_, err := f.gate.Query(context.Background(), "textDocument/references", constant(`[]`))
	nre := assertNotReady(t, err, NotReadyIndexing)
	if len(nre.Active) != 1 || nre.Active[0] != "rustAnalyzer/Roots" {
		t.Errorf("Active = %v, want [rustAnalyzer/Roots]", nre.Active)
	}
}

// TestGateDrainMidQuery: progress that drains while the gate is
// polling flips the verdict from not-ready to ready without any
// caller involvement.
func TestGateDrainMidQuery(t *testing.T) {
	f := newGateFixture(GateOptions{})
	f.begin("indexing")

	attempts := 0
	call := func(context.Context) (json.RawMessage, error) {
		attempts++
		if attempts == 3 {
			f.end("indexing") // indexing finishes between polls
		}
		if attempts < 3 {
			return json.RawMessage(`[]`), nil
		}
		return json.RawMessage(`[{"uri":"file:///a.go"}]`), nil
	}
	res, err := f.gate.Query(context.Background(), "textDocument/references", call)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Ready != ReadyDrained {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyDrained)
	}
}

// TestGateAwaitReadyHoldsUntilDrain: the public readiness question
// is answered "no" for a server whose announced work never finishes,
// even once that server has gone quiet.
func TestGateAwaitReadyHoldsUntilDrain(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: time.Second})
	f.begin("indexing")

	err := f.gate.AwaitReady(context.Background())
	assertNotReady(t, err, NotReadyIndexing)

	f2 := newGateFixture(GateOptions{Timeout: time.Second})
	f2.begin("indexing")
	f2.end("indexing")
	if err := f2.gate.AwaitReady(context.Background()); err != nil {
		t.Errorf("AwaitReady after drain: %v", err)
	}
	if f2.clock.Elapsed() != 0 {
		t.Errorf("AwaitReady waited %v on a drained server, want 0", f2.clock.Elapsed())
	}

	f3 := newGateFixture(GateOptions{Timeout: time.Second})
	if err := f3.gate.AwaitReady(context.Background()); err != nil {
		t.Errorf("AwaitReady on a server with no progress protocol: %v", err)
	}
}

// TestGateNoRequestWhileIndexing: while the server is actively
// reporting progress, the request is never issued at all. Its answer
// could only be discarded, and asking a busy server to compute
// something we will throw away is how you turn a 30s index into a
// 60s one.
func TestGateNoRequestWhileIndexing(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: time.Second})
	f.begin("indexing")
	// Every time the gate waits, the server reports more progress, so
	// it never goes quiet.
	f.clock.onWait = func() { f.report("indexing") }

	attempts := 0
	_, err := f.gate.Query(context.Background(), "textDocument/references",
		counted(constant(`[]`), &attempts))
	assertNotReady(t, err, NotReadyIndexing)
	if attempts != 0 {
		t.Errorf("attempts = %d, want 0: the request must not be issued at all", attempts)
	}
}

// TestGateContentModifiedIsRetried: -32801 means "my state moved
// under your request", which is an indexing signal rather than an
// answer.
func TestGateContentModifiedIsRetried(t *testing.T) {
	f := newGateFixture(GateOptions{})
	f.begin("indexing")
	f.end("indexing")

	attempts := 0
	call := func(context.Context) (json.RawMessage, error) {
		attempts++
		if attempts < 3 {
			return nil, &RPCError{Code: codeContentModified, Message: "content modified"}
		}
		return json.RawMessage(`[{"uri":"file:///a.go"}]`), nil
	}
	res, err := f.gate.Query(context.Background(), "textDocument/references", call)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if string(res.Result) != `[{"uri":"file:///a.go"}]` {
		t.Errorf("result = %s", res.Result)
	}
	if res.Attempts != 3 {
		t.Errorf("Attempts = %d, want 3", res.Attempts)
	}
}

// TestGateContentModifiedForever ends as not-ready, never as an
// answer.
func TestGateContentModifiedForever(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: 2 * time.Second})
	f.begin("indexing")
	f.end("indexing")

	call := func(context.Context) (json.RawMessage, error) {
		return nil, &RPCError{Code: codeContentModified, Message: "content modified"}
	}
	_, err := f.gate.Query(context.Background(), "textDocument/references", call)
	assertNotReady(t, err, NotReadyIndexing)
}

// TestGateEmptyShapesThatKeepChanging: a server that answers with a
// different flavour of nothing each time — null, [], {} — is never
// stable and never authoritative, and the error says the answer we
// could not verify was an empty one.
func TestGateEmptyShapesThatKeepChanging(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: 2 * time.Second})

	shapes := []string{`[]`, `{}`, `null`, `""`}
	n := 0
	call := func(context.Context) (json.RawMessage, error) {
		s := shapes[n%len(shapes)]
		n++
		return json.RawMessage(s), nil
	}
	res, err := f.gate.Query(context.Background(), "textDocument/references", call)
	assertNotReady(t, err, NotReadyEmpty)
	if res.Result != nil {
		t.Errorf("result = %s, want nil", res.Result)
	}
}

// TestGateContentModifiedResetsStability: the server telling us its
// state moved invalidates the evidence gathered before it. Otherwise
// a result that was stable, then contradicted, then repeated once,
// would be accepted on the strength of the stale window.
func TestGateContentModifiedResetsStability(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: 5 * time.Second})

	n := 0
	call := func(context.Context) (json.RawMessage, error) {
		n++
		if n == 5 {
			return nil, &RPCError{Code: codeContentModified, Message: "content modified"}
		}
		return json.RawMessage(`[]`), nil
	}
	res, err := f.gate.Query(context.Background(), "textDocument/references", call)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !res.Empty() {
		t.Fatalf("result = %s", res.Result)
	}
	// Stability had to be rebuilt from scratch after attempt 5, so
	// the whole settle window must fall after it.
	if want := 5 + int(DefaultSettle/DefaultPollInterval); res.Attempts < want {
		t.Errorf("Attempts = %d, want at least %d", res.Attempts, want)
	}
}

// TestGateRealErrorPropagates: a genuine server error is an answer of
// its own kind and must not be retried into a not-ready timeout.
func TestGateRealErrorPropagates(t *testing.T) {
	f := newGateFixture(GateOptions{})
	f.begin("indexing")
	f.end("indexing")

	want := &RPCError{Code: -32602, Message: "invalid params"}
	attempts := 0
	call := func(context.Context) (json.RawMessage, error) {
		attempts++
		return nil, want
	}
	_, err := f.gate.Query(context.Background(), "textDocument/references", call)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != want.Code {
		t.Fatalf("err = %v, want the server's own error", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if ec, ok := err.(interface{ ExitCode() int }); !ok || ec.ExitCode() != 1 {
		t.Errorf("server error exit code = %v, want 1 (problems found)", err)
	}
}

// TestGateContextCancellation: a cancelled context beats the gate's
// own schedule, and reports the context's error rather than a
// not-ready verdict the user did not wait for.
func TestGateContextCancellation(t *testing.T) {
	f := newGateFixture(GateOptions{})
	f.begin("indexing")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := f.gate.Query(ctx, "textDocument/references", constant(`[]`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestGateReusedAcrossQueries: in the daemon (PLAN §3) one session
// serves query after query, possibly minutes apart. The timeout is
// per query, so a gate that measured it from session start would fail
// the second query instantly.
func TestGateReusedAcrossQueries(t *testing.T) {
	f := newGateFixture(GateOptions{Timeout: 2 * time.Second})
	f.begin("indexing")
	f.end("indexing")

	answer := constant(`[{"uri":"file:///a.go"}]`)
	if _, err := f.gate.Query(context.Background(), "textDocument/references", answer); err != nil {
		t.Fatalf("first query: %v", err)
	}

	f.clock.Advance(10 * time.Minute) // the session sits in the pool

	res, err := f.gate.Query(context.Background(), "textDocument/references", answer)
	if err != nil {
		t.Fatalf("second query after an idle session: %v", err)
	}
	if res.Waited > time.Second {
		t.Errorf("Waited = %v, want the second query's own elapsed time", res.Waited)
	}

	// And a query that does time out gets its own full budget rather
	// than the session's age.
	f.begin("reindexing")
	f.clock.onWait = func() { f.report("reindexing") }
	_, err = f.gate.Query(context.Background(), "textDocument/references", constant(`[]`))
	nre := assertNotReady(t, err, NotReadyIndexing)
	if nre.Elapsed < 2*time.Second || nre.Elapsed > 3*time.Second {
		t.Errorf("Elapsed = %v, want about the %v per-query timeout", nre.Elapsed, 2*time.Second)
	}
}

// TestGateGraceMeasuredFromSession: a server that has been silent
// since the handshake has already proved it does not report
// progress, so a later query must not pay the grace period again.
func TestGateGraceMeasuredFromSession(t *testing.T) {
	f := newGateFixture(GateOptions{})
	f.clock.Advance(DefaultNoProgressGrace)

	before := f.clock.Elapsed()
	res, err := f.gate.Query(context.Background(), "textDocument/references",
		constant(`[{"uri":"file:///a.go"}]`))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if res.Ready != ReadyNoProgress {
		t.Errorf("Ready = %q, want %q", res.Ready, ReadyNoProgress)
	}
	// Only the settle window should have been spent, not grace again.
	if spent := f.clock.Elapsed() - before; spent > DefaultSettle+DefaultPollInterval {
		t.Errorf("query spent %v, want about the %v settle window", spent, DefaultSettle)
	}
}

// TestIsEmptyResult pins the shapes a server uses for "nothing
// found"; misjudging one of these is a wrong verdict in both
// directions.
func TestIsEmptyResult(t *testing.T) {
	empty := []string{"", "null", "[]", "{}", `""`, "  []  ", "\n{}\n"}
	for _, s := range empty {
		if !IsEmptyResult(json.RawMessage(s)) {
			t.Errorf("IsEmptyResult(%q) = false, want true", s)
		}
	}
	nonEmpty := []string{`[{}]`, `{"a":1}`, `"x"`, `0`, `false`, `[null]`}
	for _, s := range nonEmpty {
		if IsEmptyResult(json.RawMessage(s)) {
			t.Errorf("IsEmptyResult(%q) = true, want false", s)
		}
	}
}

// TestGateNeverReturnsUnverifiedEmpty is the invariant PLAN §5.2
// states in capitals: across every readiness state, an empty result
// is either backed by evidence or replaced by exit 5. Nothing in
// between.
func TestGateNeverReturnsUnverifiedEmpty(t *testing.T) {
	states := []struct {
		name    string
		setup   func(f *gateFixture)
		wantErr bool
	}{
		{"never drains", func(f *gateFixture) { f.begin("t") }, true},
		{"created but never begun", func(f *gateFixture) { f.create("t") }, true},
		{"drained", func(f *gateFixture) { f.begin("t"); f.end("t") }, false},
		{"no progress ever", func(f *gateFixture) {}, false},
		{"one of two tokens ends", func(f *gateFixture) {
			f.begin("a")
			f.begin("b")
			f.end("a")
		}, true},
	}
	for _, st := range states {
		t.Run(st.name, func(t *testing.T) {
			f := newGateFixture(GateOptions{Timeout: 3 * time.Second})
			st.setup(f)
			res, err := f.gate.Query(context.Background(), "textDocument/references", constant(`[]`))
			switch {
			case st.wantErr:
				assertNotReady(t, err, "")
				if res.Result != nil {
					t.Errorf("result = %s, want nil alongside the error", res.Result)
				}
			case err != nil:
				t.Fatalf("Query: %v", err)
			case !res.Empty():
				t.Errorf("result = %s, want the empty answer", res.Result)
			case res.Ready == "":
				t.Error("an accepted answer must record why it was believed")
			}
		})
	}
}
