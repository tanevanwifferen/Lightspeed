package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	"github.com/tanevanwifferen/Lightspeed/internal/edit"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// The LSP methods behind PLAN §4's mutation command surface. As with
// the read-only methods in lsp.go, they are named once so the command
// table, the capability guard and the request cannot disagree.
const (
	methodRename            = "textDocument/rename"
	methodPrepareRename     = "textDocument/prepareRename"
	methodCodeAction        = "textDocument/codeAction"
	methodCodeActionResolve = "codeAction/resolve"
	methodFormatting        = "textDocument/formatting"
	methodExecuteCommand    = "workspace/executeCommand"
	methodApplyEdit         = "workspace/applyEdit"
)

// mutationFlags are the two flags every command that can write shares.
//
// Writing is opt-in: PLAN §4 says rename previews by default, and the
// same rule is applied to every mutation, because a command that
// writes when the caller only wanted to look is the one mistake an
// agent cannot undo by re-reading the output.
type mutationFlags struct {
	apply      bool
	allowDirty bool
}

func (f *mutationFlags) register(fs *flag.FlagSet) {
	fs.BoolVar(&f.apply, "apply", false, "write the edits to disk (default: preview only)")
	fs.BoolVar(&f.allowDirty, "allow-dirty", false,
		"allow --apply even though the git worktree has uncommitted changes")
}

// mutationFormat resolves --format for a command whose answer is a set
// of edits.
//
// With no --format at all an edit *preview* is a unified diff: PLAN §4
// makes diff the default for the rename preview, and the same reason —
// the useful thing to look at, and the thing `git apply` consumes —
// holds for every preview of edits. Once --apply has written the
// files the answer is a report rather than a patch, so the ordinary
// json/tty default applies.
//
// sarif is rejected here rather than by the renderer so that the
// refusal costs no language server.
func mutationFormat(common *commonFlags, w io.Writer, previewing bool) (render.Format, error) {
	if common.format == "" && previewing {
		return render.FormatDiff, nil
	}
	f, err := render.ResolveFormat(common.format, w)
	if err != nil {
		return "", err
	}
	switch f {
	case render.FormatJSON, render.FormatText, render.FormatDiff:
		return f, nil
	default:
		return "", render.Errorf(render.CodeUnsupportedFormat,
			"format %q has no meaning for an edit (want one of json, text, diff)", f)
	}
}

// mutationCapabilities are the client capabilities a mutation command
// advertises, on top of the read-only defaults.
//
// They are deliberately not in client.DefaultClientCapabilities: a
// capability is a promise, and `references` cannot honour a
// workspace/applyEdit request or a resource operation. Advertising
// them only here means a server sends the richer edit shapes exactly
// to the commands that can apply them transactionally — and
// failureHandling "transactional" is not a hopeful claim, it is what
// internal/edit implements (PLAN §5.3).
func mutationCapabilities() map[string]any {
	caps := client.DefaultClientCapabilities()
	workspace := subMap(caps, "workspace")
	workspace["applyEdit"] = true
	workspace["workspaceEdit"] = map[string]any{
		"documentChanges":       true,
		"resourceOperations":    []string{"create", "rename", "delete"},
		"failureHandling":       "transactional",
		"normalizesLineEndings": false,
	}
	textDocument := subMap(caps, "textDocument")
	textDocument["rename"] = map[string]any{
		"dynamicRegistration": false,
		// Without this a server has no reason to advertise
		// renameProvider.prepareProvider, and rename would lose the
		// check that fails an invalid rename before anything is staged.
		"prepareSupport": true,
	}
	textDocument["codeAction"] = map[string]any{
		"dynamicRegistration": false,
		// Without literal support a server may only send Commands,
		// which have no edit to preview.
		"codeActionLiteralSupport": map[string]any{
			"codeActionKind": map[string]any{"valueSet": codeActionKinds},
		},
		"isPreferredSupport": true,
		"dataSupport":        true,
		"resolveSupport":     map[string]any{"properties": []string{"edit"}},
	}
	textDocument["formatting"] = map[string]any{"dynamicRegistration": false}
	return caps
}

