package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// StderrTailBytes bounds how much of a server's stderr is kept for a
// failure report: enough for a stack trace's head or a shim's complaint,
// small enough to ride in an error envelope an agent pays tokens for.
const StderrTailBytes = 2048

// Server is a language server subprocess speaking LSP over stdio.
type Server struct {
	*Conn
	cmd *exec.Cmd

	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error

	// tail keeps the last StderrTailBytes of the child's stderr, and killed
	// records that Wait had to kill it, so that a report can tell "it
	// exited" from "we ended it".
	tail   *tailBuffer
	killed atomic.Bool
}

// An ExitReport says how a server process ended and what it last said on
// stderr. It is what turns "connection closed" into a reason.
type ExitReport struct {
	// Exited is true when the process ended on its own — a crash, a bad
	// shim, a usage error — and false when it was still running and
	// Wait killed it (or Wait has not finished).
	Exited bool `json:"exited"`
	// Status is the process state as Go words it: "exit status 1",
	// "signal: killed". Empty unless Exited.
	Status string `json:"status,omitempty"`
	// Code is the exit code, or -1 when a signal ended the process.
	Code int `json:"code"`
	// Stderr is the tail of what the process wrote to stderr, at most
	// StderrTailBytes, cut at a line start where it was cut at all.
	Stderr string `json:"stderr,omitempty"`
	// StderrTruncated is true when the process wrote more than Stderr.
	StderrTruncated bool `json:"stderr_truncated,omitempty"`
}

// tailBuffer is an io.Writer that remembers only its last max bytes.
type tailBuffer struct {
	mu      sync.Mutex
	buf     []byte
	max     int
	dropped bool
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.dropped = true
	}
	return len(p), nil
}

// text is the kept tail, starting on a rune boundary and, when bytes were
// dropped, after the first newline so that it does not begin mid-line.
func (t *tailBuffer) text() (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.buf
	if t.dropped {
		if i := bytes.IndexByte(b, '\n'); i >= 0 && i+1 < len(b) {
			b = b[i+1:]
		}
	}
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return strings.TrimRight(string(b), "\r\n"), t.dropped
}

// StartCommand launches argv[0] with the remaining arguments as a
// stdio language server. The child's stderr is forwarded to stderr
// (server logs must never pollute the stdout JSON envelope).
func StartCommand(argv []string, stderr io.Writer) (*Server, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty server command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	tail := &tailBuffer{max: StderrTailBytes}
	if stderr != nil {
		cmd.Stderr = io.MultiWriter(stderr, tail)
	} else {
		cmd.Stderr = tail
	}
	// Copying stderr through this process means Wait would otherwise wait
	// for every descendant that inherited the pipe, and a language server's
	// children can outlive it.
	cmd.WaitDelay = time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting server %q: %w", argv[0], err)
	}
	return &Server{Conn: NewConn(stdout, stdin), cmd: cmd, tail: tail}, nil
}

// Initialize performs the LSP initialize handshake with lightspeed's
// default client capabilities: an `initialize` request followed by
// the `initialized` notification. It returns the raw
// InitializeResult. Use Connect instead when you want capability
// recording, progress tracking and the readiness gate.
func (s *Server) Initialize(ctx context.Context, rootDir string) (json.RawMessage, error) {
	return initialize(ctx, s.Conn, SessionOptions{RootDir: rootDir})
}

// Shutdown performs the polite LSP exit sequence (shutdown request,
// exit notification) and reaps the subprocess, killing it if it does
// not leave within the context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	// Best-effort politeness; a server that already died fails the
	// call, and the reap below still collects it.
	_ = shutdown(ctx, s.Conn)
	return s.Wait(ctx)
}

// Wait reaps the subprocess, killing it if it does not leave within
// the context deadline or three seconds, whichever comes first. It is
// the half of Shutdown that a caller which already closed the session
// itself still needs.
func (s *Server) Wait(ctx context.Context) error {
	s.waitOnce.Do(func() {
		s.waitDone = make(chan struct{})
		go func() {
			s.waitErr = s.cmd.Wait()
			close(s.waitDone)
		}()
	})
	select {
	case <-s.waitDone:
		return s.waitErr
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
	}
	s.killed.Store(true)
	_ = s.cmd.Process.Kill()
	<-s.waitDone
	return s.waitErr
}

// Report says how the process ended and what it last wrote to stderr. It
// is meaningful once Wait has returned; before that Exited is false.
func (s *Server) Report() ExitReport {
	r := ExitReport{Code: -1}
	r.Stderr, r.StderrTruncated = s.tail.text()
	if s.waitDone == nil {
		return r
	}
	select {
	case <-s.waitDone:
	default:
		return r
	}
	st := s.cmd.ProcessState
	if st == nil || s.killed.Load() {
		return r
	}
	r.Exited = true
	r.Status = st.String()
	r.Code = st.ExitCode()
	return r
}
