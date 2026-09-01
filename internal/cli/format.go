package cli

import (
	"context"
	"encoding/json"
	"flag"
	"path/filepath"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// formatCommand implements `lightspeed format <path...>`: the server's
// own formatter, previewed by default and written with --apply.
//
// Every file goes into one transaction, so formatting twenty files is
// still all-or-nothing: a server that answers well for nineteen of
// them and returns an out-of-range edit for the twentieth leaves the
// tree untouched rather than nineteen-twentieths formatted.
//
// That guarantee is why all the paths must share one workspace: a
// transaction is confined to a single root (PLAN §5.3), and quietly
// running two of them would be the partial application this command
// exists to avoid. Two roots is a usage error naming both, not a
// silent split.
func formatCommand(e *env, c *command, args []string) int {
	var (
		mf          mutationFlags
		tabSize     int
		insertSpace bool
	)
	common, positional, err := parseFlagsRange(e, c, args, 1, -1, func(fs *flag.FlagSet) {
		mf.register(fs)
		// The protocol makes these two mandatory, so a value is sent
		// whatever happens; most servers with an opinion of their own
		// (gofmt, rustfmt) ignore them.
		fs.IntVar(&tabSize, "tab-size", 4, "width of a tab stop, for servers that ask")
		fs.BoolVar(&insertSpace, "insert-spaces", false, "indent with spaces instead of tabs, for servers that ask")
	})
	if err != nil {
		return e.flagError(err)
	}
	if tabSize <= 0 {
		return e.usagef("format: --tab-size must be positive (got %d)", tabSize)
	}
	format, err := mutationFormat(common, e.stdout, !mf.apply)
	if err != nil {
		return e.fail(err)
	}

	paths, err := formatTargets(positional)
	if err != nil {
		return e.fail(err)
	}
	match, err := commonWorkspace(paths, common.server)
	if err != nil {
		return e.fail(err)
	}
	var warnings []string
	if mf.apply {
		clean, err := checkWorktrees(paths, mf.allowDirty)
		warnings = append(warnings, clean...)
		if err != nil {
			return e.fail(err)
		}
	}

	var collector editCollector
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), common.timeout)
	defer cancelConnect()
	s, err := startSessionWith(connectCtx, e, match, mutationSession(common, &collector))
	if err != nil {
		return e.fail(err)
	}
	defer s.close()

	changes := make([]json.RawMessage, 0, len(paths))
	for _, path := range paths {
		doc, err := s.open(path)
		if err != nil {
			return e.fail(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), common.timeout+gateSlack)
		res, err := s.query(ctx, c.Method, map[string]any{
			"textDocument": map[string]any{"uri": string(doc.URI)},
			"options": map[string]any{
				"tabSize":      tabSize,
				"insertSpaces": insertSpace,
			},
		})
		cancel()
		if err != nil {
			return e.fail(err)
		}
		warnings = append(warnings, res.Warnings...)
		if isJSONNull(res.Result) {
			continue
		}
		change, err := textDocumentEdit(doc.URI, doc.Version, res.Result)
		if err != nil {
			return e.fail(render.Errorf(render.CodeInternal, "building edit for %s: %v", path, err))
		}
		changes = append(changes, change)
	}

	raw, err := workspaceEdit(changes)
	if err != nil {
		return e.fail(render.Errorf(render.CodeInternal, "building workspace edit: %v", err))
	}
	tx, err := stageEdit(s, raw)
	if err != nil {
		return e.fail(err)
	}
	return e.writeMutation(&mf, editOutcome{
		tx:     tx,
		format: format,
		opts:   common.renderOptions(warnings),
		// A file that is already formatted needs no edits, and that is
		// the command succeeding rather than finding a problem.
		emptyIsProblem: false,
	})
}

// formatTargets makes the path arguments absolute, checks each names a
// readable file, and drops repeats. A file named twice would otherwise
// contribute its edits twice, which the applier would (correctly)
// reject as overlapping — an error about the server for a mistake on
// the command line.
func formatTargets(args []string) ([]string, error) {
	seen := make(map[string]bool, len(args))
	paths := make([]string, 0, len(args))
	for _, arg := range args {
		abs, err := filepath.Abs(arg)
		if err != nil {
			return nil, render.Errorf(render.CodeUsage, "resolving %s: %v", arg, err)
		}
		if err := mustBeFile(abs); err != nil {
			return nil, err
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true
		paths = append(paths, abs)
	}
	return paths, nil
}

// commonWorkspace resolves every path and insists they agree on one
// server and one root.
func commonWorkspace(paths []string, serverName string) (router.Match, error) {
	first, err := resolveTarget(paths[0], "", serverName)
	if err != nil {
		return router.Match{}, err
	}
	for _, path := range paths[1:] {
		match, err := resolveTarget(path, "", serverName)
		if err != nil {
			return router.Match{}, err
		}
		if match.Server.Name != first.Server.Name || match.Root != first.Root {
			return router.Match{}, render.Errorf(render.CodeUsage,
				"format: %s and %s belong to different workspaces (%s in %s, %s in %s); run one command per workspace, so that each is applied all or nothing",
				shortPath(paths[0]), shortPath(path),
				first.Server.Name, first.Root, match.Server.Name, match.Root)
		}
	}
	return first, nil
}

// shortPath is a path as a caller would recognise it: relative to the
// current directory when that is shorter, absolute otherwise.
func shortPath(path string) string {
	cwd, err := filepath.Abs(".")
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(cwd, path)
	if err != nil || strings.HasPrefix(rel, "..") || len(rel) >= len(path) {
		return path
	}
	return rel
}
