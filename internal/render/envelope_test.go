package render

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestEnvelopeShapeIsStable(t *testing.T) {
	var buf bytes.Buffer
	if err := OK(&buf, map[string]int{"n": 1}, "a warning"); err != nil {
		t.Fatal(err)
	}
	golden(t, "envelope_ok.json", buf.Bytes())

	buf.Reset()
	if err := Fail(&buf, CodeNoServer, "no server handles .zig files"); err != nil {
		t.Fatal(err)
	}
	golden(t, "envelope_fail.json", buf.Bytes())
}

func TestEnvelopeOmitsEmptyFields(t *testing.T) {
	var buf bytes.Buffer
	if err := OK(&buf, nil); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	if got != `{"version":1,"ok":true}` {
		t.Errorf("got %s, want no empty data/warnings/error keys", got)
	}
}

func TestFailErrorCarriesCodeAndDetails(t *testing.T) {
	err := Errorf(CodeServerNotInstalled, "gopls is not on PATH").
		WithDetails(map[string]string{"install": "mise use -g go:golang.org/x/tools/gopls@v0.23.0"})

	var buf bytes.Buffer
	if werr := FailError(&buf, err); werr != nil {
		t.Fatal(werr)
	}
	golden(t, "envelope_fail_details.json", buf.Bytes())

	var env Envelope
	if uerr := json.Unmarshal(buf.Bytes(), &env); uerr != nil {
		t.Fatal(uerr)
	}
	if env.OK {
		t.Error("ok is true on a failure")
	}
	if env.Error == nil || env.Error.Code != CodeServerNotInstalled {
		t.Fatalf("error = %+v", env.Error)
	}
	// The message must be the message, not a chain: PLAN §4 forbids
	// dumping internals into the contract.
	if strings.Contains(env.Error.Message, string(CodeServerNotInstalled)) {
		t.Errorf("message %q repeats the code", env.Error.Message)
	}
	if got := ExitCode(err); got != ExitNoServer {
		t.Errorf("exit = %d, want %d", got, ExitNoServer)
	}
}

func TestFailErrorClassifiesUncodedErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := FailError(&buf, errors.New("nil pointer somewhere")); err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env.Error.Code != CodeInternal {
		t.Errorf("code = %q, want %q", env.Error.Code, CodeInternal)
	}
	if env.Error.Message != "nil pointer somewhere" {
		t.Errorf("message = %q", env.Error.Message)
	}
}

func TestWriteEnvelopeReportsIOFailure(t *testing.T) {
	err := WriteEnvelope(failingWriter{}, Envelope{Version: 1, OK: true}, Options{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := CodeForError(err); got != CodeIOError {
		t.Errorf("code = %q, want %q", got, CodeIOError)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, os.ErrClosed }
