package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
)

// Defaults for the readiness gate of PLAN §5.2.
const (
	// DefaultSettle is the stability window: a result must be
	// unchanged for this long before an unverified workspace is
	// allowed to answer. The 750ms is PLAN §5.2's number.
	DefaultSettle = 750 * time.Millisecond
	// DefaultNoProgressGrace is how long a server is given to start
	// using the progress protocol before we conclude it never will.
	DefaultNoProgressGrace = 500 * time.Millisecond
	// DefaultPollInterval is the delay between readiness polls and
	// between retries of an unstable request.
	DefaultPollInterval = 100 * time.Millisecond
	// DefaultTimeout bounds the whole gated request.
	DefaultTimeout = 30 * time.Second
)

// JSON-RPC error codes that mean "ask again later" rather than "no".
// A server that is still indexing answers with these instead of
// blocking, so they are readiness signals, not failures.
const (
	codeContentModified  = -32801
	codeServerCancelled  = -32802
	codeRequestCancelled = -32800
)

// How authority for a result was established. Reported in
// QueryResult.Ready so callers (and humans reading --format json) can
// see which of PLAN §5.2's rules fired.
const (
	// ReadyDrained: every progress token the server announced ended,
	// and the answer is non-empty.
	ReadyDrained = "progress_drained"
	// ReadyDrainedStable: progress drained and the answer was also
	// stable for the settle window. Required before an empty answer
	// is believed.
	ReadyDrainedStable = "progress_drained_stable"
	// ReadyNoProgress: the server never used the progress protocol,
	// so readiness was inferred from result stability alone.
	ReadyNoProgress = "stable_no_progress"
	// ReadyProgressStuck: progress was announced but never drained,
	// yet it has been quiet and the non-empty answer stable.
	ReadyProgressStuck = "stable_progress_stuck"
)

// Warnings attached to results whose authority is inferred rather
// than observed. They travel to the envelope's "warnings" array
// (PLAN §4) so an agent can see the answer is second-class.
const (
	warnNoProgress = "server never reported $/progress; readiness inferred from result stability"
	warnStuck      = "server still reports work in progress; result accepted because it stopped changing"
)

// GateOptions configures a Gate. Zero fields take the Default*
// values above.
type GateOptions struct {
	// Settle is the stability window.
	Settle time.Duration
	// NoProgressGrace is how long to wait for a first progress event
	// before treating the server as one that never sends any.
	NoProgressGrace time.Duration
	// PollInterval is the delay between readiness polls and retries.
	PollInterval time.Duration
	// Timeout bounds a whole gated request (PLAN §4's --timeout).
	Timeout time.Duration
	// Clock is the time source; nil means the system clock. Tests
	// inject a deterministic clock so no case has to sleep.
	Clock Clock
}

func (o GateOptions) withDefaults() GateOptions {
	if o.Settle <= 0 {
		o.Settle = DefaultSettle
	}
	if o.NoProgressGrace <= 0 {
		o.NoProgressGrace = DefaultNoProgressGrace
	}
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.Timeout <= 0 {
		o.Timeout = DefaultTimeout
	}
	if o.Clock == nil {
		o.Clock = SystemClock()
	}
	return o
}

// Gate is the readiness gate: it decides whether an answer from the
// server may be believed (PLAN §5.2). The failure it exists to
// prevent is a server that answers "0 references" while still
// indexing — an answer that looks authoritative and will make an
// agent delete live code.
//
// An answer is authoritative when one of these rules holds:
//
//	R1 progress drained and the answer is non-empty. A server that
//	   says it has finished its work is the strongest signal there
//	   is, so this answer is taken on the first attempt.
//	R2 progress drained and the answer was stable for Settle
//	   (required before an empty answer is believed).
//	R3 the server never sent progress at all, NoProgressGrace has
//	   passed, and the answer was stable for Settle. Warned about,
//	   because stability is all the evidence there is.
//	R4 progress was announced but never drained, no progress event
//	   arrived for Settle, the answer is non-empty and was stable for
//	   Settle. Warned about. Empty answers never qualify here.
//
// Anything else times out as a *NotReadyError (exit code 5) rather
// than returning a result of unknown authority.
type Gate struct {
	progress *ProgressTracker
	opts     GateOptions
	// created is when the session was established. The
	// no-progress grace is measured from here, because a server
	// that has been idle for a minute has already shown it does not
	// report progress.
	created time.Time
}