// codeActionKinds is the standard CodeActionKind value set. The empty
// string is in it because the protocol uses it for "no kind", and a
// client that omits it is asking servers to withhold unclassified
// actions.
var codeActionKinds = []string{
	"", "quickfix", "refactor", "refactor.extract", "refactor.inline",
	"refactor.rewrite", "source", "source.organizeImports", "source.fixAll",
}

// subMap returns the nested object at key, creating it if the caller's
// map does not have one.
func subMap(m map[string]any, key string) map[string]any {
	if sub, ok := m[key].(map[string]any); ok {
		return sub
	}
	sub := map[string]any{}
	m[key] = sub
	return sub
}

// mutationSession is the handshake every mutation command performs.
func mutationSession(common *commonFlags, edits *editCollector) sessionOptions {
	return sessionOptions{
		gate:         common.gateOptions(),
		capabilities: mutationCapabilities(),
		onRequest:    edits.handle,
	}
}

// docSource lets internal/edit stage an edit against the bytes this
// command actually sent to the server, rather than against a fresh
// read of the disk.
//
// It matters for versioned edits: an open document has a version, and
// a documentChanges entry naming a version can only be checked against
// something that has one (PLAN §5.3). Files the command never opened
// fall through to the disk, and the transaction re-reads every file it
// is about to write anyway, so a stale document cannot become a
// silently wrong edit — only a refused one.
type docSource struct{ docs *docstore.Store }

// ReadDocument implements edit.Source.
func (s docSource) ReadDocument(path string) (edit.Document, error) {
	doc, ok := s.docs.Get(path)
	if !ok || !doc.Open {
		return edit.DiskSource{}.ReadDocument(path)
	}
	d := edit.Document{Content: doc.Content, Version: doc.Version}
	if info, err := os.Stat(path); err == nil {
		d.Mode = info.Mode().Perm()
	}
	return d, nil
}

// stageEdit turns a server's raw WorkspaceEdit into a validated,
// unwritten transaction.
//
// The JSON goes through edit.Decode rather than the generated
// unmarshaller: the `edits` union decodes *any* object carrying a
// range as an AnnotatedTextEdit with an empty newText, so a snippet or
// an unknown edit kind would arrive as a well-formed instruction to
// delete that range. Decode refuses it while the evidence still
// exists.
func stageEdit(s *session, raw json.RawMessage) (*edit.Transaction, error) {
	we, err := edit.Decode(raw)
	if err != nil {
		return nil, err
	}
	return edit.Stage(we, edit.Options{Root: s.match.Root, Source: docSource{s.docs}})
}

// textDocumentEdit builds the documentChanges form of a WorkspaceEdit
// from a raw TextEdit array, so that a request answering with plain
// edits (formatting) reaches the applier as the same shape as one
// answering with a WorkspaceEdit (rename).
//
// The server's edit elements are copied verbatim rather than decoded
// and re-encoded, because edit.Decode's job is to inspect exactly what
// the server sent.
func textDocumentEdit(uri protocol.DocumentURI, version int32, edits json.RawMessage) (json.RawMessage, error) {
	if isJSONNull(edits) {
		edits = json.RawMessage("[]")
	}
	doc := map[string]any{"uri": string(uri)}
	if version > 0 {
		doc["version"] = version
	}
	return json.Marshal(map[string]any{
		"textDocument": doc,
		"edits":        json.RawMessage(edits),
	})
}

// workspaceEdit wraps documentChanges entries into a WorkspaceEdit.
func workspaceEdit(changes []json.RawMessage) (json.RawMessage, error) {
	if len(changes) == 0 {
		return json.RawMessage("{}"), nil
	}
	return json.Marshal(map[string]any{"documentChanges": changes})
}

// editOutcome is everything the mutation commands render the same way.
type editOutcome struct {
	tx     *edit.Transaction
	format render.Format
	opts   render.Options
	// emptyIsProblem makes a transaction that changes nothing exit 1.
	//
	// It is per-command because "nothing changed" means opposite
	// things: a rename that produced no edits did not rename anything
	// and the caller's intent failed, while a format that produced no
	// edits found the file already formatted and the caller's intent
	// was already satisfied.
	emptyIsProblem bool
}

