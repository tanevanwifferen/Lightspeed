package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"
)

// Server is a language server subprocess speaking LSP over stdio.
type Server struct {
	*Conn
	cmd *exec.Cmd
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

// Initialize performs the LSP initialize handshake: an `initialize`
// request with minimal client capabilities followed by the
// `initialized` notification. It returns the raw InitializeResult.
func (s *Server) Initialize(ctx context.Context, rootDir string) (json.RawMessage, error) {
	rootURI := ""
	if rootDir != "" {
		rootURI = "file://" + rootDir
	}
	params := map[string]any{
		"processId":    os.Getpid(),
		"rootUri":      rootURI,
		"capabilities": map[string]any{},
	}
	result, err := s.Call(ctx, "initialize", params)
	if err != nil {
		return nil, fmt.Errorf("initialize: %w", err)
	}
	if err := s.Notify("initialized", map[string]any{}); err != nil {
		return nil, fmt.Errorf("initialized: %w", err)
	}
	return result, nil
}

// Shutdown performs the polite LSP exit sequence (shutdown request,
// exit notification) and reaps the subprocess, killing it if it does
// not leave within the context deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	// Best-effort politeness; a server that already died fails the
	// call, and the kill below still reaps it.
	if _, err := s.Call(ctx, "shutdown", nil); err == nil {
		_ = s.Notify("exit", nil)
	}

	waited := make(chan error, 1)
	go func() { waited <- s.cmd.Wait() }()
	select {
	case err := <-waited:
		return err
	case <-ctx.Done():
	case <-time.After(3 * time.Second):
	}
	_ = s.cmd.Process.Kill()
	return <-waited
}
