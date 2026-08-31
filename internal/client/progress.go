package client

import (
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// LSP progress method names.
const (
	methodProgress               = "$/progress"
	methodWorkDoneProgressCreate = "window/workDoneProgress/create"
)

// ProgressToken is the state of one work-done progress token.
type ProgressToken struct {
	// Token is the canonical string form of the LSP token, which may
	// be a string ("rustAnalyzer/Indexing") or a number.
	Token string
	// Title is the title reported by the begin notification.
	Title string
	// Message is the most recent report/end message.
	Message string
	// Percentage is the most recent reported percentage, -1 if none.
	Percentage int
	// Created is true if the server asked for the token with
	// window/workDoneProgress/create.
	Created bool
	// Begun is true once a $/progress begin arrived.
	Begun bool
	// Ended is true once a $/progress end arrived.
	Ended bool
	// Updated is when this token last changed.
	Updated time.Time
}

// active reports whether the token still represents outstanding work.
// A token the server created but never began counts as outstanding:
// the server announced work it has not retracted, and treating it as
// finished is exactly the optimism PLAN §5.2 warns about.
func (t *ProgressToken) active() bool { return !t.Ended }

// ProgressTracker follows $/progress and
// window/workDoneProgress/create so the readiness gate can tell
// "indexing" from "idle" (PLAN §5.2). It is safe for concurrent use;
// its notification hooks run on the connection's read loop and must
// stay cheap.
type ProgressTracker struct {
	clock Clock

	mu        sync.Mutex
	tokens    map[string]*ProgressToken
	order     []string
	seen      bool
	lastEvent time.Time
	events    int
}

// NewProgressTracker returns a tracker using clock as its time
// source; a nil clock means the system clock.
func NewProgressTracker(clock Clock) *ProgressTracker {
	if clock == nil {
		clock = SystemClock()
	}
	return &ProgressTracker{clock: clock, tokens: map[string]*ProgressToken{}}
}

// progressParams is the shared shape of $/progress and
// window/workDoneProgress/create parameters.
type progressParams struct {
	Token json.RawMessage `json:"token"`
	Value struct {
		Kind       string `json:"kind"`
		Title      string `json:"title"`
		Message    string `json:"message"`
		Percentage *int   `json:"percentage"`
	} `json:"value"`
}

// tokenKey canonicalizes an LSP ProgressToken (string | integer) to a
// map key. Numbers keep their JSON literal, which cannot collide with
// a quoted string's contents.
func tokenKey(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// HandleNotification feeds a server notification to the tracker. It
// ignores everything but $/progress, so it can be installed as the
// connection's only notification handler.
func (p *ProgressTracker) HandleNotification(method string, params json.RawMessage) {
	if method != methodProgress {
		return
	}
	var pp progressParams
	if err := json.Unmarshal(params, &pp); err != nil || len(pp.Token) == 0 {
		return // malformed: a server bug must not wedge the tracker
	}
	key := tokenKey(pp.Token)

	p.mu.Lock()
	defer p.mu.Unlock()
	tok := p.get(key)
	tok.Updated = p.clock.Now()
	switch pp.Value.Kind {
	case "begin":
		tok.Begun, tok.Ended = true, false
		tok.Title = pp.Value.Title
		tok.Message = pp.Value.Message
	case "report":
		tok.Message = pp.Value.Message
	case "end":
		tok.Ended = true
		if pp.Value.Message != "" {
			tok.Message = pp.Value.Message
		}
	default:
		// Unknown kind: record the event but do not guess at state.
	}
	if pp.Value.Percentage != nil {
		tok.Percentage = *pp.Value.Percentage
	}
	p.note()
}

// HandleCreate records a window/workDoneProgress/create request. It
// returns true if the request was a progress create (and has been
// accounted for), false for any other method.
func (p *ProgressTracker) HandleCreate(method string, params json.RawMessage) bool {
	if method != methodWorkDoneProgressCreate {
		return false
	}
	var pp progressParams
	if err := json.Unmarshal(params, &pp); err != nil || len(pp.Token) == 0 {
		return true // still ours; simply nothing to record
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tok := p.get(tokenKey(pp.Token))
	tok.Created = true
	tok.Updated = p.clock.Now()
	p.note()
	return true
}

// get returns the token state for key, creating it if new. Caller
// holds p.mu.
func (p *ProgressTracker) get(key string) *ProgressToken {
	tok, ok := p.tokens[key]
	if !ok {
		tok = &ProgressToken{Token: key, Percentage: -1}
		p.tokens[key] = tok
		p.order = append(p.order, key)
	}
	return tok
}

// note records that a progress event happened. Caller holds p.mu.
func (p *ProgressTracker) note() {
	p.seen = true
	p.events++
	p.lastEvent = p.clock.Now()
}

// Seen reports whether the server has ever used the progress
// protocol. A server that never does (PLAN §5.2's fallback case)
// gives the gate nothing to drain.
func (p *ProgressTracker) Seen() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.seen
}

// Drained reports whether progress was seen and every token the
// server announced has ended.
func (p *ProgressTracker) Drained() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seen {
		return false
	}
	for _, tok := range p.tokens {
		if tok.active() {
			return false
		}
	}
	return true
}

// Active lists the tokens that have not ended, in the order the
// server first mentioned them.
func (p *ProgressTracker) Active() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, key := range p.order {
		if p.tokens[key].active() {
			out = append(out, key)
		}
	}
	return out
}

// LastEvent reports when the last progress event arrived; the zero
// time if none ever did.
func (p *ProgressTracker) LastEvent() time.Time {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastEvent
}

// Events reports how many progress events have been processed.
func (p *ProgressTracker) Events() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.events
}

// Snapshot returns a copy of every token's state, ordered by first
// appearance, for diagnostics such as `lightspeed daemon status`.
func (p *ProgressTracker) Snapshot() []ProgressToken {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]ProgressToken, 0, len(p.order))
	for _, key := range p.order {
		out = append(out, *p.tokens[key])
	}
	return out
}

// Reset forgets all tokens. It exists for a session that has been
// re-initialized; the drained/seen state must not survive it.
func (p *ProgressTracker) Reset() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tokens = map[string]*ProgressToken{}
	p.order = nil
	p.seen = false
	p.events = 0
	p.lastEvent = time.Time{}
}

// sortedActive is Active, sorted, for stable error messages.
func (p *ProgressTracker) sortedActive() []string {
	active := p.Active()
	sort.Strings(active)
	return active
}
