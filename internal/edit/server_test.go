package edit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/fakeserver"
)

// This file closes the loop the rest of the package's tests open in
// the middle: the edit sets below arrive over a real LSP connection,
// as JSON, from a server that is trying to break things. PLAN §7 asks
// for a fake server that "emits a malicious overlapping
// WorkspaceEdit"; this is it.

// hostileRename answers textDocument/rename with whichever scripted
// edit set the requested new name selects.
func hostileRename(edits map[string]any) fakeserver.Method {
	return func(_ *fakeserver.Conn, params json.RawMessage) (any, error) {
		var p struct {
			NewName string `json:"newName"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		edit, ok := edits[p.NewName]
		if !ok {
			return nil, &fakeserver.Error{Code: fakeserver.CodeMethodNotFound, Message: "no such scenario"}
		}
		return edit, nil
	}
}

func rangeJSON(sl, sc, el, ec int) map[string]any {
	return map[string]any{
		"start": map[string]any{"line": sl, "character": sc},
		"end":   map[string]any{"line": el, "character": ec},
	}
}

func TestHostileServerEditsAreRefused(t *testing.T) {
	root := newTree(t, map[string]string{
		"a.go": "package a\n\nfunc Old() {}\n",
		"b.go": "package a\n\nfunc useB() { Old() }\n",
	})
	uri := string(uriOf(root, "a.go"))
	uriB := string(uriOf(root, "b.go"))
	before := snapshot(t, root)

	edit := func(u string, edits ...map[string]any) map[string]any {
		return map[string]any{"changes": map[string]any{u: edits}}
	}

	scenarios := map[string]any{
		// Two edits fighting over the same bytes, hidden behind a
		// perfectly good edit to another file.
		"overlap": map[string]any{"changes": map[string]any{
			uriB: []map[string]any{{"range": rangeJSON(2, 14, 2, 17), "newText": "New"}},
			uri: []map[string]any{
				{"range": rangeJSON(2, 5, 2, 13), "newText": "New() {}"},
				{"range": rangeJSON(2, 0, 2, 8), "newText": "func New"},
			},
		}},
		// A path that is nowhere near the workspace.
		"escape": edit("file:///etc/passwd",
			map[string]any{"range": rangeJSON(0, 0, 0, 1), "newText": "root::0:0:"}),
		// A range past the end of the file.
		"outofrange": edit(uri,
			map[string]any{"range": rangeJSON(99, 0, 99, 3), "newText": "New"}),
		// An edit that is not a text edit at all, and that the
		// generated union type would decode as a deletion.
		"snippet": edit(uri,
			map[string]any{"range": rangeJSON(2, 5, 2, 8), "snippet": map[string]any{
				"kind": "snippet", "value": "${1:New}",
			}}),
		// And one that is simply correct, so the test proves the
		// pipeline works rather than that it refuses everything.
		"honest": map[string]any{"documentChanges": []map[string]any{
			{
				"textDocument": map[string]any{"uri": uri, "version": nil},
				"edits":        []map[string]any{{"range": rangeJSON(2, 5, 2, 8), "newText": "New"}},
			},
			{
				"textDocument": map[string]any{"uri": uriB, "version": nil},
				"edits":        []map[string]any{{"range": rangeJSON(2, 14, 2, 17), "newText": "New"}},
			},
		}},
	}

	session, done := connectFake(t, root, fakeserver.Options{
		Capabilities: map[string]any{"renameProvider": true},
		Methods:      map[string]fakeserver.Method{"textDocument/rename": hostileRename(scenarios)},
	})
	defer done()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	call := func(scenario string) json.RawMessage {
		t.Helper()
		raw, err := session.Call(ctx, "textDocument/rename", map[string]any{
			"textDocument": map[string]any{"uri": uri},
			"position":     map[string]any{"line": 2, "character": 5},
			"newName":      scenario,
		})
		if err != nil {
			t.Fatalf("%s: rename request: %v", scenario, err)
		}
		return raw
	}

	for _, tc := range []struct{ scenario, want string }{
		{"overlap", "overlapping edits"},
		{"escape", "outside the workspace root"},
		{"outofrange", "outside the document"},
		{"snippet", "snippet"},
	} {
		t.Run(tc.scenario, func(t *testing.T) {
			we, err := Decode(call(tc.scenario))
			if err == nil {
				_, err = Stage(we, Options{Root: root})
			}
			if err == nil {
				t.Fatalf("%s: the edit was accepted", tc.scenario)
			}
			wantContains(t, err.Error(), tc.want)
			assertUnchanged(t, root, before)
		})
	}

	t.Run("honest", func(t *testing.T) {
		we, err := Decode(call("honest"))
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if _, err := Apply(we, Options{Root: root}); err != nil {
			t.Fatalf("Apply: %v", err)
		}
		if got, want := readFile(t, filepath.Join(root, "a.go")), "package a\n\nfunc New() {}\n"; got != want {
			t.Errorf("a.go = %q, want %q", got, want)
		}
		if got, want := readFile(t, filepath.Join(root, "b.go")), "package a\n\nfunc useB() { New() }\n"; got != want {
			t.Errorf("b.go = %q, want %q", got, want)
		}
	})
}

// connectFake starts a scripted server on a pipe pair and returns a
// connected session plus its teardown.
func connectFake(t *testing.T, root string, opts fakeserver.Options) (*client.Session, func()) {
	t.Helper()
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
		RootDir: root,
		Gate: client.GateOptions{
			Settle:          10 * time.Millisecond,
			NoProgressGrace: 10 * time.Millisecond,
			PollInterval:    time.Millisecond,
			Timeout:         5 * time.Second,
		},
	})
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	return session, func() {
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
	}
}
