package client

import (
	"encoding/json"
	"testing"
	"time"
)

// feed is a shorthand for a $/progress notification.
func feed(p *ProgressTracker, body string) {
	p.HandleNotification(methodProgress, json.RawMessage(body))
}

// TestProgressLifecycle walks a gopls-shaped progress set: a numeric
// token announced with window/workDoneProgress/create, then
// begin/report/end.
func TestProgressLifecycle(t *testing.T) {
	clock := newTestClock()
	p := NewProgressTracker(clock)

	if p.Seen() || p.Drained() {
		t.Fatal("a fresh tracker must be neither seen nor drained")
	}

	if !p.HandleCreate(methodWorkDoneProgressCreate, json.RawMessage(`{"token":1}`)) {
		t.Fatal("HandleCreate did not claim window/workDoneProgress/create")
	}
	if !p.Seen() {
		t.Error("Seen() = false after a create")
	}
	if p.Drained() {
		t.Error("Drained() = true with a created-but-unfinished token")
	}

	feed(p, `{"token":1,"value":{"kind":"begin","title":"Loading packages"}}`)
	feed(p, `{"token":1,"value":{"kind":"report","message":"3/10","percentage":30}}`)
	if p.Drained() {
		t.Error("Drained() = true mid-report")
	}
	if got := p.Active(); len(got) != 1 || got[0] != "1" {
		t.Errorf("Active() = %v, want [1]", got)
	}

	feed(p, `{"token":1,"value":{"kind":"end","message":"done"}}`)
	if !p.Drained() {
		t.Error("Drained() = false after the only token ended")
	}
	if got := p.Active(); len(got) != 0 {
		t.Errorf("Active() = %v, want none", got)
	}

	snap := p.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot() = %d tokens, want 1", len(snap))
	}
	tok := snap[0]
	if tok.Token != "1" || !tok.Created || !tok.Begun || !tok.Ended {
		t.Errorf("token state = %+v", tok)
	}
	if tok.Title != "Loading packages" || tok.Message != "done" || tok.Percentage != 30 {
		t.Errorf("token payload = %+v", tok)
	}
}

// TestProgressRustAnalyzerTokens: rust-analyzer announces custom
// string tokens rather than the numbers gopls uses, and runs several
// at once. The tracker must not drain until the last one ends.
func TestProgressRustAnalyzerTokens(t *testing.T) {
	p := NewProgressTracker(newTestClock())

	for _, token := range []string{"rustAnalyzer/Roots Scanned", "rustAnalyzer/Indexing"} {
		body, err := json.Marshal(map[string]any{"token": token})
		if err != nil {
			t.Fatal(err)
		}
		if !p.HandleCreate(methodWorkDoneProgressCreate, body) {
			t.Fatal("create not claimed")
		}
		feed(p, `{"token":`+quote(token)+`,"value":{"kind":"begin","title":"`+token+`"}}`)
	}
	if p.Drained() {
		t.Fatal("Drained() = true with two open rust-analyzer tokens")
	}

	feed(p, `{"token":"rustAnalyzer/Roots Scanned","value":{"kind":"end"}}`)
	if p.Drained() {
		t.Error("Drained() = true with one token still open")
	}
	if got := p.Active(); len(got) != 1 || got[0] != "rustAnalyzer/Indexing" {
		t.Errorf("Active() = %v, want [rustAnalyzer/Indexing]", got)
	}

	feed(p, `{"token":"rustAnalyzer/Indexing","value":{"kind":"end"}}`)
	if !p.Drained() {
		t.Error("Drained() = false after both tokens ended")
	}
}

// TestProgressTokenKindsDoNotCollide: a string token "1" and the
// numeric token 1 are different tokens on the wire, and ending one
// must not end the other.
func TestProgressTokenKindsDoNotCollide(t *testing.T) {
	p := NewProgressTracker(newTestClock())
	feed(p, `{"token":1,"value":{"kind":"begin"}}`)
	feed(p, `{"token":"one","value":{"kind":"begin"}}`)
	feed(p, `{"token":1,"value":{"kind":"end"}}`)

	if p.Drained() {
		t.Error("Drained() = true while the string token is still open")
	}
	if got := p.Active(); len(got) != 1 || got[0] != "one" {
		t.Errorf("Active() = %v, want [one]", got)
	}
}

// TestProgressRestart: a server may reuse a token for a new unit of
// work. A begin after an end reopens it.
func TestProgressRestart(t *testing.T) {
	p := NewProgressTracker(newTestClock())
	feed(p, `{"token":"t","value":{"kind":"begin"}}`)
	feed(p, `{"token":"t","value":{"kind":"end"}}`)
	if !p.Drained() {
		t.Fatal("Drained() = false after end")
	}
	feed(p, `{"token":"t","value":{"kind":"begin"}}`)
	if p.Drained() {
		t.Error("Drained() = true after the token was begun again")
	}
}

// TestProgressMalformed: a server bug must not wedge the tracker.
// Nothing here is recorded, and nothing panics.
func TestProgressMalformed(t *testing.T) {
	p := NewProgressTracker(newTestClock())
	feed(p, `not json`)
	feed(p, `{"value":{"kind":"begin"}}`) // no token
	p.HandleNotification("window/logMessage", json.RawMessage(`{"type":3,"message":"hi"}`))
	if p.HandleCreate("textDocument/publishDiagnostics", json.RawMessage(`{}`)) {
		t.Error("HandleCreate claimed a method that is not a progress create")
	}
	if p.Seen() {
		t.Errorf("Seen() = true after only malformed input; snapshot: %+v", p.Snapshot())
	}
}

// TestProgressLastEventAndReset check the bookkeeping the gate's
// quiescence rule depends on.
func TestProgressLastEventAndReset(t *testing.T) {
	clock := newTestClock()
	p := NewProgressTracker(clock)
	if !p.LastEvent().IsZero() {
		t.Error("LastEvent() is set before any event")
	}
	feed(p, `{"token":"t","value":{"kind":"begin"}}`)
	first := p.LastEvent()
	clock.Advance(2 * time.Second)
	feed(p, `{"token":"t","value":{"kind":"report"}}`)
	if !p.LastEvent().After(first) {
		t.Error("LastEvent() did not advance with the second event")
	}
	if p.Events() != 2 {
		t.Errorf("Events() = %d, want 2", p.Events())
	}

	p.Reset()
	if p.Seen() || p.Drained() || len(p.Snapshot()) != 0 || !p.LastEvent().IsZero() {
		t.Error("Reset() left state behind")
	}
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
