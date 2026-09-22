package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	goplscmd "github.com/tanevanwifferen/Lightspeed/internal/gopls/cmd"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/symbols"
)

// `source` and `context`: symbol-addressed retrieval (docs/DECISIONS.md D22).
//
// An agent that has listed a file's symbols wants one of them, and reading the
// whole file to get it is the cost this exists to avoid. So the answer is the
// symbol's own text, cut out of the file by the range the language server gave
// for it, with an explicit cap and an explicit `truncated`.

// The line index and the declaration heuristics are internal/symbols'.
type lineIndex = symbols.LineIndex

var newLineIndex = symbols.NewLineIndex

func endLine(sym symbol) int { return symbols.EndLine(sym) }

// capSource applies --max-lines and --max-bytes to a slice of source, and
// reports whether it cut. Lines are cut on a line boundary, bytes on a rune
// boundary: what is returned is always valid UTF-8 where the input was, so a
// cap that falls inside a CJK character or an emoji drops the character
// instead of splitting it.
func capSource(src []byte, maxLines, maxBytes int) (out []byte, truncated bool) {
	out = src
	if maxLines > 0 {
		n := 0
		for i, b := range out {
			if b != '\n' {
				continue
			}
			if n++; n == maxLines {
				out = out[:i]
				truncated = true
				break
			}
		}
	}
	if maxBytes > 0 && len(out) > maxBytes {
		cut := maxBytes
		for cut > 0 && !utf8.RuneStart(out[cut]) {
			cut--
		}
		out = out[:cut]
		truncated = true
	}
	return bytes.TrimSuffix(out, []byte("\r")), truncated
}

// contentHash is the hash a caller compares to see whether a symbol has
// changed since it read it.
func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// A sourceItem is one symbol's source, as `source` and `context` return it.
type sourceItem struct {
	ID string `json:"id,omitempty"`
	// File is the path relative to the workspace, or absolute for a file
	// outside it.
	File string `json:"file"`
	Kind string `json:"kind"`
	// Line and EndLine are 1-based and inclusive, and describe the whole
	// symbol, doc comment above its declaration included: not the --context
	// lines and not where a cap cut. What was returned is Source, and
	// SourceLine, ReturnedLines and ReturnedBytes say how much of it that is.
	Line    int `json:"line"`
	EndLine int `json:"end_line"`
	// SourceLine is the line Source starts at, given only when it is not
	// Line: --context lines come before the symbol.
	SourceLine int    `json:"source_line,omitempty"`
	Source     string `json:"source"`
	// ReturnedLines and ReturnedBytes measure Source, context and cap
	// included. Compare them with TotalLines and the symbol's own size to see
	// a cut; Truncated says whether there was one.
	ReturnedLines int `json:"returned_lines"`
	ReturnedBytes int `json:"returned_bytes"`
	// Hash is of the symbol's own lines — comment included, context and caps
	// not — so it is the same whatever the caller asked for around it.
	Hash string `json:"hash"`
	// Truncated is true when --max-lines or --max-bytes cut Source short.
	Truncated bool `json:"truncated"`
	// TotalLines is how many lines the symbol has, before any cap.
	TotalLines int `json:"total_lines"`

	// symbolReturned is how many of the symbol's own lines Source holds, for
	// the warning of a cut.
	symbolReturned int
}

// sourceCaps are the limits of one call.
type sourceCaps struct {
	context  int
	maxLines int
	maxBytes int
}

// symbolSource cuts a symbol's text out of its file.
func symbolSource(ff *fileSymbols, i int, caps sourceCaps) sourceItem {
	sym := ff.syms[i]
	item := sliceSource(newLineIndex(ff.doc.Content), int(sym.Full.Start.Line), endLine(sym), caps)
	item.File = ff.displayPath()
	item.Kind = idKind(sym.Kind)
	if ff.ids != nil {
		item.ID = ff.ids[i]
	}
	return item
}

