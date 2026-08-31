package client

import "time"

// Clock is the time source used by the readiness gate. Tests inject a
// deterministic implementation so the settle window and the timeout
// path can be exercised without real sleeps (PLAN §5.2 deserves the
// most test effort in the project, and a test suite that waits 750ms
// per case does not get run).
type Clock interface {
	// Now reports the current time.
	Now() time.Time
	// After returns a channel that receives once d has elapsed. A
	// non-positive d must fire immediately.
	After(d time.Duration) <-chan time.Time
}

// SystemClock returns the real wall clock.
func SystemClock() Clock { return systemClock{} }

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

func (systemClock) After(d time.Duration) <-chan time.Time {
	if d <= 0 {
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		return ch
	}
	return time.After(d)
}
