package fakeserver

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// This file adds the scripted mode PLAN §7 asks for: a server that
// can be told to answer empty while still "indexing", to emit
// progress tokens it never ends, to use rust-analyzer's custom
// tokens, or to flap an answer before it settles. Serve (the M0
// fixed script) keeps working unchanged; Run is the general form.
//
// The frame codec below is deliberately a second, independent copy of
// the base protocol — the same reason the M0 codec is independent of
// internal/client: a framing bug on one side must not be cancelled
// out by the same bug on the other.

// Method handles one client request. Returning an error answers with
// a JSON-RPC error response; return an *Error to choose the code.
type Method func(c *Conn, params json.RawMessage) (any, error)

// Error is a JSON-RPC error a Method can return.
type Error struct {
	Code    int64
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("fakeserver: %d %s", e.Code, e.Message) }

// LSP error codes a scripted server may want to return. ContentModified
// is what a real server sends while its state is moving underneath a
// request, which is a readiness signal (PLAN §5.2).
const (
	CodeMethodNotFound  = -32601
	CodeContentModified = -32801
)

// Options is the script. The zero value behaves like Serve: empty
// capabilities and the echo method.
type Options struct {
	// Capabilities is the InitializeResult.capabilities object. Nil
	// means empty capabilities, i.e. a server that advertises
	// nothing.
	Capabilities map[string]any
	// ServerName is reported in InitializeResult.serverInfo.
	ServerName string
	// Methods handles requests, overriding the built-ins.
	Methods map[string]Method
	// OnStart runs once, before the first frame is read, with the
	// connection the script will use. Tests keep the handle to
	// inspect what the client sent back.
	OnStart func(c *Conn)
	// OnInitialize runs while answering `initialize`, before the
	// result is written: the moment a server may already create
	// progress tokens.
	OnInitialize func(c *Conn)
	// AfterInitialized runs when the client's `initialized`
	// notification arrives, on the read loop, so anything it writes
	// is guaranteed to reach the client before the answer to the
	// client's next request.
	AfterInitialized func(c *Conn)
	// OnNotification observes every other client notification.
	OnNotification func(c *Conn, method string, params json.RawMessage)
}

// ClientResponse is a response the client sent back to one of the
// server's requests. Tests assert on these to prove the client
// answered window/workDoneProgress/create instead of refusing it.
type ClientResponse struct {
	// Method is the server request this answers.
	Method string
	// Result is the client's result, if it succeeded.
	Result json.RawMessage
	// Error is the client's error object, if it refused.
	Error *Error
}

// Conn is the scripted server's side of the connection: it writes
// notifications and server-to-client requests, and records the
// client's answers.
type Conn struct {
	w io.Writer

	writeMu sync.Mutex

	mu        sync.Mutex
	nextID    int64
	sent      map[int64]string // request id → method
	responses []ClientResponse
	notified  []string
}

// Notify sends a notification to the client.
func (c *Conn) Notify(method string, params any) error {
	raw, err := marshal(params)
	if err != nil {
		return err
	}
	return c.writeFrame(frame{JSONRPC: "2.0", Method: method, Params: raw})
}

// Request sends a server-to-client request. It does not wait for the
// answer — the read loop records it — so a script may call it from
// inside a Method without deadlocking.
func (c *Conn) Request(method string, params any) error {
	raw, err := marshal(params)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.sent[id] = method
	c.mu.Unlock()

	idJSON := json.RawMessage(strconv.FormatInt(id, 10))
	return c.writeFrame(frame{JSONRPC: "2.0", ID: &idJSON, Method: method, Params: raw})
}

// CreateProgress sends window/workDoneProgress/create for a token,
// the way rust-analyzer announces a custom token before using it.
func (c *Conn) CreateProgress(token any) error {
	return c.Request("window/workDoneProgress/create", map[string]any{"token": token})
}

// ProgressBegin sends a $/progress begin.
func (c *Conn) ProgressBegin(token any, title string) error {
	return c.Notify("$/progress", map[string]any{
		"token": token,
		"value": map[string]any{"kind": "begin", "title": title},
	})
}

// ProgressReport sends a $/progress report.
func (c *Conn) ProgressReport(token any, message string, percentage int) error {
	return c.Notify("$/progress", map[string]any{
		"token": token,
		"value": map[string]any{"kind": "report", "message": message, "percentage": percentage},
	})
}

