// Package client implements the LSP side of lightspeed: a minimal
// JSON-RPC 2.0 connection with LSP base-protocol framing, plus the
// initialize/shutdown lifecycle. Capability tracking, $/progress and
// readiness gating (PLAN §5.2) arrive in M1.
package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// RPCError is a JSON-RPC 2.0 error object, returned by Call when the
// server answers with an error response.
type RPCError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("jsonrpc: %d %s", e.Code, e.Message)
}

// JSON-RPC 2.0 error codes used by this client.
const (
	codeMethodNotFound = -32601
)

// message is the wire form of any JSON-RPC 2.0 message.
type message struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *RPCError        `json:"error,omitempty"`
}

// ErrConnClosed is returned by calls whose connection closed before a
// response arrived.
var ErrConnClosed = errors.New("jsonrpc: connection closed")

// Conn is a JSON-RPC 2.0 connection over a byte stream with LSP
// base-protocol framing (Content-Length headers). It supports client
// requests and notifications; incoming server requests are refused
// with MethodNotFound and incoming server notifications are dropped,
// which is all the M0 `raw` command needs.
type Conn struct {
	w       io.Writer
	writeMu sync.Mutex

	mu      sync.Mutex
	nextID  int64
	pending map[int64]chan *message
	closed  bool
	err     error

	done chan struct{}
}

// NewConn starts a connection reading from r and writing to w. The
// read loop runs until r fails or is closed.
func NewConn(r io.Reader, w io.Writer) *Conn {
	c := &Conn{
		w:       w,
		pending: make(map[int64]chan *message),
		done:    make(chan struct{}),
	}
	go c.readLoop(bufio.NewReader(r))
	return c
}

// Done is closed when the read loop has exited (server hung up or the
// connection failed).
func (c *Conn) Done() <-chan struct{} { return c.done }

// Call sends a request and waits for the matching response or for ctx
// to be done. A server-reported error is returned as *RPCError.
func (c *Conn) Call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	raw, err := marshalParams(params)
	if err != nil {
		return nil, err
	}

	ch := make(chan *message, 1)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, c.closeErr()
	}
	c.nextID++
	id := c.nextID
	c.pending[id] = ch
	c.mu.Unlock()

	idJSON := json.RawMessage(strconv.FormatInt(id, 10))
	if err := c.write(&message{JSONRPC: "2.0", ID: &idJSON, Method: method, Params: raw}); err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}

	select {
	case resp, ok := <-ch:
		if !ok || resp == nil {
			return nil, c.closeErr()
		}
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	}
}

// Notify sends a notification (no response expected).
func (c *Conn) Notify(method string, params any) error {
	raw, err := marshalParams(params)
	if err != nil {
		return err
	}
	return c.write(&message{JSONRPC: "2.0", Method: method, Params: raw})
}

func marshalParams(params any) (json.RawMessage, error) {
	if params == nil {
		return nil, nil
	}
	if raw, ok := params.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshaling params: %w", err)
	}
	return b, nil
}

func (c *Conn) write(msg *message) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := fmt.Fprintf(c.w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = c.w.Write(body)
	return err
}

func (c *Conn) readLoop(r *bufio.Reader) {
	var err error
	for {
		var msg *message
		msg, err = readMessage(r)
		if err != nil {
			break
		}
		switch {
		case msg.ID != nil && msg.Method == "": // response
			var id int64
			if uerr := json.Unmarshal(*msg.ID, &id); uerr != nil {
				continue // non-numeric id; we never issue those
			}
			c.mu.Lock()
			ch := c.pending[id]
			delete(c.pending, id)
			c.mu.Unlock()
			if ch != nil {
				ch <- msg
			}
		case msg.ID != nil: // server-to-client request: refuse politely
			resp := &message{JSONRPC: "2.0", ID: msg.ID, Error: &RPCError{
				Code:    codeMethodNotFound,
				Message: fmt.Sprintf("method %q not supported by lightspeed client", msg.Method),
			}}
			if werr := c.write(resp); werr != nil {
				err = werr
			}
		default: // server notification: dropped in M0
		}
		if err != nil {
			break
		}
	}

	c.mu.Lock()
	c.closed = true
	if !errors.Is(err, io.EOF) {
		c.err = err
	}
	for id, ch := range c.pending {
		close(ch)
		delete(c.pending, id)
	}
	c.mu.Unlock()
	close(c.done)
}

func (c *Conn) closeErr() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return fmt.Errorf("%w: %v", ErrConnClosed, c.err)
	}
	return ErrConnClosed
}

// readMessage reads one Content-Length framed JSON-RPC message.
func readMessage(r *bufio.Reader) (*message, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break // end of headers
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("malformed header line %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, fmt.Errorf("malformed Content-Length: %w", err)
			}
		}
		// Content-Type and unknown headers are ignored.
	}
	if length < 0 {
		return nil, errors.New("missing Content-Length header")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	msg := new(message)
	if err := json.Unmarshal(body, msg); err != nil {
		return nil, fmt.Errorf("malformed JSON-RPC body: %w", err)
	}
	return msg, nil
}
