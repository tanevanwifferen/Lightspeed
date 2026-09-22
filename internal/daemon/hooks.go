package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

const (
	methodApplyEdit          = "workspace/applyEdit"
	methodPublishDiagnostics = "textDocument/publishDiagnostics"
)

// A PushedEdit is one workspace/applyEdit request a language server
// sent while a command that asked to collect them was running.
type PushedEdit struct {
	// Label is the server's description of the edit, for an error
	// message that has to say which of several it means.
	Label string `json:"label,omitempty"`
	// Edit is the WorkspaceEdit, verbatim.
	Edit json.RawMessage `json:"edit"`
}

// sessionHooks is what a pooled session records of the server's own
// traffic, because two of the things lightspeed asks of a server are
// not answered by a request at all:
//
//   - A code action that arrives as a command makes the server push
//     its edits back as workspace/applyEdit requests.
//   - Diagnostics arrive as textDocument/publishDiagnostics whenever
//     the server likes.
//
// When the CLI owned its session, both were handled by callbacks
// installed for the one command that needed them. A session shared
// between commands cannot have per-command callbacks, so the daemon
// records both, always, and a command asks for what it wants.
type sessionHooks struct {
	// collectMu serialises the requests that collect edits. Two
	// commands running server commands on one session at once could
	// not tell whose edits were whose, and refusing to interleave
	// them is cheaper than a wrong attribution.
	collectMu sync.Mutex

	mu     sync.Mutex
	armed  bool
	pushed []PushedEdit

	// diagnostics is the latest publish per file. A publish
	// *replaces* the file's set, per the protocol: a server clearing a
	// file's diagnostics sends an empty array, and appending would
	// make fixed errors immortal.
	diagnostics map[protocol.DocumentURI]json.RawMessage
	publishes   int
	lastPublish time.Time
}

func newSessionHooks() *sessionHooks {
	return &sessionHooks{diagnostics: map[protocol.DocumentURI]json.RawMessage{}}
}

// onRequest implements client.RequestHandler for workspace/applyEdit.
//
// A session that is not collecting refuses: lightspeed advertises
// workspace/applyEdit because a command it ran may need it, not as a
// standing invitation for a server to rewrite the tree. The edits are
// only recorded — nothing is written from the read loop, where this
// runs, and everything that is written goes through internal/edit.
func (h *sessionHooks) onRequest(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method != methodApplyEdit {
		return nil, fmt.Errorf("%w: %s", client.ErrMethodNotFound, method)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.armed {
		return map[string]any{
			"applied":       false,
			"failureReason": "lightspeed applies edits only as part of the command it was asked to run",
		}, nil
	}
	var req struct {
		Label string          `json:"label"`
		Edit  json.RawMessage `json:"edit"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return map[string]any{"applied": false, "failureReason": "malformed applyEdit params"}, nil
	}
	h.pushed = append(h.pushed, PushedEdit{Label: req.Label, Edit: req.Edit})
	// "applied" here means the client has taken responsibility for the
	// edit, which it has: it is staged and will be written or shown.
	// Saying false would make a server believe its own command failed.
	return map[string]any{"applied": true}, nil
}

// arm starts collecting pushed edits, discarding earlier ones, and
// returns the function that stops and hands them over.
func (h *sessionHooks) arm() (collect func() []PushedEdit) {
	h.collectMu.Lock()
	h.mu.Lock()
	h.armed, h.pushed = true, nil
	h.mu.Unlock()
	return func() []PushedEdit {
		h.mu.Lock()
		out := h.pushed
		h.armed, h.pushed = false, nil
		h.mu.Unlock()
		h.collectMu.Unlock()
		return out
	}
}

// onNotification implements client.NotificationHandler. It runs on the
// read loop and takes the lock only long enough to store.
func (h *sessionHooks) onNotification(method string, params json.RawMessage) {
	if method != methodPublishDiagnostics {
		return
	}
	var pp struct {
		URI         string          `json:"uri"`
		Diagnostics json.RawMessage `json:"diagnostics"`
	}
	if err := json.Unmarshal(params, &pp); err != nil || pp.URI == "" {
		return // a malformed notification must not wedge a wait
	}
	uri, err := protocol.ParseDocumentURI(pp.URI)
	if err != nil {
		return
	}
	if len(pp.Diagnostics) == 0 {
		pp.Diagnostics = json.RawMessage("[]")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.diagnostics[uri] = pp.Diagnostics
	h.lastPublish = time.Now()
	h.publishes++
}

// forget drops what the server last published about a file. It is
// called before a document is (re)sent to the server, so that "the
// server has published about this file" can only be satisfied by a
// publish that answers the content it now has — a warm session
// remembers diagnostics for text that has since changed, and counting
// those would be reporting on a file nobody looked at.
func (h *sessionHooks) forget(uri protocol.DocumentURI) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.diagnostics, uri)
}

// A DiagnosticsRequest asks what a session has been told about some
// files.
type DiagnosticsRequest struct {
	Target
	// URIs are the files the caller needs an answer about.
	URIs []string `json:"uris"`
	// Snapshot asks for everything published, not only the summary.
	Snapshot bool `json:"snapshot,omitempty"`
}

// DiagnosticsState is the evidence for `check`'s rule in push mode:
// which files the server has never mentioned, and whether it has
// stopped talking. See docs/DECISIONS.md D10.
type DiagnosticsState struct {
	// Missing are the requested files nothing has been published for,
	// in the order they were asked about.
	Missing []string `json:"missing,omitempty"`
	// Publishes is how many publishes the session has ever received.
	Publishes int `json:"publishes"`
	// Quiet is how long ago the last one arrived. It is measured
	// here, on one clock, rather than compared against a timestamp
	// from another process; it is meaningless when Publishes is 0,
	// and silence before the first word is not the same as silence
	// after the last.
	Quiet time.Duration `json:"quiet_ns"`
	// ByURI is the latest diagnostics array per file, when asked for.
	ByURI map[string]json.RawMessage `json:"by_uri,omitempty"`
}

func (h *sessionHooks) state(uris []string, snapshot bool) *DiagnosticsState {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := &DiagnosticsState{Publishes: h.publishes}
	if h.publishes > 0 {
		st.Quiet = time.Since(h.lastPublish)
	}
	asked := make(map[protocol.DocumentURI]bool, len(uris))
	for _, raw := range uris {
		uri, err := protocol.ParseDocumentURI(raw)
		if err != nil {
			st.Missing = append(st.Missing, raw)
			continue
		}
		asked[uri] = true
		if _, ok := h.diagnostics[uri]; !ok {
			st.Missing = append(st.Missing, raw)
		}
	}
	if snapshot {
		// Only the files asked about. A warm session has published for
		// every file any earlier command opened, about content it no
		// longer has open; handing those back would report a.go's old
		// problems in a run about b.go, which a fresh server would
		// never do.
		st.ByURI = make(map[string]json.RawMessage, len(asked))
		for uri := range asked {
			if diags, ok := h.diagnostics[uri]; ok {
				st.ByURI[string(uri)] = diags
			}
		}
	}
	return st
}
