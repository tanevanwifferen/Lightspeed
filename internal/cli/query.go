package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/tanevanwifferen/Lightspeed/internal/docstore"
	goplscmd "github.com/tanevanwifferen/Lightspeed/internal/gopls/cmd"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// flagError maps a flag-parsing failure to an exit code. `-h` is not a
// failure: flag has already printed the usage to stderr, and an agent
// that asked for help should not have to read an error envelope to
// find it.
func (e *env) flagError(err error) int {
	if errors.Is(err, flag.ErrHelp) {
		return render.ExitOK
	}
	return e.fail(err)
}

// checkResultsFormat rejects the formats no read-only command can
// produce, before a server is started. diff describes edits and sarif
// describes diagnostics; both belong to later milestones' commands, and
// asking for one here is a usage error rather than an empty file.
func checkResultsFormat(f render.Format) error {
	switch f {
	case render.FormatJSON, render.FormatText:
		return nil
	default:
		return render.Errorf(render.CodeUnsupportedFormat,
			"format %q has no meaning for a read-only query (want one of json, text)", f)
	}
}

// writeResults renders a result set and picks the exit code.
//
// An empty result set is rendered normally and exits 1: it is a real,
// authoritative answer — the readiness gate would have exited 5
// instead if it were not — and exit 1 is grep's convention for "the
// tool worked and matched nothing". The two must never be conflated,
// which is the whole point of PLAN §5.2.
func (e *env) writeResults(f render.Format, rs render.ResultSet, opts render.Options) int {
	if err := render.Results(e.stdout, f, rs, opts); err != nil {
		return e.fail(err)
	}
	if len(rs.Results) == 0 {
		return render.ExitProblems
	}
	return render.ExitOK
}

// positionFor converts a parsed command-line point into the UTF-16 LSP
// position the server expects. Every conversion goes through the
// vendored gopls Mapper (PLAN §5.1); a byte column in a line
// containing CJK or emoji is not the character the server would count.
func positionFor(doc *docstore.Document, p goplscmd.Point, arg string) (protocol.Position, error) {
	var (
		pos protocol.Position
		err error
	)
	switch {
	case p.HasPosition():
		pos, err = doc.Mapper.LineCol8Position(p.Line, p.Column)
	case p.HasOffset():
		pos, err = doc.Mapper.OffsetPosition(p.Offset)
	default:
		return protocol.Position{}, render.Errorf(render.CodeInvalidPosition,
			"%s: no position; write file:line:col, file:line:col-line:col or file:#offset", arg)
	}
	if err != nil {
		return protocol.Position{}, render.Errorf(render.CodeInvalidPosition, "%s: %v", arg, err)
	}
	return pos, nil
}

// locationSet turns the locations of a definition, references or
// implementation answer into a renderable result set.
//
// Results routinely point at files the command never opened, so each
// one is resolved through the document store's Mapper cache. A file
// the store cannot read — a jdt:// URI, a generated source, a file
// deleted since the server indexed it — is reported as a warning
// rather than dropped in silence or allowed to abort the answer.
func locationSet(s *session, kind string, locs []protocol.Location) (render.ResultSet, []string) {
	rs := render.ResultSet{Kind: kind, Results: make([]render.Result, 0, len(locs))}
	var warnings []string
	for _, loc := range locs {
		m, err := s.docs.MapperForURI(loc.URI)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("dropped a result in %s: %v", loc.URI, err))
			continue
		}
		span, err := render.NewSpanFromLocation(m, loc)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("dropped a result in %s: %v", loc.URI, err))
			continue
		}
		rs.Results = append(rs.Results, render.Result{Span: span})
	}
	return rs, warnings
}

// mustBeFile rejects a location that names no readable file, before a
// server is started for it. A missing path is a usage error (exit 2),
// not a language server's problem.
func mustBeFile(path string) error {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return render.Errorf(render.CodeNoSuchFile, "%s: no such file", path)
	case err != nil:
		return render.Errorf(render.CodeIOError, "%s: %v", path, err)
	case info.IsDir():
		return render.Errorf(render.CodeUsage, "%s is a directory, not a file", path)
	}
	return nil
}

// locationQuery is the plumbing every location-shaped command shares:
// parse the span, resolve the server, open the document, convert the
// position, run the request under the readiness gate.
type locationQuery struct {
	common   *commonFlags
	session  *session
	position protocol.Position
	doc      *docstore.Document
}

// prepare runs everything up to and including the position conversion.
// The returned cleanup must be called by the caller; it is separate
// from the error return so that a failure after the server started
// still reaps it.
func prepare(e *env, common *commonFlags, arg string) (*locationQuery, func(), error) {
	return prepareWith(e, common, arg, sessionOptions{gate: common.gateOptions()})
}