// sliceSource cuts the declaration on 0-based lines first..last, its leading
// comment and decorators included, out of a file, and applies the caps. It is
// what `source` and `search_symbols --detail full` share, so that a symbol's
// text is the same whichever asked for it.
func sliceSource(x *lineIndex, first, last int, caps sourceCaps) sourceItem {
	first = x.LeadingStart(first)
	last = min(last, x.Count()-1)

	whole := x.Slice(first, last)
	item := sourceItem{
		Hash:       contentHash(whole),
		TotalLines: last - first + 1,
	}

	lo, hi := max(0, first-caps.context), min(x.Count()-1, last+caps.context)
	src, cut := capSource(x.Slice(lo, hi), caps.maxLines, caps.maxBytes)
	item.Source, item.Truncated = string(src), cut
	item.Line, item.EndLine = first+1, last+1
	if lo != first {
		item.SourceLine = lo + 1
	}
	item.ReturnedBytes = len(src)
	if len(src) > 0 {
		item.ReturnedLines = strings.Count(string(src), "\n") + 1
	}
	item.symbolReturned = min(max(item.ReturnedLines-(first-lo), 0), item.TotalLines)
	return item
}

// cutWarning says what a cap took, in terms of the symbol: the lines of it that
// came back, not the lines of Source, which may include context.
func cutWarning(it sourceItem) string {
	if it.symbolReturned >= it.TotalLines {
		return fmt.Sprintf("%s: the --context lines were cut at the cap; the symbol's %d lines are complete", labelOf(it), it.TotalLines)
	}
	return fmt.Sprintf("%s was cut at the cap: %d of its %d lines returned (%d bytes)", labelOf(it), it.symbolReturned, it.TotalLines, it.ReturnedBytes)
}

// displayPath is the file as an id would spell it, or its absolute path when
// it is outside the workspace.
func (ff *fileSymbols) displayPath() string {
	if ff.rel != "" {
		return ff.rel
	}
	return ff.doc.Path
}

// An itemError is one target of a batch that could not be resolved.
type itemError struct {
	Target  string      `json:"target"`
	Code    render.Code `json:"code"`
	Message string      `json:"message"`
	Data    any         `json:"data,omitempty"`
}

// warning is the item's failure as one line for the warnings: `target: message`,
// without saying the target twice when the message already begins with it
// (`nosuch.go: nosuch.go: no such file` was what an outline of a missing file
// among several printed).
func (ie itemError) warning() string {
	if strings.HasPrefix(ie.Message, ie.Target+": ") {
		return ie.Message
	}
	return ie.Target + ": " + ie.Message
}

func newItemError(target string, err error) itemError {
	out := itemError{Target: target, Code: render.CodeInternal, Message: err.Error()}
	if coded, ok := err.(*render.CodedError); ok {
		out.Code, out.Message, out.Data = coded.Code, coded.Message, coded.Details
	}
	return out
}

// sourceFlags are the flags `source` and `context` share.
type sourceFlags struct {
	path string
	// id is --id: a symbol id, the same as giving it as an argument.
	id       string
	maxLines int
	maxBytes int
}

func (f *sourceFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.path, "path", ".", "directory whose workspace the symbol ids are relative to")
	fs.IntVar(&f.maxLines, "max-lines", 0, "cap the source at this many lines (0 = no cap); a cut is reported as truncated:true")
	fs.IntVar(&f.maxBytes, "max-bytes", 0, "cap the source at this many bytes, cut on a character boundary (0 = no cap)")
}

// registerID adds --id, for the commands that name symbols.
func (f *sourceFlags) registerID(fs *flag.FlagSet) {
	fs.StringVar(&f.id, "id", "", "a symbol id (path::Container.Name#kind), as an alternative to the argument, like every command that takes a location")
}

func (f *sourceFlags) validate(name string) error {
	if f.maxLines < 0 || f.maxBytes < 0 {
		return render.Errorf(render.CodeUsage, "%s: --max-lines and --max-bytes must not be negative", name)
	}
	return nil
}

// resolvedTarget is a `source` argument taken as far as the symbol it names.
type resolvedTarget struct {
	arg   string
	file  *fileSymbols
	index int
}

