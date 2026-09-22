package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed mcp` — the command table as an MCP server over stdio.
//
// There is no per-tool code here. Every command with an MCP entry in the
// table (command.go) is registered as the tools that entry declares; the
// input schema is derived from its parameter spec (params.go); a call is
// turned into the argument vector the command's own flag parsing takes and
// run in this process, through the same `Run` the command line uses, against
// the same daemon. A command added to the table is a tool with nothing
// added here (docs/DECISIONS.md D20).

// mcpInstructions is what the server tells an agent about itself: the compact
// guide, generated from the command table (guide.go) so that it names only tools
// the server has. Short on purpose: it is in every session's context.
func mcpInstructions() string { return buildGuide(true, guideMCP) }

// mcpTool is one tool of the MCP surface: its declaration, and the command
// whose Run answers it.
type mcpTool struct {
	cmd  *command
	spec toolSpec
}

// mcpTools is every tool the command table declares, in table order.
func mcpTools() []mcpTool {
	var tools []mcpTool
	for _, c := range commands {
		for _, spec := range c.MCP {
			tools = append(tools, mcpTool{cmd: c, spec: spec})
		}
	}
	return tools
}

// definition is the tool as listed by tools/list. A tool with no --apply is
// marked read-only. Nothing is open-world: the only thing a tool reaches is
// the workspace and the language servers configured for it.
func (t mcpTool) definition() *mcp.Tool {
	closed := false
	return &mcp.Tool{
		Name:        t.spec.Name,
		Description: t.spec.description(),
		InputSchema: t.spec.inputSchema(),
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  !t.spec.mutates(),
			OpenWorldHint: &closed,
		},
	}
}

// An mcpServer runs tool calls. It holds nothing that outlives a call except
// the settings of the server: every call gets its own env, and so its own
// server-definition cache, because this process lives for a whole session and
// a `.lightspeed.toml` edited between two calls must be seen by the second
// (the staleness rule of docs/DECISIONS.md D17 is per command).
type mcpServer struct {
	// cwd is the directory a call's relative paths resolve against when
	// it names no workspace.
	cwd      string
	noDaemon bool
	offline  bool
	// stderr receives what the commands write for humans: usage text on a
	// bad flag, the note that a stale daemon was restarted, and every
	// language server's own stderr in process. It is the server's stderr,
	// which is where an MCP client looks for logs; stdout is the protocol.
	stderr io.Writer
	// life is the server's own lifetime: it is cancelled when the client
	// hangs up or a signal arrives. Every call runs under it as well as under
	// its request's context, because the SDK cancels a call's context only
	// for notifications/cancelled — on a hang-up it waits for the calls in
	// flight, so a call left to its own timeout would hold the whole process
	// (and its language servers) open for that long.
	life context.Context
	// active counts the calls that are running, so that a test can see a
	// cancelled one stop rather than inferring it from the client's side.
	active atomic.Int64
}

// newMCPServer builds the server for the settings in e, with every tool of
// the command table registered.
func newMCPServer(e *env, cwd string) *mcp.Server {
	server, _ := newMCPServerState(e, cwd)
	return server
}

// newMCPServerState is newMCPServer that also returns the state of the
// calls, for the test that has to see a cancelled call actually stop.
func newMCPServerState(e *env, cwd string) (*mcp.Server, *mcpServer) {
	s := &mcpServer{cwd: cwd, noDaemon: e.noDaemon, offline: e.offline, stderr: &lockedWriter{w: e.stderr}, life: e.base()}
	server := mcp.NewServer(
		&mcp.Implementation{Name: "lightspeed", Version: mcpVersion()},
		&mcp.ServerOptions{Instructions: mcpInstructions()},
	)
	for _, t := range mcpTools() {
		server.AddTool(t.definition(), func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.call(ctx, t, req.Params.Arguments), nil
		})
	}
	return server, s
}

// call runs one tool and returns its result. It never returns a protocol
// error: a failure of the command, of the arguments or of the tool itself is
// a tool result with isError set, which is what the model can read and act on.
func (s *mcpServer) call(ctx context.Context, t mcpTool, raw json.RawMessage) (res *mcp.CallToolResult) {
	s.active.Add(1)
	defer s.active.Add(-1)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer context.AfterFunc(s.life, cancel)()
	defer func() {
		// One misbehaving command must not take the session — and every
		// other tool the agent has — down with it.
		if r := recover(); r != nil {
			res = toolResult(failureEnvelope(render.Errorf(render.CodeInternal, "%s: panic: %v", t.spec.Name, r)), ExitCrash)
		}
	}()
	argv, err := t.spec.argv(raw, s.cwd)
	if err != nil {
		return toolResult(failureEnvelope(err), render.ExitCode(err))
	}
	var out strings.Builder
	// The request's context, which the SDK cancels on notifications/cancelled
	// and when the client goes away, is the context the command's own
	// requests run under: without it a call the client has given up on runs
	// to its --timeout.
	sub := &env{stdout: &out, stderr: s.stderr, noDaemon: s.noDaemon, offline: s.offline, ctx: ctx, overMCP: true}
	exit := t.cmd.Run(sub, t.cmd, argv)
	return toolResult(out.String(), exit)
}

// exitMeta is the key under which a result carries the exit code the same
// invocation would have had on the command line.
const exitMeta = "lightspeed/exit"

// toolResult wraps what a command wrote to stdout as a tool result: the
// envelope as text and as structured content, and isError exactly when the
// envelope says ok:false. Exit 1 with ok:true — an empty answer, a check that
// found errors — is an answer, not a failure of the tool.
//
// Every command is run with --format=json, so stdout is one envelope. Should
// it ever not be, the result says so rather than passing the bytes off as one.
func toolResult(stdout string, exit int) *mcp.CallToolResult {
	text := strings.TrimSpace(stdout)
	var probe struct {
		Version *int  `json:"version"`
		OK      *bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(text), &probe); err != nil || probe.Version == nil || probe.OK == nil {
		text = strings.TrimSpace(failureEnvelope(render.Errorf(render.CodeInternal,
			"the command exited %d without writing an envelope: %.200q", exit, text)))
		probe.OK = new(bool)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: text}},
		StructuredContent: json.RawMessage(text),
		IsError:           !*probe.OK,
		Meta:              mcp.Meta{exitMeta: exit},
	}
}

// failureEnvelope renders err as the failed envelope the CLI would print.
func failureEnvelope(err error) string {
	var b strings.Builder
	_ = render.FailError(&b, err)
	return b.String()
}

// mcpVersion is the version reported to clients.
func mcpVersion() string { return currentBuildInfo().Version }

// lockedWriter serialises writes from concurrent tool calls.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// hangUpReader calls hangUp when reading fails, which for a client's stdin is
// the client having gone: EOF, or the pipe broken.
type hangUpReader struct {
	r      io.Reader
	hangUp func()
}

func (h *hangUpReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if err != nil {
		h.hangUp()
	}
	return n, err
}

// nopWriteCloser is an io.WriteCloser that does not close what it wraps: the
// server writes to the process's stdout, which it does not own.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// mcpCommand implements `lightspeed mcp`: serve the tools until the client
// closes its end, or a signal arrives.
//
// stdout belongs to the protocol from the first byte, so nothing else may be
// written to it — which is why each call's command gets a buffer of its own
// as its stdout, and why the daemon it may spawn logs to a file.
func mcpCommand(e *env, c *command, args []string) int {
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "usage: lightspeed mcp [flags]\n\n%s\n\nflags:\n", c.Summary)
		fs.PrintDefaults()
	}
	noDaemon := fs.Bool("no-daemon", false,
		"run each call's language server inside this process instead of the workspace's shared daemon (env: "+noDaemonEnv+"=1)")
	offline := fs.Bool("offline", false, offlineUsage)
	if err := fs.Parse(args); err != nil {
		return e.flagError(render.Errorf(render.CodeUsage, "mcp: %v", err))
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return e.usagef("mcp: unexpected argument %q; the server takes no arguments", fs.Arg(0))
	}
	if e.stdin == nil {
		return e.usagef("mcp: no input stream")
	}
	cwd, err := os.Getwd()
	if err != nil {
		return e.fail(render.Errorf(render.CodeIOError, "mcp: the working directory: %v", err))
	}

	// The server's lifetime ends with a signal or with the client's end of
	// the pipe, and the calls in flight end with it (see mcpServer.life).
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, hangUp := context.WithCancel(ctx)
	defer hangUp()
	server := newMCPServer(&env{
		stderr:   e.stderr,
		noDaemon: e.noDaemon || *noDaemon,
		offline:  e.offline || *offline,
		ctx:      ctx,
	}, cwd)
	in := &hangUpReader{r: e.stdin, hangUp: hangUp}
	err = server.Run(ctx, &mcp.IOTransport{Reader: io.NopCloser(in), Writer: nopWriteCloser{e.stdout}})
	switch {
	case err == nil, errors.Is(err, context.Canceled), errors.Is(err, io.EOF):
		return ExitOK
	}
	fmt.Fprintf(e.stderr, "lightspeed mcp: %v\n", err)
	return ExitCrash
}
