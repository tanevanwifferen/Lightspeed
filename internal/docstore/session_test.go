package docstore_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
)

// This is the one docstore test that uses a real LSP conversation: it
// proves *client.Session satisfies docstore.Notifier and that the
// didOpen actually reaches a server — the precondition PLAN §5.4
// records as "documents must be didOpened before most servers
// answer". The scripted server enforces exactly that rule.

// openTracker is the server's view of which documents are open.
type openTracker struct {
	mu   sync.Mutex
	open map[string]bool
}

func (o *openTracker) observe(method string, params json.RawMessage) {
	var p struct {
		TextDocument struct {
			URI string `json:"uri"`
		} `json:"textDocument"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	switch method {
	case docstore.MethodDidOpen:
		o.open[p.TextDocument.URI] = true
	case docstore.MethodDidClose:
		delete(o.open, p.TextDocument.URI)
	}
}

func (o *openTracker) isOpen(uri string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.open[uri]
}

func TestStoreAgainstSession(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.go")
	if err := os.WriteFile(path, []byte("package main\n\nfunc 変数名() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	uri := string(protocol.URIFromPath(path))

	tracker := &openTracker{open: map[string]bool{}}
	opts := fakeserver.Options{
		Capabilities: map[string]any{"referencesProvider": true},
		OnNotification: func(_ *fakeserver.Conn, method string, params json.RawMessage) {
			tracker.observe(method, params)
		},
		Methods: map[string]fakeserver.Method{
			"textDocument/references": func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
				var p struct {
					TextDocument struct {
						URI string `json:"uri"`
					} `json:"textDocument"`
				}
				if err := json.Unmarshal(params, &p); err != nil {
					return nil, err
				}
				// The behaviour this whole package exists for: a
				// server that says nothing about a file it was never
				// told to open.
				if !tracker.isOpen(p.TextDocument.URI) {
					return []any{}, nil
				}
				return []any{map[string]any{
					"uri": p.TextDocument.URI,
					"range": map[string]any{
						"start": map[string]any{"line": 2, "character": 5},
						"end":   map[string]any{"line": 2, "character": 8},
					},
				}}, nil
			},
		},
	}

	clientR, serverW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	serverR, clientW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- fakeserver.Run(serverR, serverW, opts) }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	session, err := client.Connect(ctx, client.NewConn(clientR, clientW), client.SessionOptions{
		RootDir: dir,
		Gate: client.GateOptions{
			// Real time here: this test is about the document
			// lifecycle, not the gate's schedule, so keep the
			// windows short rather than virtual.
			Settle:          10 * time.Millisecond,
			NoProgressGrace: 10 * time.Millisecond,
			PollInterval:    time.Millisecond,
			Timeout:         5 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() {
		// A fresh context: the test's own ctx is cancelled by its
		// defer before cleanups run, and a cancelled shutdown leaves
		// the scripted server waiting forever.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		if err := session.Close(closeCtx); err != nil {
			t.Errorf("session close: %v", err)
		}
		select {
		case err := <-served:
			if err != nil {
				t.Errorf("fakeserver: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("fakeserver did not exit")
		}
		clientW.Close()
		serverW.Close()
		clientR.Close()
		serverR.Close()
	})

	store := docstore.New(session, docstore.Options{})

	// Position of 変数名 in byte columns, as the CLI's location
	// syntax gives it.
	pos, err := store.Position(path, 3, 6)
	if err != nil {
		t.Fatalf("Position: %v", err)
	}
	params := map[string]any{
		"textDocument": map[string]any{"uri": uri},
		"position":     pos,
		"context":      map[string]any{"includeDeclaration": true},
	}

	// Before didOpen the server answers nothing.
	res, err := session.Query(ctx, "textDocument/references", params)
	if err != nil {
		t.Fatalf("Query before open: %v", err)
	}
	if !res.Empty() {
		t.Fatalf("result before didOpen = %s, want empty", res.Result)
	}

	if _, err := store.Open(path); err != nil {
		t.Fatalf("Open: %v", err)
	}
	res, err = session.Query(ctx, "textDocument/references", params)
	if err != nil {
		t.Fatalf("Query after open: %v", err)
	}
	if res.Empty() {
		t.Fatal("result after didOpen is empty; the didOpen did not reach the server")
	}

	// And the result's UTF-16 range converts back to byte columns.
	var locations []protocol.Location
	if err := json.Unmarshal(res.Result, &locations); err != nil {
		t.Fatalf("decoding locations: %v", err)
	}
	if len(locations) != 1 {
		t.Fatalf("locations = %v", locations)
	}
	line, col8, err := store.LineCol8(locations[0].URI, locations[0].Range.Start)
	if err != nil {
		t.Fatalf("LineCol8: %v", err)
	}
	if line != 3 || col8 != 6 {
		t.Errorf("location = %d:%d, want 3:6", line, col8)
	}
	text, err := store.RangeText(locations[0].URI, locations[0].Range)
	if err != nil {
		t.Fatalf("RangeText: %v", err)
	}
	if string(text) != "変数名" {
		t.Errorf("RangeText = %q, want 変数名", text)
	}

	if err := store.CloseAll(); err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	// Give the notification time to land, then confirm the server
	// stopped considering the file open.
	deadline := time.Now().Add(2 * time.Second)
	for tracker.isOpen(uri) && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if tracker.isOpen(uri) {
		t.Error("the server still considers the document open after CloseAll")
	}
}