// NewGate returns a gate over a progress tracker. The tracker may be
// nil, which is the same as a server that never sends progress.
func NewGate(progress *ProgressTracker, opts GateOptions) *Gate {
	o := opts.withDefaults()
	if progress == nil {
		progress = NewProgressTracker(o.Clock)
	}
	return &Gate{progress: progress, opts: o, created: o.Clock.Now()}
}

// Options reports the gate's effective options.
func (g *Gate) Options() GateOptions { return g.opts }

// Progress reports the gate's progress tracker.
func (g *Gate) Progress() *ProgressTracker { return g.progress }

// QueryResult is a gated result together with the evidence for
// believing it.
type QueryResult struct {
	// Result is the raw JSON-RPC result.
	Result json.RawMessage
	// Ready is which rule established authority (a Ready* constant).
	Ready string
	// Warnings are the envelope warnings for an inferred readiness.
	Warnings []string
	// Attempts is how many times the request was issued.
	Attempts int
	// Waited is how long the gate held the request, including the
	// initial wait for progress to drain.
	Waited time.Duration
}

// Empty reports whether the result carries no data.
func (r QueryResult) Empty() bool { return IsEmptyResult(r.Result) }

// IsEmptyResult reports whether a JSON-RPC result is empty: absent,
// null, an empty array, an empty object or an empty string. These are
// the shapes an LSP server uses for "nothing found" — and the shapes
// PLAN §5.2 refuses to hand back without evidence.
func IsEmptyResult(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	switch string(trimmed) {
	case "", "null", "[]", "{}", `""`:
		return true
	}
	return false
}

// AwaitReady blocks until the server's announced progress set has
// drained, or until it is clear the server does not speak the
// progress protocol at all. It reports a *NotReadyError if neither
// happens before the gate's timeout — including the case where a
// token was announced and never ended, which is a stuck server, not a
// ready one.
//
// Query does not use this directly: a query may still proceed on a
// weaker signal (progress gone quiet) because rule R4 can rescue a
// stable non-empty answer. AwaitReady is the stricter public
// question, "is this workspace loaded".
func (g *Gate) AwaitReady(ctx context.Context) error {
	start := g.opts.Clock.Now()
	deadline := start.Add(g.opts.Timeout)
	for {
		now := g.opts.Clock.Now()
		switch {
		case g.progress.Drained():
			return nil
		case g.noProgress(now):
			return nil
		case !now.Before(deadline):
			return &NotReadyError{
				Reason:  NotReadyIndexing,
				Elapsed: now.Sub(start),
				Active:  g.progress.sortedActive(),
			}
		}
		if err := g.wait(ctx, g.opts.PollInterval, deadline); err != nil {
			return err
		}
	}
}

// noProgress reports whether the server has shown that it does not
// use the progress protocol: nothing seen, and the grace period since
// the session was established has passed.
func (g *Gate) noProgress(now time.Time) bool {
	return !g.progress.Seen() && now.Sub(g.created) >= g.opts.NoProgressGrace
}

// quiescent reports whether progress was announced, has not drained,
// and has been silent for the settle window: stuck tokens.
func (g *Gate) quiescent(now time.Time) bool {
	return g.progress.Seen() && !g.progress.Drained() &&
		now.Sub(g.progress.LastEvent()) >= g.opts.Settle
}

// awaitProceed holds the request until it is worth issuing at all:
// the progress set drained, the server showed it does not report
// progress, or progress went quiet for the settle window (stuck
// tokens, which rule R4 may still be able to work with). While a
// server is actively reporting progress the request is not issued —
// its answer could only be discarded.
func (g *Gate) awaitProceed(ctx context.Context, method string, start time.Time, deadline time.Time) error {
	for {
		now := g.opts.Clock.Now()
		switch {
		case g.progress.Drained(), g.noProgress(now), g.quiescent(now):
			return nil
		case !now.Before(deadline):
			return &NotReadyError{
				Method:  method,
				Reason:  NotReadyIndexing,
				Elapsed: now.Sub(start),
				Active:  g.progress.sortedActive(),
			}
		}
		if err := g.wait(ctx, g.opts.PollInterval, deadline); err != nil {
			return err
		}
	}
}

// callFunc issues the request being gated. It is a function rather
// than a method so the gate can be tested without a server, and so
// the same gate works for a subprocess server and a daemon-pooled
// one.
type callFunc func(ctx context.Context) (json.RawMessage, error)

