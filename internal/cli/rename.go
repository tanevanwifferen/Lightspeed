package cli

import (
	"encoding/json"
	"strings"

	goplscmd "github.com/tanevanwifferen/Lightspeed/internal/gopls/cmd"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// locationPath is the file a location argument names, validated before
// a server is started for it. It is the part of prepare that has to
// happen early: the dirty-worktree check of PLAN §9.4 is a
// precondition, and finding out about it after a 90-second
// rust-analyzer load would make it useless.
func locationPath(arg string) (string, error) {
	loc := goplscmd.ParseLocation(arg)
	if !loc.IsValid() {
		return "", render.Errorf(render.CodeInvalidPosition,
			"%q is not a location; write file, file:line, file:line:col, file:line:col-line:col or file:#offset", arg)
	}
	path := loc.URI.Path()
	if err := mustBeFile(path); err != nil {
		return "", err
	}
	return path, nil
}

// renameCommand implements `lightspeed rename <loc> <newname>`.
//
// Preview by default, --apply to write, and the write goes through
// internal/edit's transactional applier so that a rename across three
// files either lands completely or leaves the tree exactly as it was
// (PLAN §8, M2).
//
// prepareRename runs first wherever the server advertises it. It is
// the difference between "this position cannot be renamed" and "the
// rename produced no edits", which look identical from the outside and
// mean opposite things; asking first also means an invalid rename
// fails before a single edit has been staged.
func renameCommand(e *env, c *command, args []string) int {
	var mf mutationFlags
	common, positional, err := parseFlags(e, c, args, 2, mf.register)
	if err != nil {
		return e.flagError(err)
	}
	newName := positional[1]
	if strings.TrimSpace(newName) == "" {
		return e.usagef("rename: the new name must not be empty")
	}
	format, err := mutationFormat(common, e.stdout, !mf.apply)
	if err != nil {
		return e.fail(err)
	}

	path, err := locationPath(positional[0])
	if err != nil {
		return e.fail(err)
	}
	var warnings []string
	if mf.apply {
		clean, err := checkWorktrees([]string{path}, mf.allowDirty)
		warnings = append(warnings, clean...)
		if err != nil {
			return e.fail(err)
		}
	}

	var collector editCollector
	q, cleanup, err := prepareWith(e, common, positional[0], mutationSession(common, &collector))
	defer cleanup()
	if err != nil {
		return e.fail(err)
	}

	position := textDocumentPosition(q.doc.URI, q.position)
	if q.session.lsp.Supports(methodPrepareRename) {
		ctx, cancel := q.queryContext()
		res, err := q.session.query(ctx, methodPrepareRename, position)
		cancel()
		if err != nil {
			return e.fail(err)
		}
		warnings = append(warnings, res.Warnings...)
		if err := checkPrepareRename(res.Result, positional[0]); err != nil {
			return e.fail(err)
		}
	}

	params := map[string]any{
		"textDocument": position["textDocument"],
		"position":     position["position"],
		"newName":      newName,
	}
	ctx, cancel := q.queryContext()
	defer cancel()
	res, err := q.session.query(ctx, c.Method, params)
	if err != nil {
		return e.fail(err)
	}
	warnings = append(warnings, res.Warnings...)

	tx, err := stageEdit(q.session, res.Result)
	if err != nil {
		return e.fail(err)
	}
	return e.writeMutation(&mf, editOutcome{
		tx:     tx,
		format: format,
		opts:   common.renderOptions(warnings),
		// A rename that changes nothing renamed nothing. That is a
		// failed intent, not a satisfied one.
		emptyIsProblem: true,
	})
}

// checkPrepareRename reads a textDocument/prepareRename answer.
//
// The protocol gives it four shapes: null, a Range, {range,
// placeholder}, and {defaultBehavior}. Only the first and a
// defaultBehavior of false are refusals — everything else is the
// server describing the identifier it is willing to rename, and the
// description itself is of no use to a caller who already named the
// position.
func checkPrepareRename(raw json.RawMessage, arg string) error {
	cannot := render.Errorf(render.CodeNotFound,
		"%s: the server will not rename the symbol at this position", arg)
	if isJSONNull(raw) {
		return cannot
	}
	var probe struct {
		DefaultBehavior *bool `json:"defaultBehavior"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return protocolError("prepareRename", err)
	}
	if probe.DefaultBehavior != nil && !*probe.DefaultBehavior {
		return cannot
	}
	return nil
}
