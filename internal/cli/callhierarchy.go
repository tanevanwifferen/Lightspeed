package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed call_hierarchy <loc>` — who calls this, and what does it
// call.
//
// The protocol splits the question in two: prepareCallHierarchy turns
// a position into an item, and incomingCalls/outgoingCalls expand an
// item. Expanding the answers again is what makes it a hierarchy, and
// what makes it dangerous: a call graph has cycles, and a breadth of
// twenty at depth four is 160 000 requests. So the traversal is bounded
// three ways — a depth limit, a node budget, and a visited set — and
// every bound that bites is reported rather than silently applied.
const (
	methodPrepareCallHierarchy = "textDocument/prepareCallHierarchy"
	methodIncomingCalls        = "callHierarchy/incomingCalls"
	methodOutgoingCalls        = "callHierarchy/outgoingCalls"
)

// The --direction values.
const (
	directionIncoming = "incoming"
	directionOutgoing = "outgoing"
	directionBoth     = "both"
)

// Traversal bounds.
const (
	// defaultCallDepth is one level: the direct callers and callees.
	// It is the answer to the question as usually asked, and deeper is
	// opt-in because deeper is exponential.
	defaultCallDepth = 1
	// maxCallDepth is as deep as --depth may go. Five levels of a real
	// call graph is already thousands of requests; past that the
	// caller wants a different tool, not a bigger flag.
	maxCallDepth = 5
	// maxCallNodes bounds the whole traversal, so that a wide graph is
	// truncated rather than unbounded.
	maxCallNodes = 500
)

// callHierarchyCommand implements `lightspeed call_hierarchy <loc>`.
func callHierarchyCommand(e *env, c *command, args []string) int {
	var (
		direction string
		depth     int
	)
	common, sf, locArg, _, err := parseLocationFlags(e, c, args, 0, func(fs *flag.FlagSet) {
		fs.StringVar(&direction, "direction", directionBoth,
			"which calls to report: incoming (callers), outgoing (callees) or both")
		fs.IntVar(&depth, "depth", defaultCallDepth,
			"how many levels to expand (1 = direct calls only)")
	})
	if err != nil {
		return e.flagError(err)
	}
	switch direction {
	case directionIncoming, directionOutgoing, directionBoth:
	default:
		return e.usagef("call_hierarchy: --direction must be one of %s, %s, %s (got %q)",
			directionIncoming, directionOutgoing, directionBoth, direction)
	}
	if depth < 1 || depth > maxCallDepth {
		return e.usagef("call_hierarchy: --depth must be between 1 and %d (got %d)", maxCallDepth, depth)
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

	q, cleanup, err := prepareWith(e, common, locArg, sessionOptions{
		gate:         common.gateOptions(),
		capabilities: callHierarchyCapabilities(),
	})
	defer cleanup()
	if err != nil {
		return e.fail(err)
	}

	ctx, cancel := q.queryContext()
	defer cancel()
	res, err := q.session.query(ctx, methodPrepareCallHierarchy,
		textDocumentPosition(q.doc.URI, q.position))
	if err != nil {
		return e.fail(err)
	}
	warnings = append(warnings, res.Warnings...)

	items, err := decodeCallHierarchyItems(res.Result)
	if err != nil {
		return e.fail(err)
	}
	if len(items) == 0 {
		return e.fail(render.Errorf(render.CodeNotFound,
			"%s: %s reports no callable symbol at this position", locArg, q.session.match.Server.Name))
	}
	if len(items) > 1 {
		// Several items means the server could not tell which symbol
		// the position belongs to either. The first is used — a
		// hierarchy of one of them is more useful than nothing — but
		// the others are named, because an answer about a symbol the
		// caller did not mean has to be recognisable as one.
		warnings = append(warnings, fmt.Sprintf(
			"%s reports %d callable symbols at this position (%s); the hierarchy below is for %q only",
			q.session.match.Server.Name, len(items), strings.Join(itemNames(items), ", "), items[0].Name))
	}

	walker := &callWalker{q: q, depth: depth}
	rs := render.ResultSet{Kind: "call_hierarchy"}
	if direction == directionIncoming || direction == directionBoth {
		if err := walker.walk(&rs, items[0], methodIncomingCalls, 1); err != nil {
			return e.fail(err)
		}
	}
	if direction == directionOutgoing || direction == directionBoth {
		walker.reset()
		if err := walker.walk(&rs, items[0], methodOutgoingCalls, 1); err != nil {
			return e.fail(err)
		}
	}
	rs.Truncated = walker.truncated
	if walker.truncated {
		rs.Total = walker.nodes
		warnings = append(warnings, fmt.Sprintf(
			"the call graph was cut off at %d entries; narrow it with --direction or a smaller --depth", maxCallNodes))
	}
	if walker.cycles > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d branch(es) were not expanded because they lead back to a symbol already shown", walker.cycles))
	}
	warnings = append(warnings, walker.warnings...)

	// The order is the traversal order, and it is not sorted: a
	// hierarchy read depth-first is a hierarchy, and sorting it by
	// path would leave a flat list of locations whose indentation
	// means nothing.
	return e.writeResults(format, rs, common.renderOptions(warnings))
}

// callHierarchyCapabilities advertise textDocument.callHierarchy.
// Without it a server has no reason to advertise callHierarchyProvider,
// and the command would refuse to run against a server that supports
// it perfectly well.
func callHierarchyCapabilities() map[string]any {
	caps := client.DefaultClientCapabilities()
	subMap(caps, "textDocument")["callHierarchy"] = map[string]any{"dynamicRegistration": false}
	return caps
}

// callItem is a CallHierarchyItem, kept alongside the bytes the server
// sent. The raw form is what goes back in the next request: `data` is
// the server's private state for the item, and re-encoding it from a
// decoded struct would drop whatever this build does not model.
type callItem struct {
	Name           string
	Detail         string
	Kind           int
	URI            protocol.DocumentURI
	Range          protocol.Range
	SelectionRange protocol.Range
	Raw            json.RawMessage
}

// key identifies an item for cycle detection: the symbol's own
// declaration site.
func (i callItem) key() string {
	return fmt.Sprintf("%s#%d:%d", i.URI, i.SelectionRange.Start.Line, i.SelectionRange.Start.Character)
}

// callWalker expands a call hierarchy depth-first under the bounds
// described at the top of this file.
type callWalker struct {
	q     *locationQuery
	depth int

	// visited holds the items already expanded, so a recursive or
	// mutually recursive call is reported once and not followed.
	visited map[string]bool
	// nodes counts reported entries, cycles the branches not
	// followed, and truncated records that maxCallNodes bit.
	nodes     int
	cycles    int
	truncated bool
	warnings  []string
}

// reset clears the visited set between directions, so that the
// outgoing half of a `--direction both` answer is not silently pruned
// by what the incoming half already showed.
func (w *callWalker) reset() { w.visited = nil }

func (w *callWalker) mark(key string) bool {
	if w.visited == nil {
		w.visited = map[string]bool{}
	}
	if w.visited[key] {
		return false
	}
	w.visited[key] = true
	return true
}

// walk expands item one level and recurses. method is the direction.
func (w *callWalker) walk(rs *render.ResultSet, item callItem, method string, depth int) error {
	if depth > w.depth || w.truncated {
		return nil
	}
	if !w.mark(item.key()) {
		w.cycles++
		return nil
	}
	if !w.q.session.lsp.Supports(method) {
		// The capability covers all three methods, so a server that
		// answered prepareCallHierarchy has already claimed these.
		// Checking anyway keeps PLAN §5.4 true if that ever changes.
		return w.q.session.lsp.Check(method)
	}

	ctx, cancel := w.q.queryContext()
	res, err := w.q.session.query(ctx, method, map[string]any{"item": item.Raw})
	cancel()
	if err != nil {
		return err
	}
	w.warnings = append(w.warnings, res.Warnings...)

	calls, err := decodeCalls(res.Result, method)
	if err != nil {
		return err
	}
	for _, call := range calls {
		if w.nodes >= maxCallNodes {
			w.truncated = true
			return nil
		}
		result, err := w.result(call, method, depth)
		if err != nil {
			w.warnings = append(w.warnings, err.Error())
			continue
		}
		rs.Results = append(rs.Results, result)
		w.nodes++
		if err := w.walk(rs, call.item, method, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// result renders one call as a result row.
//
// The row points at the *other* symbol's declaration — the caller's
// name for an incoming call, the callee's for an outgoing one —
// because that is where a caller's next command wants to go, and
// because a declaration is one place while a call site is often many.
// The call sites are not lost: the first is named in the detail column
// with a count of the rest.
func (w *callWalker) result(call callRelation, method string, depth int) (render.Result, error) {
	item := call.item
	m, err := w.q.session.docs.MapperForURI(item.URI)
	if err != nil {
		return render.Result{}, fmt.Errorf("dropped %q in %s: %v", item.Name, item.URI, err)
	}
	span, err := render.NewSpan(m, item.SelectionRange)
	if err != nil {
		return render.Result{}, fmt.Errorf("dropped %q in %s: %v", item.Name, item.URI, err)
	}
	return render.Result{
		Span:   span,
		Kind:   symbolKindName(item.Kind),
		Label:  callLabel(method, depth, item.Name),
		Detail: w.describeCallSites(call, method, item),
	}, nil
}

// callLabel is the tree shape, in a form that survives a
// `file:line:col: ` prefix: two spaces of indentation per level and an
// arrow that says which way the call goes.
func callLabel(method string, depth int, name string) string {
	arrow := "<-"
	if method == methodOutgoingCalls {
		arrow = "->"
	}
	return strings.Repeat("  ", depth-1) + arrow + " " + name
}

// describeCallSites names where the call happens, plus whatever
// signature detail the server volunteered.
func (w *callWalker) describeCallSites(call callRelation, method string, item callItem) string {
	var parts []string
	if item.Detail != "" {
		parts = append(parts, item.Detail)
	}
	if len(call.fromRanges) > 0 {
		// For an incoming call the ranges are in the caller's file;
		// for an outgoing one they are in the file of the item being
		// expanded, which is the caller again. Either way it is the
		// place where the call is written.
		site := call.fromRanges[0]
		uri := item.URI
		if method == methodOutgoingCalls {
			uri = w.q.doc.URI
		}
		if where, ok := w.pointOf(uri, site); ok {
			if extra := len(call.fromRanges) - 1; extra > 0 {
				where += fmt.Sprintf(" (+%d more)", extra)
			}
			parts = append(parts, "called at "+where)
		}
	}
	return strings.Join(parts, "; ")
}

// pointOf renders a range as `file:line:col` in byte columns, or
// reports that the file could not be read.
func (w *callWalker) pointOf(uri protocol.DocumentURI, rng protocol.Range) (string, bool) {
	m, err := w.q.session.docs.MapperForURI(uri)
	if err != nil {
		return "", false
	}
	span, err := render.NewSpan(m, rng)
	if err != nil {
		return "", false
	}
	return fmt.Sprintf("%s:%d:%d", shortPath(span.Path), span.Start.Line, span.Start.Column), true
}

// callRelation is one incoming or outgoing call: the item at the other
// end, and the ranges where the call is written.
type callRelation struct {
	item       callItem
	fromRanges []protocol.Range
}

// decodeCallHierarchyItems decodes a prepareCallHierarchy answer.
func decodeCallHierarchyItems(raw json.RawMessage) ([]callItem, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, protocolError("prepareCallHierarchy", err)
	}
	out := make([]callItem, 0, len(elems))
	for i, elem := range elems {
		item, err := decodeCallItem(elem)
		if err != nil {
			return nil, protocolError(fmt.Sprintf("callHierarchyItem[%d]", i), err)
		}
		out = append(out, item)
	}
	return out, nil
}

// decodeCalls decodes an incomingCalls or outgoingCalls answer. The two
// differ only in whether the item is called `from` or `to`.
func decodeCalls(raw json.RawMessage, method string) ([]callRelation, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, protocolError(method, err)
	}
	out := make([]callRelation, 0, len(elems))
	for i, elem := range elems {
		var wrapper struct {
			From       json.RawMessage  `json:"from"`
			To         json.RawMessage  `json:"to"`
			FromRanges []protocol.Range `json:"fromRanges"`
		}
		if err := json.Unmarshal(elem, &wrapper); err != nil {
			return nil, protocolError(fmt.Sprintf("%s[%d]", method, i), err)
		}
		raw := wrapper.From
		if method == methodOutgoingCalls {
			raw = wrapper.To
		}
		if isJSONNull(raw) {
			return nil, protocolError(fmt.Sprintf("%s[%d]", method, i),
				fmt.Errorf("the call carries no item"))
		}
		item, err := decodeCallItem(raw)
		if err != nil {
			return nil, protocolError(fmt.Sprintf("%s[%d]", method, i), err)
		}
		out = append(out, callRelation{item: item, fromRanges: wrapper.FromRanges})
	}
	return out, nil
}

// decodeCallItem decodes one CallHierarchyItem, keeping the bytes.
func decodeCallItem(raw json.RawMessage) (callItem, error) {
	var obj struct {
		Name           string          `json:"name"`
		Detail         string          `json:"detail"`
		Kind           int             `json:"kind"`
		URI            string          `json:"uri"`
		Range          protocol.Range  `json:"range"`
		SelectionRange *protocol.Range `json:"selectionRange"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return callItem{}, err
	}
	if obj.URI == "" {
		return callItem{}, fmt.Errorf("the item has no uri")
	}
	uri, err := protocol.ParseDocumentURI(obj.URI)
	if err != nil {
		return callItem{}, err
	}
	selection := obj.Range
	if obj.SelectionRange != nil {
		selection = *obj.SelectionRange
	}
	if obj.Name == "" {
		obj.Name = "(unnamed)"
	}
	return callItem{
		Name:           obj.Name,
		Detail:         obj.Detail,
		Kind:           obj.Kind,
		URI:            uri,
		Range:          obj.Range,
		SelectionRange: selection,
		Raw:            raw,
	}, nil
}

// itemNames lists item names for a warning message.
func itemNames(items []callItem) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Name)
	}
	return out
}