// Query runs call under the readiness rules documented on Gate. It
// returns a *NotReadyError (exit code 5) rather than an answer whose
// authority it cannot establish.
func (g *Gate) Query(ctx context.Context, method string, call callFunc) (QueryResult, error) {
	// The timeout is per query, not per session: a session in the
	// daemon's pool serves many queries, and each gets its own
	// schedule.
	start := g.opts.Clock.Now()
	deadline := start.Add(g.opts.Timeout)

	if err := g.awaitProceed(ctx, method, start, deadline); err != nil {
		return QueryResult{Waited: g.opts.Clock.Now().Sub(start)}, err
	}

	var (
		last       json.RawMessage
		haveLast   bool
		lastChange = g.opts.Clock.Now()
		attempts   int
		retryable  error
	)
	for {
		result, err := call(ctx)
		attempts++
		switch {
		case err == nil:
			retryable = nil
		case isRetryable(err):
			// "Content modified" / "server cancelled": the server is
			// telling us its state moved under the request. That is
			// an indexing signal, not an answer — and it invalidates
			// whatever stability the previous answers had built up.
			retryable = err
			result = nil
			haveLast = false
			lastChange = g.opts.Clock.Now()
		default:
			return QueryResult{Attempts: attempts, Waited: g.opts.Clock.Now().Sub(start)}, err
		}

		now := g.opts.Clock.Now()
		if retryable == nil {
			if !haveLast || !bytes.Equal(last, result) {
				last, haveLast = result, true
				lastChange = now
			}
			stable := now.Sub(lastChange) >= g.opts.Settle
			if ready, reason, warnings := g.judge(result, stable); ready {
				return QueryResult{
					Result:   result,
					Ready:    reason,
					Warnings: warnings,
					Attempts: attempts,
					Waited:   now.Sub(start),
				}, nil
			}
		}

		if !now.Before(deadline) {
			return QueryResult{Attempts: attempts, Waited: now.Sub(start)},
				g.notReady(method, attempts, start, now, last, haveLast, retryable)
		}
		if err := g.wait(ctx, g.opts.PollInterval, deadline); err != nil {
			return QueryResult{Attempts: attempts, Waited: g.opts.Clock.Now().Sub(start)}, err
		}
	}
}

// judge applies the four authority rules. See the Gate doc comment.
func (g *Gate) judge(result json.RawMessage, stable bool) (ready bool, reason string, warnings []string) {
	var (
		empty   = IsEmptyResult(result)
		drained = g.progress.Drained()
		now     = g.opts.Clock.Now()
	)
	switch {
	case drained && !empty: // R1
		return true, ReadyDrained, nil
	case drained && stable: // R2
		return true, ReadyDrainedStable, nil
	case stable && g.noProgress(now): // R3
		return true, ReadyNoProgress, []string{warnNoProgress}
	case stable && !empty && g.quiescent(now): // R4
		return true, ReadyProgressStuck, []string{warnStuck}
	default:
		return false, "", nil
	}
}

// notReady builds the timeout error, choosing the reason that best
// explains why authority was never established.
func (g *Gate) notReady(method string, attempts int, start, now time.Time, last json.RawMessage, haveLast bool, retryable error) error {
	active := g.progress.sortedActive()
	reason := NotReadyUnstable
	switch {
	case len(active) > 0 || retryable != nil:
		reason = NotReadyIndexing
	case haveLast && IsEmptyResult(last):
		reason = NotReadyEmpty
	}
	return &NotReadyError{
		Method:   method,
		Reason:   reason,
		Elapsed:  now.Sub(start),
		Attempts: attempts,
		Active:   active,
	}
}

// wait sleeps for d, but never past the gate deadline, and returns
// early if ctx is done.
func (g *Gate) wait(ctx context.Context, d time.Duration, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if remaining := deadline.Sub(g.opts.Clock.Now()); remaining < d {
		d = remaining
	}
	timer := g.opts.Clock.After(d)
	select {
	case <-timer:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// isRetryable reports whether a server error means "the state moved,
// ask again" rather than "no".
func isRetryable(err error) bool {
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		return false
	}
	switch rpcErr.Code {
	case codeContentModified, codeServerCancelled, codeRequestCancelled:
		return true
	}
	return false
}