// prepareWith is prepare with the handshake spelled out, for the
// mutation commands: they advertise capabilities the read-only surface
// must not (see mutationCapabilities).
func prepareWith(e *env, common *commonFlags, arg string, sopts sessionOptions) (*locationQuery, func(), error) {
	noop := func() {}

	loc := goplscmd.ParseLocation(arg)
	if !loc.IsValid() {
		return nil, noop, render.Errorf(render.CodeInvalidPosition,
			"%q is not a location; write file, file:line, file:line:col, file:line:col-line:col or file:#offset", arg)
	}
	path := loc.URI.Path()
	if err := mustBeFile(path); err != nil {
		return nil, noop, err
	}

	match, err := resolveTarget(path, "", common.server)
	if err != nil {
		return nil, noop, err
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), common.timeout)
	defer cancel()
	s, err := startSessionWith(connectCtx, e, match, sopts)
	if err != nil {
		return nil, noop, err
	}
	cleanup := s.close

	doc, err := s.open(path)
	if err != nil {
		return nil, cleanup, err
	}
	pos, err := positionFor(doc, loc.Start, arg)
	if err != nil {
		return nil, cleanup, err
	}
	return &locationQuery{common: common, session: s, position: pos, doc: doc}, cleanup, nil
}

// queryContext bounds a gated request. It is deliberately looser than
// --timeout: see gateSlack.
func (q *locationQuery) queryContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), q.common.timeout+gateSlack)
}

// locationCommand implements definition, references and implementation,
// which differ only in their method and in references' -d flag.
func locationCommand(e *env, c *command, args []string) int {
	name := c.Name
	var declaration bool
	common, sf, locArg, _, err := parseLocationFlags(e, c, args, 0, func(fs *flag.FlagSet) {
		if name == "references" {
			// gopls spells this -d; the long form is here because an
			// agent writing a command line reaches for the readable
			// one and both should work.
			fs.BoolVar(&declaration, "d", false, "include the declaration of the symbol among the references")
			fs.BoolVar(&declaration, "declaration", false, "alias for -d")
		}
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

	locArg, warnings, err := e.location(common, sf, locArg)
	if err != nil {
		return e.fail(err)
	}

	q, cleanup, err := prepare(e, common, locArg)
	defer cleanup()
	if err != nil {
		return e.fail(err)
	}

	params := textDocumentPosition(q.doc.URI, q.position)
	if name == "references" {
		params["context"] = map[string]any{"includeDeclaration": declaration}
	}

	ctx, cancel := q.queryContext()
	defer cancel()
	res, err := q.session.query(ctx, c.Method, params)
	if err != nil {
		return e.fail(err)
	}

	locs, err := decodeLocations(res.Result)
	if err != nil {
		return e.fail(err)
	}
	rs, locWarnings := locationSet(q.session, name, locs)
	rs.Sort()
	warnings = append(warnings, res.Warnings...)
	return e.writeResults(format, rs, common.renderOptions(append(warnings, locWarnings...)))
}

// hoverCommand implements `lightspeed hover <loc>`: the signature and
// documentation of the symbol under a position.
//
// The hover text is the result's label, so `--format text` prints one
// grep-compatible line (embedded newlines escaped) and `--format json`
// carries the markdown verbatim. Nothing is truncated: a hover is one
// result, and the token discipline of PLAN §4 is about result counts.
func hoverCommand(e *env, c *command, args []string) int {
	common, sf, locArg, _, err := parseLocationFlags(e, c, args, 0, nil)
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

	locArg, warnings, err := e.location(common, sf, locArg)
	if err != nil {
		return e.fail(err)
	}

	q, cleanup, err := prepare(e, common, locArg)
	defer cleanup()
	if err != nil {
		return e.fail(err)
	}

	ctx, cancel := q.queryContext()
	defer cancel()
	res, err := q.session.query(ctx, c.Method, textDocumentPosition(q.doc.URI, q.position))
	if err != nil {
		return e.fail(err)
	}

	text, rng, err := decodeHover(res.Result)
	if err != nil {
		return e.fail(err)
	}

	rs := render.ResultSet{Kind: "hover"}
	if text != "" {
		// A server that omits the range is describing the symbol at
		// the position we asked about, so that is where the answer is
		// reported.
		at := protocol.Range{Start: q.position, End: q.position}
		if rng != nil {
			at = *rng
		}
		span, err := render.NewSpan(q.doc.Mapper, at)
		if err != nil {
			return e.fail(err)
		}
		rs.Results = append(rs.Results, render.Result{Span: span, Kind: "hover", Label: text})
	}
	return e.writeResults(format, rs, common.renderOptions(append(warnings, res.Warnings...)))
}