// writeMutation commits the transaction when --apply was given, then
// renders it.
//
// Both halves render exactly the same [edit.Transaction.ChangeSet],
// built from the bytes the commit wrote, which is what makes PLAN §8's
// third M2 criterion true by construction: `--format diff` piped
// through `git apply` reproduces `--apply`, because the preview is not
// a second, independently computed answer.
//
// Rendering happens after the commit so that a failed write produces
// an error envelope rather than a patch describing changes that are
// not on disk.
func (e *env) writeMutation(mf *mutationFlags, out editOutcome) int {
	t := out.tx
	if mf.apply {
		if _, err := t.Apply(); err != nil {
			return e.fail(err)
		}
	}
	opts := out.opts
	opts.Root = t.Root()
	opts.Warnings = append(slices.Clone(opts.Warnings), t.Warnings()...)
	if err := render.Changes(e.stdout, out.format, t.ChangeSet(), opts); err != nil {
		return e.fail(err)
	}
	if t.Empty() && out.emptyIsProblem {
		return render.ExitProblems
	}
	return render.ExitOK
}

// editCollector answers workspace/applyEdit.
//
// A code action can arrive as a command rather than an edit; running
// it with workspace/executeCommand makes the server push its edits
// back as applyEdit requests. Those edits must go through the same
// transactional applier as every other edit, so the collector only
// records them — nothing is written from the read loop, where this
// handler runs.
//
// A collector that has not been armed refuses: we advertise
// workspace/applyEdit because a command we ran may need it, not as a
// standing invitation for a server to rewrite the tree.
type editCollector struct {
	// mu guards the fields below. handle runs on the connection's read
	// loop while the command goroutine waits for executeCommand to
	// return; the ordering happens to be safe, but relying on the
	// client library's internal locking for that would be a
	// dependency nobody would think to preserve.
	mu    sync.Mutex
	armed bool
	edits []json.RawMessage
	// labels are the servers' descriptions of the edits, for the
	// error message when there is more than one.
	labels []string
}

// arm accepts applyEdit requests until disarm is called.
func (c *editCollector) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed, c.edits, c.labels = true, nil, nil
}

func (c *editCollector) disarm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.armed = false
}

// handle implements client.RequestHandler for workspace/applyEdit.
func (c *editCollector) handle(_ context.Context, method string, params json.RawMessage) (any, error) {
	if method != methodApplyEdit {
		return nil, fmt.Errorf("%w: %s", client.ErrMethodNotFound, method)
	}
	if c == nil {
		return map[string]any{
			"applied":       false,
			"failureReason": "lightspeed applies edits only as part of the command it was asked to run",
		}, nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.armed {
		return map[string]any{
			"applied":       false,
			"failureReason": "lightspeed applies edits only as part of the command it was asked to run",
		}, nil
	}
	var req struct {
		Label string          `json:"label"`
		Edit  json.RawMessage `json:"edit"`
	}
	if err := json.Unmarshal(params, &req); err != nil {
		return map[string]any{"applied": false, "failureReason": "malformed applyEdit params"}, nil
	}
	c.edits = append(c.edits, req.Edit)
	c.labels = append(c.labels, req.Label)
	// "applied" here means the client has taken responsibility for the
	// edit, which it has: it is staged and will be written or shown.
	// Saying false would make a server believe its own command failed.
	return map[string]any{"applied": true}, nil
}

// collected returns the single WorkspaceEdit the server pushed.
//
// More than one is refused rather than merged. Two edit sets computed
// against the same starting state cannot be composed without knowing
// which of them the second was written against, and guessing would
// turn a server's two safe edits into one wrong one. No server
// lightspeed targets sends more than one per command; if that changes,
// the fix is to teach internal/edit to stage a sequence, not to paper
// over it here.
func (c *editCollector) collected() (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch len(c.edits) {
	case 0:
		return nil, nil
	case 1:
		return c.edits[0], nil
	default:
		return nil, render.Errorf(render.CodeEditConflict,
			"the server pushed %d separate workspace edits (%s); lightspeed applies one edit set per command, and applying part of them is not something it will do",
			len(c.edits), joinLabels(c.labels))
	}
}

func joinLabels(labels []string) string {
	out := make([]string, 0, len(labels))
	for i, l := range labels {
		if l == "" {
			l = fmt.Sprintf("edit %d", i+1)
		}
		out = append(out, l)
	}
	return strings.Join(out, ", ")
}
