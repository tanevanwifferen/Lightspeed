// Package fakeserver is a hermetic scripted language server for tests
// (PLAN §7 tests item b). It speaks just enough LSP over an
// io.Reader/io.Writer pair to exercise the client: initialize
// handshake, shutdown/exit, a fake/echo method that returns its params
// verbatim, and MethodNotFound for everything else.
//
// The framing code here is deliberately independent of
// internal/client, so a framing bug in one side cannot cancel out the
// same bug in the other.
//
// Later milestones extend the script with the adversarial behaviours
// the plan calls for: answering empty while "indexing" and emitting
// overlapping WorkspaceEdits.
package fakeserver

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

type request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method,omitempty"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

type response struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id"`
	Result  json.RawMessage  `json:"result,omitempty"`
	Error   *responseError   `json:"error,omitempty"`
}

type responseError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
}

// EchoMethod returns its params verbatim as the result.
const EchoMethod = "fake/echo"

// Serve runs the scripted server until the client sends `exit` or the
// input stream ends.
func Serve(r io.Reader, w io.Writer) error {
	br := bufio.NewReader(r)
	for {
		req, err := readFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}

		if req.ID == nil { // notification
			if req.Method == "exit" {
				return nil
			}
			continue // initialized, didOpen, …: nothing to do
		}

		resp := response{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			resp.Result = json.RawMessage(`{"capabilities":{},"serverInfo":{"name":"lightspeed-fakeserver","version":"0.0.1"}}`)
		case "shutdown":
			resp.Result = json.RawMessage("null")
		case EchoMethod:
			if len(req.Params) == 0 {
				resp.Result = json.RawMessage("null")
			} else {
				resp.Result = req.Params
			}
		default:
			resp.Error = &responseError{Code: -32601, Message: fmt.Sprintf("method not found: %s", req.Method)}
		}
		if err := writeFrame(w, &resp); err != nil {
			return err
		}
	}
}

func readFrame(r *bufio.Reader) (*request, error) {
	length := -1
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			return nil, fmt.Errorf("fakeserver: malformed header %q", line)
		}
		if strings.EqualFold(strings.TrimSpace(name), "Content-Length") {
			length, err = strconv.Atoi(strings.TrimSpace(value))
			if err != nil {
				return nil, err
			}
		}
	}
	if length < 0 {
		return nil, errors.New("fakeserver: missing Content-Length")
	}
	body := make([]byte, length)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	req := new(request)
	if err := json.Unmarshal(body, req); err != nil {
		return nil, fmt.Errorf("fakeserver: bad JSON: %w", err)
	}
	return req, nil
}

func writeFrame(w io.Writer, resp *response) error {
	body, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "Content-Length: %d\r\n\r\n", len(body)); err != nil {
		return err
	}
	_, err = w.Write(body)
	return err
}
