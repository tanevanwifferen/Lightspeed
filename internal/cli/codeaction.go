package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	goplscmd "github.com/tanevanwifferen/Lightspeed/internal/gopls/cmd"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// codeActionCommand implements `lightspeed codeaction <loc|range>`.
//
// With no action selected it lists what the server offers at that
// position. With --index or --title it takes one action and shows its
// edits, or writes them with --apply — the same preview-by-default
// rule and the same transactional applier as rename and format.
//
// The awkward part is that a code action need not carry an edit. The
// protocol lets a server send an action whose edit is computed lazily
// (fetched with codeAction/resolve) or one that is only a command to
// run (whose edits come back as workspace/applyEdit requests). All
// three shapes end up in one transaction here, because an agent
// picking action 2 should not have to know which of them it is.
func codeActionCommand(e *env, c *command, args []string) int {
	var (
		mf    mutationFlags
		index int
		title string
		kinds string
	)
	common, sf, locArg, _, err := parseLocationFlags(e, c, args, 0, func(fs *flag.FlagSet) {
		mf.register(fs)
		fs.IntVar(&index, "index", 0, "apply the nth action of the list (1-based)")
		fs.StringVar(&title, "title", "", "apply the action with this title")
		fs.StringVar(&kinds, "kind", "", "comma-separated CodeActionKinds to ask for, e.g. quickfix,source.organizeImports")
	})
	if err != nil {
		return e.flagError(err)
	}
	switch {
	case index != 0 && title != "":
		return e.usagef("codeaction: give --index or --title, not both")
	case index < 0:
		return e.usagef("codeaction: --index is 1-based (got %d)", index)
	}
	chosen := index != 0 || title != ""
	if mf.apply && !chosen {
		return e.usagef("codeaction: --apply needs an action; pick one with --index or --title")
	}

	format, err := codeActionFormat(common, e.stdout, chosen, mf.apply)
	if err != nil {
		return e.fail(err)
	}

	arg, warnings, err := e.location(common, sf, locArg)
	if err != nil {
		return e.fail(err)
	}
	path, err := locationPath(arg)
	if err != nil {
		return e.fail(err)
	}
	if mf.apply {
		clean, err := checkWorktrees([]string{path}, mf.allowDirty)
		warnings = append(warnings, clean...)
		if err != nil {
			return e.fail(err)
		}
	}

	var collector editCollector
	q, cleanup, err := prepareWith(e, common, arg, mutationSession(common, &collector))
	defer cleanup()
	if err != nil {
		return e.fail(err)
	}
	rng, err := requestedRange(q, arg)
	if err != nil {
		return e.fail(err)
	}

	params := map[string]any{
		"textDocument": map[string]any{"uri": string(q.doc.URI)},
		"range":        rng,
		"context": map[string]any{
			// Required by the protocol. lightspeed does not collect
			// diagnostics (that is `check`, M5), so the server is
			// asked for what it can offer unprompted.
			"diagnostics": []any{},
			"triggerKind": 1, // Invoked, i.e. by a user, not by typing
		},
	}
	if only := splitKinds(kinds); len(only) > 0 {
		params["context"].(map[string]any)["only"] = only
	}

	ctx, cancel := q.queryContext()
	defer cancel()
	res, err := q.session.query(ctx, c.Method, params)
	if err != nil {
		return e.fail(err)
	}
	warnings = append(warnings, res.Warnings...)

	actions, err := decodeCodeActions(res.Result)
	if err != nil {
		return e.fail(err)
	}
	if !chosen {
		return e.listCodeActions(q, format, rng, actions, common.renderOptions(warnings))
	}

	action, err := selectCodeAction(actions, index, title)
	if err != nil {
		return e.fail(err)
	}
	raw, notes, err := resolveActionEdit(q, &collector, action)
	warnings = append(warnings, notes...)
	if err != nil {
		return e.fail(err)
	}

	tx, err := stageEdit(q.session, raw)
	if err != nil {
		return e.fail(err)
	}
	return e.writeMutation(&mf, editOutcome{
		tx:     tx,
		format: format,
		opts:   common.renderOptions(warnings),
		// An action that changes no file may still have done its job
		// server-side; the warning above says so. Reporting it as a
		// problem would be guessing.
		emptyIsProblem: false,
	})
}

// codeActionFormat resolves --format for either half of the command:
// the listing is a result set (json or text), a chosen action is an
// edit (json, text or diff, defaulting to diff while previewing).
func codeActionFormat(common *commonFlags, w io.Writer, chosen, apply bool) (render.Format, error) {
	if chosen {
		return mutationFormat(common, w, !apply)
	}
	f, err := common.resolveFormat(w)
	if err != nil {
		return "", err
	}
	if f == render.FormatDiff {
		return "", render.Errorf(render.CodeUnsupportedFormat,
			"format \"diff\" has no meaning for a list of alternative actions; pick one with --index or --title first")
	}
	return f, checkResultsFormat(f)
}