// resolveTarget reads one `<id|loc>` argument down to a symbol. A failure of
// the argument — an id that is not well formed or has gone stale, a location
// with no symbol at it — is returned as itemError so that a batch can carry on
// without it; a failure of the server is returned as err and ends the command,
// because nothing after it could be believed either.
func (ss *sessionSet) resolveTarget(arg string) (t resolvedTarget, item *itemError, err error) {
	fail := func(err error) (resolvedTarget, *itemError, error) {
		ie := newItemError(arg, err)
		return resolvedTarget{}, &ie, nil
	}

	if isSymbolID(arg) {
		parts, err := parseSymbolID(arg)
		if err != nil {
			return fail(err)
		}
		file, err := idFile(ss.root, parts, arg, sameNameFiles(ss.e.base(), ss.root, parts))
		if err != nil {
			return fail(err)
		}
		ff, err := ss.load(file)
		if err != nil {
			return resolvedTarget{}, nil, err
		}
		i, err := ff.find(parts, arg)
		if err != nil {
			return fail(err)
		}
		return resolvedTarget{arg: arg, file: ff, index: i}, nil, nil
	}

	loc := goplscmd.ParseLocation(arg)
	if !loc.IsValid() {
		return fail(render.Errorf(render.CodeInvalidPosition,
			"%q is neither a symbol id nor a location; write path::Name#kind, file:line:col or file:#offset", arg))
	}
	file := loc.URI.Path()
	if err := mustBeFile(file); err != nil {
		return fail(err)
	}
	ff, err := ss.load(file)
	if err != nil {
		return resolvedTarget{}, nil, err
	}
	pos, err := positionFor(ff.doc, loc.Start, arg)
	if err != nil {
		return fail(err)
	}
	i := enclosingSymbol(ff.syms, pos)
	if i < 0 {
		return fail(render.Errorf(render.CodeNotFound, "%s: no symbol contains this position in %s", arg, file))
	}
	return resolvedTarget{arg: arg, file: ff, index: i}, nil, nil
}

// enclosingSymbol is the innermost symbol whose declaration contains pos, or
// -1. Where two have the same range the first — the outer — wins.
func enclosingSymbol(syms []symbol, pos protocol.Position) int {
	best := -1
	for i, sym := range syms {
		if posBefore(pos, sym.Full.Start) || posBefore(sym.Full.End, pos) {
			continue
		}
		if best < 0 || rangeContains(syms[best].Full, sym.Full) {
			best = i
		}
	}
	return best
}

// sourceData is the payload of `source`.
type sourceData struct {
	Symbols []sourceItem `json:"symbols"`
	Count   int          `json:"count"`
	// Errors are the targets that could not be resolved, when others could.
	Errors []itemError `json:"errors,omitempty"`
}

