package cli

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// symbolSet turns decoded symbols into a renderable result set.
//
// The label is the qualified name — "Server.Handle", not "Handle" —
// because that is what a caller greps for and what distinguishes two
// methods with the same name. The source line is still carried in the
// JSON payload; the label only replaces it in text output.
func symbolSet(s *session, kind string, syms []symbol) (render.ResultSet, []string) {
	rs := render.ResultSet{Kind: kind, Results: make([]render.Result, 0, len(syms))}
	var (
		warnings  []string
		unlocated int
	)
	for _, sym := range syms {
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
		rs.Results = append(rs.Results, render.Result{
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
	common, positional, err := parseFlags(e, c, args, 1, nil)
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
	match, err := resolveTarget(path, "", common.server)
	if err != nil {
		return e.fail(err)
	}

	connectCtx, cancelConnect := context.WithTimeout(context.Background(), common.timeout)
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

	ctx, cancel := context.WithTimeout(context.Background(), common.timeout+gateSlack)
	defer cancel()
	res, err := s.query(ctx, c.Method, map[string]any{
		"textDocument": map[string]any{"uri": string(doc.URI)},
	})
	if err != nil {
		return e.fail(err)
	}

	syms, err := decodeDocumentSymbols(res.Result, doc.URI)
	if err != nil {
		return e.fail(err)
	}
	rs, warnings := symbolSet(s, "symbols", syms)
	return e.writeResults(format, rs, common.renderOptions(append(res.Warnings, warnings...)))
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

	match, err := resolveWorkspace(path, language, common.server)
	if err != nil {
		return e.fail(err)
	}

	connectCtx, cancelConnect := context.WithTimeout(context.Background(), common.timeout)
	defer cancelConnect()
	s, err := startSession(connectCtx, e, match, common.gateOptions())
	if err != nil {
		return e.fail(err)
	}
	defer s.close()

	ctx, cancel := context.WithTimeout(context.Background(), common.timeout+gateSlack)
	defer cancel()
	res, err := s.query(ctx, c.Method, map[string]any{"query": positional[0]})
	if err != nil {
		return e.fail(err)
	}

	syms, err := decodeWorkspaceSymbols(res.Result)
	if err != nil {
		return e.fail(err)
	}
	rs, warnings := symbolSet(s, "workspace_symbol", syms)
	return e.writeResults(format, rs, common.renderOptions(append(res.Warnings, warnings...)))
}