// requestedRange is the range the caller asked about, converted from
// the command line's byte columns to the server's UTF-16 positions.
// A point location asks about a zero-width range at that point, which
// is what an editor sends for a cursor with no selection.
func requestedRange(q *locationQuery, arg string) (protocol.Range, error) {
	loc := goplscmd.ParseLocation(arg)
	rng := protocol.Range{Start: q.position, End: q.position}
	if !loc.End.IsValid() || loc.End == loc.Start {
		return rng, nil
	}
	end, err := positionFor(q.doc, loc.End, arg)
	if err != nil {
		return protocol.Range{}, err
	}
	rng.End = end
	return rng, nil
}

// splitKinds parses --kind into a CodeActionKind list.
func splitKinds(s string) []string {
	var out []string
	for _, k := range strings.Split(s, ",") {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, k)
		}
	}
	return out
}

// codeAction is one element of a textDocument/codeAction answer,
// flattened out of the CodeAction | Command union.
type codeAction struct {
	Title string
	Kind  string
	// Preferred marks the action a server considers the obvious one.
	Preferred bool
	// Disabled is why the action cannot be applied, "" if it can.
	Disabled string
	// Edit is the WorkspaceEdit the action carries, if any.
	Edit json.RawMessage
	// Command is the Command object to execute, if any.
	Command json.RawMessage
	// Raw is the element exactly as the server sent it. It is what
	// codeAction/resolve has to be given back, because the `data`
	// field is the server's private state and re-encoding it from a
	// decoded struct would drop whatever we did not model.
	Raw json.RawMessage
	// Literal reports that the element was a bare Command rather than
	// a CodeAction. A bare Command has nothing to resolve.
	Literal bool
}

// decodeCodeActions decodes the (Command | CodeAction)[] union.
//
// The two are told apart by the type of the `command` field: a Command
// literal has a string there (its identifier), a CodeAction has an
// object (a nested Command) or nothing.
func decodeCodeActions(raw json.RawMessage) ([]codeAction, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, protocolError("codeAction", err)
	}
	out := make([]codeAction, 0, len(elems))
	for i, elem := range elems {
		var obj struct {
			Title     string          `json:"title"`
			Kind      string          `json:"kind"`
			Preferred bool            `json:"isPreferred"`
			Edit      json.RawMessage `json:"edit"`
			Command   json.RawMessage `json:"command"`
			Disabled  *struct {
				Reason string `json:"reason"`
			} `json:"disabled"`
		}
		if err := json.Unmarshal(elem, &obj); err != nil {
			return nil, protocolError(fmt.Sprintf("codeAction[%d]", i), err)
		}
		action := codeAction{
			Title:     obj.Title,
			Kind:      obj.Kind,
			Preferred: obj.Preferred,
			Raw:       elem,
		}
		if obj.Disabled != nil {
			action.Disabled = obj.Disabled.Reason
			if action.Disabled == "" {
				action.Disabled = "no reason given"
			}
		}
		if !isJSONNull(obj.Edit) {
			action.Edit = obj.Edit
		}
		switch {
		case isJSONNull(obj.Command):
		case obj.Command[0] == '"':
			// A Command literal: the element itself is the command.
			action.Command = elem
			action.Literal = true
		default:
			action.Command = obj.Command
		}
		if action.Title == "" {
			action.Title = fmt.Sprintf("action %d", i+1)
		}
		out = append(out, action)
	}
	return out, nil
}

// listCodeActions renders the actions on offer.
//
// Every action is reported at the position the caller asked about,
// because that is where it applies; the label carries the 1-based
// index so that `--index` can be read straight off a text listing,
// which a JSON consumer gets for free from the array order.
func (e *env) listCodeActions(q *locationQuery, format render.Format, rng protocol.Range, actions []codeAction, opts render.Options) int {
	span, err := render.NewSpan(q.doc.Mapper, rng)
	if err != nil {
		return e.fail(err)
	}
	rs := render.ResultSet{Kind: "codeaction", Results: make([]render.Result, 0, len(actions))}
	for i, a := range actions {
		kind := a.Kind
		if kind == "" {
			kind = "codeaction"
		}
		rs.Results = append(rs.Results, render.Result{
			Span:   span,
			Kind:   kind,
			Label:  fmt.Sprintf("[%d] %s", i+1, a.Title),
			Detail: describeAction(a),
		})
	}
	return e.writeResults(format, rs, opts)
}

// describeAction is the secondary column of a listing: whether the
// server prefers this action, and whether it can be applied at all.
func describeAction(a codeAction) string {
	switch {
	case a.Disabled != "":
		return "disabled: " + a.Disabled
	case a.Preferred:
		return "preferred"
	default:
		return ""
	}
}