// sourceCommand implements `lightspeed source <id|loc>...`.
func sourceCommand(e *env, c *command, args []string) int {
	var sf sourceFlags
	common, targets, err := parseFlagsRange(e, c, args, 0, -1, func(fs *flag.FlagSet) {
		sf.register(fs)
		sf.registerID(fs)
	})
	if err != nil {
		return e.flagError(err)
	}
	if sf.id != "" {
		targets = append(targets, sf.id)
	}
	if len(targets) == 0 {
		return e.usagef("source: expected a symbol id or a location (%s), or --id", c.Args)
	}
	if err := sf.validate(c.Name); err != nil {
		return e.fail(err)
	}
	format, err := managementFormat(common, "source", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	root, err := commandWorkspace(sf.path)
	if err != nil {
		return e.fail(err)
	}

	ss := newSessionSet(e, common, root)
	defer ss.close()
	caps := sourceCaps{context: common.context, maxLines: sf.maxLines, maxBytes: sf.maxBytes}

	var (
		data     sourceData
		warnings []string
		failures []error
	)
	data.Symbols = []sourceItem{}
	for _, arg := range targets {
		t, item, err := ss.resolveTarget(arg)
		if err != nil {
			return e.fail(err)
		}
		if item != nil {
			data.Errors = append(data.Errors, *item)
			failures = append(failures, &render.CodedError{Code: item.Code, Message: item.Message, Details: item.Data})
			continue
		}
		it := symbolSource(t.file, t.index, caps)
		data.Symbols = append(data.Symbols, it)
		if it.Truncated {
			warnings = append(warnings, cutWarning(it))
		}
	}
	data.Count = len(data.Symbols)
	warnings = append(warnings, ss.warnings()...)

	if len(data.Symbols) == 0 {
		// Nothing was resolved. One failed target is that target's own error,
		// with its candidates; several are reported together.
		if len(failures) == 1 {
			return e.fail(failures[0])
		}
		return e.fail(render.Errorf(render.CodeStaleID, "none of the %d targets could be resolved", len(failures)).
			WithDetails(map[string]any{"errors": data.Errors}))
	}
	exit := ExitOK
	for _, ie := range data.Errors {
		warnings = append(warnings, ie.warning())
		exit = ExitProblems
	}

	if format == render.FormatText {
		writeSourceText(e.stdout, data)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// labelOf names an item in a warning.
func labelOf(it sourceItem) string {
	if it.ID != "" {
		return it.ID
	}
	return fmt.Sprintf("%s:%d", it.File, it.Line)
}

// writeSourceText prints each symbol under a header line, the way a diff
// prints a file.
func writeSourceText(w io.Writer, d sourceData) {
	for _, it := range d.Symbols {
		fmt.Fprintf(w, "== %s (%s:%d-%d)", labelOf(it), it.File, it.Line, it.EndLine)
		if it.Truncated {
			fmt.Fprintf(w, " [cut: %d of %d lines returned]", it.symbolReturned, it.TotalLines)
		}
		fmt.Fprintf(w, " ==\n%s\n", it.Source)
	}
	for _, ie := range d.Errors {
		fmt.Fprintf(w, "# %s\n", ie.warning())
	}
}

// --- context ---

// defaultHeaderLines caps the import block of `context`.
const defaultHeaderLines = 80

// A headerBlock is the top of a file: its package clause and imports.
type headerBlock struct {
	Line      int    `json:"line"`
	EndLine   int    `json:"end_line"`
	Source    string `json:"source"`
	Truncated bool   `json:"truncated"`
}

// contextData is the payload of `context`: the symbol, what it needs from the
// top of its file, and what the server says its signature is.
type contextData struct {
	sourceItem
	// Signature is the declaration as written, sliced from the file.
	Signature string `json:"signature,omitempty"`
	// Header is the file's package and import block: everything above its
	// first declaration.
	Header *headerBlock `json:"header,omitempty"`
	// Hover is what the server's hover says at the symbol's name.
	Hover string `json:"hover,omitempty"`
}

// fileHeader is the block of a file above its first declaration and that
// declaration's doc comment, with trailing blank lines dropped, capped at
// maxLines. It is "the package clause and imports" for every language whose
// first declaration follows them, which is the ones with imports.
func fileHeader(ff *fileSymbols, maxLines int) *headerBlock {
	if len(ff.syms) == 0 {
		return nil
	}
	x := newLineIndex(ff.doc.Content)
	firstDecl := x.Count()
	for _, sym := range ff.syms {
		firstDecl = min(firstDecl, x.LeadingStart(int(sym.Full.Start.Line)))
	}
	last := firstDecl - 1
	for last >= 0 && x.Text(last) == "" {
		last--
	}
	if last < 0 {
		return nil
	}
	h := &headerBlock{Line: 1, EndLine: last + 1}
	if maxLines > 0 && last+1 > maxLines {
		last = maxLines - 1
		h.EndLine, h.Truncated = maxLines, true
	}
	h.Source = string(x.Slice(0, last))
	return h
}

// contextCommand implements `lightspeed context <id|loc>`: one call for what
// an agent needs to understand a symbol without opening its file.
func contextCommand(e *env, c *command, args []string) int {
	var (
		sf        sourceFlags
		query     string
		maxHeader int
	)
	common, positional, err := parseFlagsRange(e, c, args, 0, 1, func(fs *flag.FlagSet) {
		sf.register(fs)
		sf.registerID(fs)
		fs.StringVar(&query, "symbol", "",
			"resolve a dotted symbol path (pkg.Type.Method) to the symbol, instead of naming an id or a location")
		fs.IntVar(&maxHeader, "max-header-lines", defaultHeaderLines, "cap the file's import block at this many lines (0 = no cap)")
	})
	if err != nil {
		return e.flagError(err)
	}
	if err := sf.validate(c.Name); err != nil {
		return e.fail(err)
	}
	// One way of naming the symbol, as on every command that takes a location.
	named := len(positional)
	for _, s := range []string{sf.id, query} {
		if s != "" {
			named++
		}
	}
	if named != 1 {
		return e.usagef("context: name the symbol exactly one way: %s, --id or --symbol (got %d)", c.Args, named)
	}
	if maxHeader < 0 {
		return e.usagef("context: --max-header-lines must not be negative")
	}
	format, err := managementFormat(common, "context", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	root, err := commandWorkspace(sf.path)
	if err != nil {
		return e.fail(err)
	}

	var warnings []string
	target := sf.id
	switch {
	case len(positional) == 1:
		target = positional[0]
	case query != "":
		loc, notes, err := resolveSymbol(e, common, &symbolFlags{query: query, path: sf.path})
		if err != nil {
			return e.fail(err)
		}
		target, warnings = loc, notes
	}

	ss := newSessionSet(e, common, root)
	defer ss.close()
	t, item, err := ss.resolveTarget(target)
	if err != nil {
		return e.fail(err)
	}
	if item != nil {
		return e.fail(&render.CodedError{Code: item.Code, Message: item.Message, Details: item.Data})
	}

	sym := t.file.syms[t.index]
	src := symbolSource(t.file, t.index, sourceCaps{context: common.context, maxLines: sf.maxLines, maxBytes: sf.maxBytes})
	x := newLineIndex(t.file.doc.Content)
	data := contextData{
		sourceItem: src,
		Signature:  sym.Signature,
		Header:     fileHeader(t.file, maxHeader),
	}
	if data.Signature == "" {
		data.Signature = x.DeclSignature(int(sym.Full.Start.Line), endLine(sym))
	}
	if src.Truncated {
		warnings = append(warnings, cutWarning(src))
	}
	if data.Header != nil && data.Header.Truncated {
		warnings = append(warnings, fmt.Sprintf("the file's header was cut at %d lines", maxHeader))
	}

	// The hover is what the server says the symbol is, which is more than
	// the source when the type is inferred. It is an extra, so a server that
	// does not offer it, or fails, costs a warning and not the answer.
	s := t.file.session
	switch {
	case !s.lsp.Supports(methodHover):
		warnings = append(warnings, s.match.Server.Name+" does not advertise hover, so there is no hover text")
	default:
		ctx, cancel := s.requestContext()
		res, err := s.query(ctx, methodHover, textDocumentPosition(t.file.doc.URI, sym.Range.Start))
		cancel()
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("no hover text: %v", err))
			break
		}
		warnings = append(warnings, res.Warnings...)
		if text, _, err := decodeHover(res.Result); err != nil {
			warnings = append(warnings, fmt.Sprintf("no hover text: %v", err))
		} else if hoverIsJustSignature(text, data.Signature) {
			// The server's hover is a fenced code block holding exactly the
			// declaration `signature` already carries and `source`'s own
			// first line already shows (undocumented symbols get no more
			// than this from gopls); printing it twice buys nothing
			// (docs/DECISIONS.md D47).
			warnings = append(warnings, "hover omitted: identical to the signature already in source")
		} else {
			data.Hover = text
		}
	}
	warnings = append(warnings, ss.warnings()...)

	if format == render.FormatText {
		writeContextText(e.stdout, data)
		return ExitOK
	}
	return e.writeData(common, data, warnings, ExitOK)
}

// hoverIsJustSignature reports whether hover is nothing but a fenced code
// block holding signature: what a server without more to say (no doc
// comment) answers, and what `context` already prints twice over —
// `signature` and source's own first line — without it (D47).
func hoverIsJustSignature(hover, signature string) bool {
	h := strings.TrimSpace(hover)
	if !strings.HasPrefix(h, "```") || !strings.HasSuffix(h, "```") || len(h) < 6 {
		return false
	}
	inner := strings.TrimSuffix(strings.TrimPrefix(h, "```"), "```")
	if nl := strings.IndexByte(inner, '\n'); nl >= 0 {
		// A leading language tag ("go") on its own line, not part of the code.
		if tag := strings.TrimSpace(inner[:nl]); tag != "" && !strings.ContainsAny(tag, " \t(){};") {
			inner = inner[nl+1:]
		}
	}
	return signature != "" && strings.TrimSpace(inner) == strings.TrimSpace(signature)
}

// writeContextText prints the parts of a context in reading order.
func writeContextText(w io.Writer, d contextData) {
	if d.Header != nil {
		fmt.Fprintf(w, "== header (%s:%d-%d) ==\n%s\n", d.File, d.Header.Line, d.Header.EndLine, d.Header.Source)
	}
	if d.Hover != "" {
		fmt.Fprintf(w, "== hover ==\n%s\n", d.Hover)
	}
	fmt.Fprintf(w, "== %s (%s:%d-%d) ==\n%s\n", labelOf(d.sourceItem), d.File, d.Line, d.EndLine, d.Source)
}
