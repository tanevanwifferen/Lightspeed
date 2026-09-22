package client

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestServerReportsExitStatusAndStderr(t *testing.T) {
	var forwarded strings.Builder
	srv, err := StartCommand([]string{"sh", "-c", "echo oops >&2; exit 7"}, &forwarded)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Wait(ctx)

	rep := srv.Report()
	if !rep.Exited || rep.Code != 7 || rep.Status != "exit status 7" {
		t.Errorf("report = %+v, want exited with status 7", rep)
	}
	if rep.Stderr != "oops" {
		t.Errorf("stderr = %q, want oops", rep.Stderr)
	}
	if forwarded.String() != "oops\n" {
		t.Errorf("forwarded stderr = %q: the tail must not swallow what the caller asked to see", forwarded.String())
	}
}

func TestServerKilledByWaitIsNotReportedAsExited(t *testing.T) {
	srv, err := StartCommand([]string{"sh", "-c", "echo up >&2; sleep 30"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = srv.Wait(ctx)
	if rep := srv.Report(); rep.Exited {
		t.Errorf("a server Wait had to kill was reported as having exited: %+v", rep)
	}
}

func TestTailBufferKeepsTheEndAtALineStart(t *testing.T) {
	tb := &tailBuffer{max: 20}
	for i := 0; i < 10; i++ {
		_, _ = tb.Write([]byte("line " + string(rune('a'+i)) + "\n"))
	}
	got, dropped := tb.text()
	if !dropped || got != "line i\nline j" {
		t.Errorf("tail = %q (dropped %v), want the last whole lines", got, dropped)
	}
}