// selectCodeAction picks the action --index or --title named.
//
// An ambiguous --title is an error rather than a first match: two
// actions with the same title do different things, and picking one for
// the caller is exactly the sort of silent choice a tool that writes
// files must not make.
func selectCodeAction(actions []codeAction, index int, title string) (*codeAction, error) {
	if len(actions) == 0 {
		return nil, render.Errorf(render.CodeNotFound, "the server offers no code action at this position")
	}
	var chosen *codeAction
	if index != 0 {
		if index > len(actions) {
			return nil, render.Errorf(render.CodeUsage,
				"--index %d: the server offered %d action(s): %s", index, len(actions), listTitles(actions))
		}
		chosen = &actions[index-1]
	} else {
		var matches []int
		for i, a := range actions {
			if strings.EqualFold(a.Title, title) {
				matches = append(matches, i)
			}
		}
		switch len(matches) {
		case 0:
			return nil, render.Errorf(render.CodeNotFound,
				"no action titled %q; the server offered: %s", title, listTitles(actions))
		case 1:
			chosen = &actions[matches[0]]
		default:
			return nil, render.Errorf(render.CodeUsage,
				"%d actions are titled %q; select one with --index instead", len(matches), title)
		}
	}
	if chosen.Disabled != "" {
		return nil, render.Errorf(render.CodeProblemsFound,
			"the action %q is disabled: %s", chosen.Title, chosen.Disabled)
	}
	return chosen, nil
}

func listTitles(actions []codeAction) string {
	out := make([]string, 0, len(actions))
	for i, a := range actions {
		out = append(out, fmt.Sprintf("[%d] %s", i+1, a.Title))
	}
	return strings.Join(out, ", ")
}

// resolveActionEdit gets the WorkspaceEdit out of a chosen action,
// through whichever of the three routes the action needs.
//
//  1. The action already carries an edit. Nothing to do.
//  2. It carries none, and the server advertises
//     codeActionProvider.resolveProvider: ask codeAction/resolve to
//     fill it in. This is the protocol's own answer to a lazily
//     computed edit and the cheapest route, because it computes an
//     edit without running anything.
//  3. It is (or resolves to) a command: run it with
//     workspace/executeCommand and collect the workspace/applyEdit
//     requests the server sends back.
//
// Route 3 is the one with teeth. The edits do not exist until the
// command has run, so a *preview* has to run it too — and a server
// command may do things outside the files we are about to show. That
// cannot be hidden, so it is warned about instead.
func resolveActionEdit(q *locationQuery, collector *editCollector, action *codeAction) (json.RawMessage, []string, error) {
	var warnings []string

	if action.Edit != nil && action.Command != nil {
		warnings = append(warnings, fmt.Sprintf(
			"the action %q carries both an edit and a command; only the edit is applied, and the command was not run",
			action.Title))
		return action.Edit, warnings, nil
	}
	if action.Edit != nil {
		return action.Edit, warnings, nil
	}

	if !action.Literal && q.session.lsp.Supports(methodCodeActionResolve) {
		ctx, cancel := q.queryContext()
		resolved, err := q.session.call(ctx, methodCodeActionResolve, action.Raw)
		cancel()
		if err != nil {
			return nil, warnings, err
		}
		filled, err := decodeCodeActions(json.RawMessage("[" + string(resolved) + "]"))
		if err != nil {
			return nil, warnings, err
		}
		if len(filled) == 1 {
			if filled[0].Edit != nil {
				return filled[0].Edit, warnings, nil
			}
			if filled[0].Command != nil {
				action = &filled[0]
			}
		}
	}

	if action.Command == nil {
		return nil, warnings, render.Errorf(render.CodeUnsupportedMethod,
			"the action %q carries no edit, and %s advertises neither codeAction/resolve nor a command to run for it",
			action.Title, q.session.match.Server.Name)
	}

	name, params, err := commandParams(action.Command)
	if err != nil {
		return nil, warnings, err
	}
	if !q.session.lsp.Supports(methodExecuteCommand) {
		return nil, warnings, render.Errorf(render.CodeUnsupportedMethod,
			"the action %q is the server command %q, and %s does not advertise executeCommandProvider",
			action.Title, name, q.session.match.Server.Name)
	}
	warnings = append(warnings, fmt.Sprintf(
		"the action %q had no edit of its own; it was produced by running the server command %q, which may have had effects beyond the files shown here",
		action.Title, name))

	collector.arm()
	defer collector.disarm()
	ctx, cancel := q.queryContext()
	defer cancel()
	if _, err := q.session.call(ctx, methodExecuteCommand, params); err != nil {
		return nil, warnings, err
	}
	raw, err := collector.collected()
	if err != nil {
		return nil, warnings, err
	}
	if raw == nil {
		warnings = append(warnings, fmt.Sprintf(
			"the server command %q sent no edits back", name))
		return json.RawMessage("{}"), warnings, nil
	}
	return raw, warnings, nil
}

// commandParams turns a Command object into executeCommand params.
func commandParams(raw json.RawMessage) (string, map[string]any, error) {
	var cmd struct {
		Command   string            `json:"command"`
		Arguments []json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &cmd); err != nil {
		return "", nil, protocolError("code action command", err)
	}
	if cmd.Command == "" {
		return "", nil, protocolError("code action command",
			fmt.Errorf("the action's command has no identifier"))
	}
	params := map[string]any{"command": cmd.Command}
	if cmd.Arguments != nil {
		params["arguments"] = cmd.Arguments
	}
	return cmd.Command, params, nil
}
