package daemon

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// docs/DECISIONS.md D29.

func TestBuildDiffers(t *testing.T) {
	self := Build{Executable: "/a/lightspeed", Size: 100, ModTimeNS: 5, Protocol: ProtocolVersion}
	cases := []struct {
		name  string
		other Build
		want  string // substring of the reason; "" means the same build
	}{
		{"same", self, ""},
		{"same binary under another name", Build{Executable: "/b/ls", Size: 100, ModTimeNS: 5, Protocol: ProtocolVersion}, ""},
		{"rebuilt: new mtime", Build{Executable: "/a/lightspeed", Size: 100, ModTimeNS: 6, Protocol: ProtocolVersion}, "different lightspeed executable"},
		{"rebuilt: new size", Build{Executable: "/a/lightspeed", Size: 101, ModTimeNS: 5, Protocol: ProtocolVersion}, "different lightspeed executable"},
		{"other protocol", Build{Executable: "/a/lightspeed", Size: 100, ModTimeNS: 5, Protocol: ProtocolVersion + 1}, "protocol version"},
		{"a daemon from before build identity", Build{}, "predates build identity"},
	}
	for _, tc := range cases {
		got := self.Differs(tc.other)
		if (tc.want == "") != (got == "") || !strings.Contains(got, tc.want) {
			t.Errorf("%s: Differs = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestSelfBuildIsCurrentAndIdentifiesTheProtocol(t *testing.T) {
	b := SelfBuild()
	if b.Protocol != ProtocolVersion || b.Size == 0 || b.ModTimeNS == 0 || b.Executable == "" {
		t.Errorf("SelfBuild = %+v, want the executable's path, size, mtime and the protocol version", b)
	}
	if !b.Current() {
		t.Error("the running test binary is not its own current executable")
	}
	if got := (Build{Executable: "/nonexistent/lightspeed"}).Current(); !got {
		t.Error("a build that could not be read must never claim to be out of date")
	}
}

func TestHandshakeAndStatusCarryTheBuild(t *testing.T) {
	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{})
	c := ts.dial(t)
	hs, err := c.Handshake(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for name, b := range map[string]Build{"handshake": hs.Build, "status": st.Build} {
		if why := SelfBuild().Differs(b); why != "" {
			t.Errorf("%s build differs from this process's own: %s", name, why)
		}
	}
}

// A daemon whose executable has been replaced leaves when it is idle, long
// before its listen timeout, and not while a client is connected.
func TestServerExitsWhenItsExecutableIsReplacedAndItIsIdle(t *testing.T) {
	var current atomic.Bool
	current.Store(true)
	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{
		ListenTimeout:     time.Hour,
		ExecutableCheck:   10 * time.Millisecond,
		ExecutableCurrent: current.Load,
	})
	c := ts.dial(t)

	current.Store(false)
	select {
	case err := <-ts.serveErr:
		t.Fatalf("the daemon left with a client connected: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	_ = c.Close()
	if err := ts.waitServe(t); !errors.Is(err, ErrExecutableReplaced) {
		t.Errorf("Serve returned %v, want ErrExecutableReplaced", err)
	}
}

func TestServerStaysWhileItsExecutableIsCurrent(t *testing.T) {
	ts := newTestServer(t, nil, PoolOptions{}, ServerOptions{
		ListenTimeout:     time.Hour,
		ExecutableCheck:   10 * time.Millisecond,
		ExecutableCurrent: func() bool { return true },
	})
	select {
	case err := <-ts.serveErr:
		t.Fatalf("an idle daemon with a current executable left: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
}
