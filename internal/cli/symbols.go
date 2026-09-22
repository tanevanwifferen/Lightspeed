package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"slices"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// symbolSet turns decoded symbols into a renderable result set.
//
// The label is the qualified name — "Server.Handle", not "Handle" —
// because that is what a caller greps for and what distinguishes two
// methods with the same name. The source line is still carried in the
// JSON payload; the label only replaces it in text output.
func symbolSet(s *session, kind string, syms []symbol, ids []string) (render.ResultSet, []string) {
	rs := render.ResultSet{Kind: kind, Results: make([]render.Result, 0, len(syms))}
	var (
		warnings  []string
		unlocated int
	)
	for i, sym := range syms {
		m, err := s.docs.MapperForURI(sym.URI)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("dropped symbol %q in %s: %v", sym.Qualified, sym.URI, err))
			continue
		}
		span, err := render.NewSpan(m, sym.Range)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("dropped symbol %q in %s: %v", sym.Qualified, sym.URI, err))
			continue
		}
		if !sym.HasRange {
			unlocated++
		}
		var id string
		if i < len(ids) {
			id = ids[i]
		}
		rs.Results = append(rs.Results, render.Result{
			ID:     id,
			Span:   span,
			Kind:   sym.Kind,
			Detail: sym.Detail,
			Label:  sym.Qualified,
		})
	}
	if unlocated > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d symbol(s) came back with a file but no range; they are reported at the start of the file", unlocated))
	}
	return rs, warnings
}

// symbolsCommand implements `lightspeed symbols <file>`: the symbols
// declared in one file, in document order.
//
// Document order is the server's order and is kept rather than sorted:
// a hierarchical DocumentSymbol answer is flattened depth-first, so a
// method follows the type it belongs to, which is how the file reads.
func symbolsCommand(e *env, c *command, args []string) int {
	var anchor string
	common, positional, err := parseFlags(e, c, args, 1, func(fs *flag.FlagSet) {
		fs.StringVar(&anchor, "path", ".", "directory whose workspace the symbol ids are relative to")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := common.resolveFormat(e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if err := checkResultsFormat(format); err != nil {
		return e.fail(err)
	}

	path, err := filepath.Abs(positional[0])
	if err != nil {
		return e.fail(render.Errorf(render.CodeUsage, "resolving %s: %v", positional[0], err))
	}
	if err := mustBeFile(path); err != nil {
		return e.fail(err)
	}
	match, err := e.resolveTarget(path, "", common.server)
	if err != nil {
		return e.fail(err)
	}

	connectCtx, cancelConnect := context.WithTimeout(e.base(), common.timeout)
	defer cancelConnect()
	s, err := startSession(connectCtx, e, match, common.gateOptions())
	if err != nil {
		return e.fail(err)
	}
	defer s.close()

	doc, err := s.open(path)
	if err != nil {
		return e.fail(err)
	}

	ctx, cancel := context.WithTimeout(e.base(), common.timeout+gateSlack)
	defer cancel()
	res, err := s.query(ctx, c.Method, map[string]any{
		"textDocument": map[string]any{"uri": string(doc.URI)},
	})
	if err != nil {
		return e.fail(err)
	}

	syms, pinWarnings, err := decodeFileSymbols(res.Result, doc)
	if err != nil {
		return e.fail(err)
	}
	// Ids are relative to the workspace, so a file outside it has none.
	var ids []string
	if root, err := commandWorkspace(anchor); err != nil {
		return e.fail(err)
	} else if rel, in := relToRoot(root, path); in {
		ids = symbolIDs(rel, syms)
	}
	rs, warnings := symbolSet(s, "symbols", syms, ids)
	return e.writeResults(format, rs, common.renderOptions(s.match.Root, append(append(res.Warnings, pinWarnings...), warnings...)))
}

// workspaceSymbolCommand implements
// `lightspeed workspace_symbol <query>`: a name search across the
// whole workspace.
//
// Unlike every other read-only command this one names no file, so
// there is nothing to route on. --path says which tree to search
// (default the current directory) and --language names its language
// outright; without either, a file in the tree is found to speak for
// it. The server's own ordering is preserved because it is a relevance
// ranking, and re-sorting it by path would throw away the one thing
// the server knows and we do not.
func workspaceSymbolCommand(e *env, c *command, args []string) int {
	var (
		path     string
		language string
	)
	common, positional, err := parseFlags(e, c, args, 1, func(fs *flag.FlagSet) {
		fs.StringVar(&path, "path", ".", "directory whose workspace to search")
		fs.StringVar(&language, "language", "", "language id of the workspace, when no file in it identifies one")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := common.resolveFormat(e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if err := checkResultsFormat(format); err != nil {
		return e.fail(err)
	}

	match, err := e.resolveWorkspace(path, language, common.server)
	if err != nil {
		return e.fail(err)
	}

	connectCtx, cancelConnect := context.WithTimeout(e.base(), common.timeout)
	defer cancelConnect()
	s, err := startSession(connectCtx, e, match, common.gateOptions())
	if err != nil {
		return e.fail(err)
	}
	defer s.close()

	ctx, cancel := context.WithTimeout(e.base(), common.timeout+gateSlack)
	defer cancel()
	res, err := s.query(ctx, c.Method, map[string]any{"query": positional[0]})
	if err != nil {
		return e.fail(err)
	}

	syms, err := decodeWorkspaceSymbols(res.Result)
	if err != nil {
		return e.fail(err)
	}
	// Ids need each result's file outlined (a `~N` suffix depends on every
	// duplicate in it), which idIndex does, within limits it reports.
	root, err := commandWorkspace(path)
	if err != nil {
		return e.fail(err)
	}
	index := newIDIndex(e, s, root)
	ids := make([]string, len(syms))
	for i, sym := range syms {
		if sym.HasRange {
			ids[i] = index.idAt(sym.URI, sym.Range)
		}
	}
	rs, warnings := symbolSet(s, "workspace_symbol", syms, ids)
	return e.writeResults(format, rs, common.renderOptions(s.match.Root, slices.Concat(res.Warnings, warnings, index.summary())))
}