// ProgressEnd sends a $/progress end.
func (c *Conn) ProgressEnd(token any, message string) error {
	return c.Notify("$/progress", map[string]any{
		"token": token,
		"value": map[string]any{"kind": "end", "message": message},
	})
}

// ClientResponses returns the answers the client gave to the server's
// requests, in arrival order.
func (c *Conn) ClientResponses() []ClientResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]ClientResponse(nil), c.responses...)
}

// Notifications returns the methods of the notifications the client
// sent, in order — enough to assert on the didOpen/didClose
// lifecycle.
func (c *Conn) Notifications() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.notified...)
}

// Run serves a scripted session until the client sends `exit` or the
// input ends.
func Run(r io.Reader, w io.Writer, opts Options) error {
	c := &Conn{w: w, sent: map[int64]string{}}
	if opts.OnStart != nil {
		opts.OnStart(c)
	}
	br := bufio.NewReader(r)
	for {
		f, err := readAnyFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		switch {
		case f.ID != nil && f.Method == "": // response to our request
			c.recordResponse(f)
		case f.ID == nil: // notification
			c.mu.Lock()
			c.notified = append(c.notified, f.Method)
			c.mu.Unlock()
			if f.Method == "exit" {
				return nil
			}
			if f.Method == "initialized" && opts.AfterInitialized != nil {
				opts.AfterInitialized(c)
			}
			if opts.OnNotification != nil {
				opts.OnNotification(c, f.Method, f.Params)
			}
		default: // request
			if err := c.respond(f, opts); err != nil {
				return err
			}
		}
	}
}

// respond dispatches one client request and writes the answer.
func (c *Conn) respond(f frame, opts Options) error {
	resp := frame{JSONRPC: "2.0", ID: f.ID}
	result, err := c.dispatch(f, opts)
	switch {
	case err == nil:
		raw, merr := marshal(result)
		if merr != nil {
			return merr
		}
		if raw == nil {
			raw = json.RawMessage("null")
		}
		resp.Result = raw
	default:
		var scripted *Error
		if errors.As(err, &scripted) {
			resp.Error = &responseError{Code: scripted.Code, Message: scripted.Message}
		} else {
			resp.Error = &responseError{Code: -32603, Message: err.Error()}
		}
	}
	return c.writeFrame(resp)
}

// dispatch finds the handler for a request: the script first, then
// the built-in lifecycle and echo methods.
func (c *Conn) dispatch(f frame, opts Options) (any, error) {
	if m, ok := opts.Methods[f.Method]; ok {
		return m(c, f.Params)
	}
	switch f.Method {
	case "initialize":
		if opts.OnInitialize != nil {
			opts.OnInitialize(c)
		}
		caps := opts.Capabilities
		if caps == nil {
			caps = map[string]any{}
		}
		name := opts.ServerName
		if name == "" {
			name = "lightspeed-fakeserver"
		}
		return map[string]any{
			"capabilities": caps,
			"serverInfo":   map[string]any{"name": name, "version": "0.0.1"},
		}, nil
	case "shutdown":
		return nil, nil
	case EchoMethod:
		if len(f.Params) == 0 {
			return nil, nil
		}
		return f.Params, nil
	default:
		return nil, &Error{Code: CodeMethodNotFound, Message: "method not found: " + f.Method}
	}
}

func (c *Conn) recordResponse(f frame) {
	var id int64
	if err := json.Unmarshal(*f.ID, &id); err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	resp := ClientResponse{Method: c.sent[id], Result: f.Result}
	if f.Error != nil {
		resp.Error = &Error{Code: f.Error.Code, Message: f.Error.Message}
	}
	c.responses = append(c.responses, resp)
}

// frame is any JSON-RPC message, in either direction.
type frame struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *responseError   `json:"error,omitempty"`
}

func marshal(v any) (json.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	if raw, ok := v.(json.RawMessage); ok {
		return raw, nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("fakeserver: marshaling params: %w", err)
	}
	return b, nil
}

func (c *Conn) writeFrame(f frame) error {
	body, err := json.Marshal(f)
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

// readAnyFrame reads one Content-Length framed message of any kind.
func readAnyFrame(r *bufio.Reader) (frame, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return frame{}, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return frame{}, fmt.Errorf("fakeserver: malformed header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return frame{}, err
			}
		}
	}
	if length < 0 {
		return frame{}, errors.New("fakeserver: missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return frame{}, err
	}
	var f frame
	if err := json.Unmarshal(body, &f); err != nil {
		return frame{}, fmt.Errorf("fakeserver: bad JSON: %w", err)
	}
	return f, nil
}
