package client

import (
	"strings"
	"testing"
	"time"
)

// The CLI renders these strings; PLAN §4 says an error is an envelope
// with a machine code and a message, never a stack trace. The message
// has to say what was being asked and what the server was busy with,
// because "not ready" on its own is not actionable.
func TestNotReadyErrorMessage(t *testing.T) {
	err := &NotReadyError{
		Method:   "textDocument/references",
		Reason:   NotReadyIndexing,
		Elapsed:  30 * time.Second,
		Attempts: 12,
		Active:   []string{"rustAnalyzer/Indexing"},
	}
	msg := err.Error()
	for _, want := range []string{
		"not ready",
		"textDocument/references",
		NotReadyIndexing,
		"30s",
		"12 attempt",
		"rustAnalyzer/Indexing",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
	if err.Code() != "not_ready" {
		t.Errorf("Code() = %q", err.Code())
	}
	if err.ExitCode() != 5 {
		t.Errorf("ExitCode() = %d, want 5", err.ExitCode())
	}

	// A gate that gave up before issuing anything still reads well.
	bare := (&NotReadyError{Reason: NotReadyIndexing, Elapsed: time.Second}).Error()
	if strings.Contains(bare, "for ") || !strings.Contains(bare, "0 attempt") {
		t.Errorf("bare message = %q", bare)
	}
}

func TestUnsupportedMethodErrorMessage(t *testing.T) {
	err := &UnsupportedMethodError{
		Method:     "textDocument/implementation",
		Capability: "implementationProvider",
		ServerName: "pyright",
	}
	msg := err.Error()
	for _, want := range []string{"pyright", "textDocument/implementation", "implementationProvider"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not mention %q", msg, want)
		}
	}
	if err.Code() != "no_capability" {
		t.Errorf("Code() = %q", err.Code())
	}
	if err.ExitCode() != 3 {
		t.Errorf("ExitCode() = %d, want 3", err.ExitCode())
	}

	// Unknown server name and no capability path still produce a
	// sentence.
	plain := (&UnsupportedMethodError{Method: "textDocument/hover"}).Error()
	if !strings.Contains(plain, "server does not support textDocument/hover") {
		t.Errorf("message = %q", plain)
	}
}

// TestSystemClock: the production clock is the one path the virtual
// clock never exercises.
func TestSystemClock(t *testing.T) {
	c := SystemClock()
	start := c.Now()
	<-c.After(time.Millisecond)
	if !c.Now().After(start) {
		t.Error("the system clock did not advance across a wait")
	}
	select {
	case <-c.After(0):
	case <-time.After(time.Second):
		t.Error("After(0) did not fire immediately")
	}
	select {
	case <-c.After(-time.Second):
	case <-time.After(time.Second):
		t.Error("After(negative) did not fire immediately")
	}
}
