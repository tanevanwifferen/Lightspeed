package client

import (
	"sync"
	"time"
)

// testClock is a virtual clock: it never sleeps. After advances the
// clock by the requested duration and fires immediately, so a gate
// configured with a 30s timeout and a 750ms settle window runs its
// whole schedule in microseconds. Every readiness test uses it, which
// is what makes PLAN §5.2's timeout paths deterministic instead of
// slow and flaky.
type testClock struct {
	mu    sync.Mutex
	now   time.Time
	waits int

	// onWait runs after each advance, so a test can make things
	// happen "while" the gate is waiting — a server that keeps
	// reporting progress, for instance.
	onWait func()
}

// clockEpoch is an arbitrary fixed start, so failures print stable
// timestamps.
var clockEpoch = time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)

func newTestClock() *testClock { return &testClock{now: clockEpoch} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	if d > 0 {
		c.now = c.now.Add(d)
	}
	c.waits++
	now := c.now
	onWait := c.onWait
	c.mu.Unlock()

	if onWait != nil {
		onWait()
	}

	ch := make(chan time.Time, 1)
	ch <- now
	return ch
}

// Advance moves the clock forward without a wait, for tests that need
// time to pass outside the gate's control.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// Elapsed reports virtual time since the epoch.
func (c *testClock) Elapsed() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now.Sub(clockEpoch)
}

// Waits reports how many times something waited on this clock.
func (c *testClock) Waits() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.waits
}
