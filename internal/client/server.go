package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"
)

// Server is a language server subprocess speaking LSP over stdio.
type Server struct {
	*Conn
	cmd *exec.Cmd

	waitOnce sync.Once
	waitDone chan struct{}
	waitErr  error
}

// StartCommand launches argv[0] with the remaining arguments as a
// stdio language server. The child's stderr is forwarded to stderr
// (server logs must never pollute the stdout JSON envelope).
func StartCommand(argv []string, stderr io.Writer) (*Server, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("empty server command")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stderr = stderr

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
	return &Server{Conn: NewConn(stdout, stdin), cmd: cmd}, nil
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
	_ = s.cmd.Process.Kill()
	<-s.waitDone
	return s.waitErr
}
