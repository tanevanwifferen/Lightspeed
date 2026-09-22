package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// `lightspeed type_hierarchy <loc>` — what does this type extend or implement,
// and what extends or implements it (docs/DECISIONS.md D38).
//
// The protocol answers it with textDocument/prepareTypeHierarchy and
// typeHierarchy/supertypes|subtypes. Few servers have it (gopls, pyright and
// rust-analyzer at the time of writing do not all), so a server without
// typeHierarchyProvider still gets the half that `implementation` can answer:
// the subtypes, one level deep, each row marked evidence:"implementation" and
// the fallback said in a warning. The supertypes have no fallback, and asking
// for only them is exit 3 naming the missing capability.
const (
	methodPrepareTypeHierarchy = "textDocument/prepareTypeHierarchy"
	methodSupertypes           = "typeHierarchy/supertypes"
	methodSubtypes             = "typeHierarchy/subtypes"
)

// The --direction values.
const (
	directionSupertypes = "supertypes"
	directionSubtypes   = "subtypes"
)

const (
	// defaultTypeDepth is the direct supertypes and subtypes.
	defaultTypeDepth = 1
	// maxTypeDepth is as deep as --depth goes: an inheritance tree five levels
	// down is already a survey, not a lookup.
	maxTypeDepth = 5
	// maxTypeNodes bounds the traversal whatever --depth says, as
	// maxCallNodes does for call_hierarchy.
	maxTypeNodes = 300
	// defaultTypeLimit is how many rows are listed unless --limit says.
	defaultTypeLimit = 100
)

// typeHierarchyCapabilities advertise textDocument.typeHierarchy: without it a
// server has no reason to advertise typeHierarchyProvider (see
// callHierarchyCapabilities).
func typeHierarchyCapabilities() map[string]any {
	caps := client.DefaultClientCapabilities()
	subMap(caps, "textDocument")["typeHierarchy"] = map[string]any{"dynamicRegistration": false}
	return caps
}

// A typeRow is one type in the hierarchy.
type typeRow struct {
	// Direction is "supertype" or "subtype", relative to the subject.
	Direction string `json:"direction"`
	// Depth is 1 for a direct supertype or subtype.
	Depth  int    `json:"depth"`
	ID     string `json:"id,omitempty"`
	Name   string `json:"name"`
	Kind   string `json:"kind,omitempty"`
	Detail string `json:"detail,omitempty"`
	File   string `json:"file,omitempty"`
	Line   int    `json:"line,omitempty"`
	Column int    `json:"column,omitempty"`
	// External marks a type outside the workspace (a dependency, the standard
	// library): its File is absolute and it has no id.
	External bool `json:"external,omitempty"`
	// Evidence is the query the row came from.
	Evidence string `json:"evidence"`
}

// typeHierarchyData is the payload of `type_hierarchy`.
type typeHierarchyData struct {
	Root    string  `json:"root"`
	Subject subject `json:"subject"`
	// Source is the evidence the answer is built on: "type_hierarchy", or
	// "implementation" when the server has no type hierarchy.
	Source    string    `json:"source"`
	Direction string    `json:"direction"`
	Depth     int       `json:"depth"`
	Rows      []typeRow `json:"rows"`
	Count     int       `json:"count"`
	Total     int       `json:"total"`
	Truncated bool      `json:"truncated"`
	Limit     int       `json:"limit,omitempty"`
	// Cycles counts branches not followed because they lead back to a type
	// already shown.
	Cycles int `json:"cycles,omitempty"`
}

func typeHierarchyCommand(e *env, c *command, args []string) int {
	var (
		direction string
		depth     int
		fset      *flag.FlagSet
	)
	common, sf, locArg, _, err := parseLocationFlags(e, c, args, 0, func(fs *flag.FlagSet) {
		fset = fs
		fs.StringVar(&direction, "direction", directionBoth, "which types to report: supertypes, subtypes or both")
		fs.IntVar(&depth, "depth", defaultTypeDepth, "how many levels to expand (1 = direct supertypes and subtypes)")
	})
	if err != nil {
		return e.flagError(err)
	}
	switch direction {
	case directionSupertypes, directionSubtypes, directionBoth:
	default:
		return e.usagef("type_hierarchy: --direction must be one of %s, %s, %s (got %q)",
			directionSupertypes, directionSubtypes, directionBoth, direction)
	}
	if depth < 1 || depth > maxTypeDepth {
		return e.usagef("type_hierarchy: --depth must be between 1 and %d (got %d)", maxTypeDepth, depth)
	}
	format, err := managementFormat(common, "type_hierarchy", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	limit := effectiveLimit(fset, common, defaultTypeLimit)

	cs, err := openSubject(e, common, sf, locArg)
	if err != nil {
		return e.fail(err)
	}
	defer cs.Close()
	sess := cs.Q.session

	data := typeHierarchyData{Root: cs.Root, Subject: cs.Subject, Direction: direction, Depth: depth, Rows: []typeRow{}}
	warnings := slices.Clone(cs.Warnings)
	w := &typeWalker{cs: cs, ids: newIDIndex(e, sess, cs.Root), depth: depth}

	if sess.lsp.Supports(methodPrepareTypeHierarchy) {
		data.Source = evidenceTypeHierarchy
		if err := w.native(direction, &data, &warnings); err != nil {
			return e.fail(err)
		}
	} else {
		data.Source = evidenceImplementation
		if err := w.fallback(direction, depth, &data, &warnings); err != nil {
			return e.fail(err)
		}
	}
	data.Rows = append(data.Rows, w.rows...)
	data.Cycles = w.cycles
	if w.truncated {
		warnings = append(warnings, fmt.Sprintf("the hierarchy was cut off at %d types; narrow it with --direction or a smaller --depth", maxTypeNodes))
	}
	if w.cycles > 0 {
		warnings = append(warnings, fmt.Sprintf("%d branch(es) were not expanded because they lead back to a type already shown", w.cycles))
	}
	warnings = append(warnings, w.warnings...)
	warnings = append(warnings, w.ids.summary()...)

	var truncated bool
	data.Rows, truncated, data.Total = capRows(data.Rows, limit)
	data.Count, data.Truncated, data.Limit = len(data.Rows), truncated || w.truncated, limit
	if truncated {
		warnings = append(warnings, truncationWarning("types", data.Count, data.Total, "narrow it with --direction or --depth, or raise --limit"))
	}

	exit := ExitOK
	if len(data.Rows) == 0 {
		exit = ExitProblems
	}
	if format == render.FormatText {
		writeTypeHierarchyText(e.stdout, data, warnings)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// A typeWalker expands a type hierarchy under the bounds of maxTypeNodes and
// --depth, with a visited set for the cycles inheritance never has and a
// server's hierarchy sometimes does.
type typeWalker struct {
	cs        *composed
	ids       *idIndex
	depth     int
	visited   map[string]bool
	rows      []typeRow
	cycles    int
	truncated bool
	warnings  []string
}

// native is the type-hierarchy path: prepare, then expand each requested
// direction.
func (w *typeWalker) native(direction string, data *typeHierarchyData, warnings *[]string) error {
	sess := w.cs.Q.session
	ctx, cancel := w.cs.Q.queryContext()
	res, err := sess.query(ctx, methodPrepareTypeHierarchy, textDocumentPosition(w.cs.Q.doc.URI, w.cs.Q.position))
	cancel()
	if err != nil {
		return err
	}
	*warnings = append(*warnings, res.Warnings...)
	items, err := decodeTypeItems(res.Result)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return render.Errorf(render.CodeNotFound, "%s: %s reports no type at this position",
			w.cs.Subject.Location, sess.match.Server.Name)
	}
	if len(items) > 1 {
		*warnings = append(*warnings, fmt.Sprintf(
			"%s reports %d types at this position (%s); the hierarchy below is for %q only",
			sess.match.Server.Name, len(items), strings.Join(itemNames(items), ", "), items[0].Name))
	}
	for _, m := range []struct{ dir, method, label string }{
		{directionSupertypes, methodSupertypes, "supertype"},
		{directionSubtypes, methodSubtypes, "subtype"},
	} {
		if direction != directionBoth && direction != m.dir {
			continue
		}
		w.visited = nil
		if err := w.expand(items[0], m.method, m.label, 1); err != nil {
			return err
		}
	}
	return nil
}

func (w *typeWalker) mark(key string) bool {
	if w.visited == nil {
		w.visited = map[string]bool{}
	}
	if w.visited[key] {
		return false
	}
	w.visited[key] = true
	return true
}

// expand asks for one level of item and recurses to the depth.
func (w *typeWalker) expand(item callItem, method, label string, depth int) error {
	if w.truncated {
		return nil
	}
	if depth > w.depth {
		return nil
	}
	if !w.mark(item.key()) {
		w.cycles++
		return nil
	}
	sess := w.cs.Q.session
	ctx, cancel := w.cs.Q.queryContext()
	res, err := sess.query(ctx, method, map[string]any{"item": item.Raw})
	cancel()
	if err != nil {
		return err
	}
	w.warnings = append(w.warnings, res.Warnings...)
	items, err := decodeTypeItems(res.Result)
	if err != nil {
		return err
	}
	for _, it := range items {
		if len(w.rows) >= maxTypeNodes {
			w.truncated = true
			return nil
		}
		w.rows = append(w.rows, w.row(it, label, depth))
		if err := w.expand(it, method, label, depth+1); err != nil {
			return err
		}
	}
	return nil
}

// row renders an item. A type outside the workspace keeps its absolute path and
// has no id; a file the store cannot read keeps its line and loses its column.
func (w *typeWalker) row(it callItem, label string, depth int) typeRow {
	r := typeRow{Direction: label, Depth: depth, Name: it.Name, Kind: symbolKindName(it.Kind),
		Detail: it.Detail, Line: int(it.SelectionRange.Start.Line) + 1, Evidence: evidenceTypeHierarchy}
	rel, in := relToRoot(w.cs.Root, it.URI.Path())
	if !in {
		r.File, r.External = it.URI.Path(), true
		return r
	}
	r.File = rel
	if loc, ok := w.cs.Locus(protocol.Location{URI: it.URI, Range: it.SelectionRange}); ok {
		r.Line, r.Column = loc.Line, loc.Column
	}
	r.ID = w.ids.idAt(it.URI, it.SelectionRange)
	return r
}

// fallback answers the subtypes with `implementation`, one level deep, and says
// so. The supertypes have no equivalent.
func (w *typeWalker) fallback(direction string, depth int, data *typeHierarchyData, warnings *[]string) error {
	sess := w.cs.Q.session
	server := sess.match.Server.Name
	if direction == directionSupertypes {
		return unsupportedTypeHierarchy(sess, "supertypes need typeHierarchyProvider, and `implementation` can only answer the subtypes")
	}
	if !sess.lsp.Supports(methodImplementation) {
		return unsupportedTypeHierarchy(sess, "this server has neither typeHierarchyProvider nor implementationProvider")
	}
	ctx, cancel := w.cs.Q.queryContext()
	res, err := sess.query(ctx, methodImplementation, textDocumentPosition(w.cs.Q.doc.URI, w.cs.Q.position))
	cancel()
	if err != nil {
		return err
	}
	*warnings = append(*warnings, res.Warnings...)
	locs, err := decodeLocations(res.Result)
	if err != nil {
		return err
	}
	note := fmt.Sprintf("%s has no typeHierarchyProvider; the subtypes are its textDocument/implementation answers (evidence: implementation), one level deep", server)
	if direction == directionBoth {
		note += ", and the supertypes are not available"
	}
	*warnings = append(*warnings, note)
	if depth > 1 {
		*warnings = append(*warnings, fmt.Sprintf("--depth %d has no effect without a type hierarchy; implementation is one level", depth))
	}
	for _, loc := range locs {
		if len(w.rows) >= maxTypeNodes {
			w.truncated = true
			break
		}
		rel, in := relToRoot(w.cs.Root, loc.URI.Path())
		row := typeRow{Direction: "subtype", Depth: 1, Line: int(loc.Range.Start.Line) + 1, Evidence: evidenceImplementation}
		if !in {
			row.File, row.External = loc.URI.Path(), true
			row.Name = "(outside the workspace)"
			w.rows = append(w.rows, row)
			continue
		}
		row.File = rel
		if m, err := sess.docs.MapperForURI(loc.URI); err == nil {
			if span, err := render.NewSpanFromLocation(m, loc); err == nil {
				row.Line, row.Column = span.Start.Line, span.Start.Column
				row.Name = identAtColumn(span.Text, span.Start.Column)
			}
		}
		row.ID = w.ids.idAt(loc.URI, loc.Range)
		if row.ID != "" {
			if parts, err := parseSymbolID(row.ID); err == nil {
				row.Name, row.Kind = parts.Name, parts.Kind
			}
		}
		if row.Name == "" {
			row.Name = "(unnamed)"
		}
		w.rows = append(w.rows, row)
	}
	slices.SortStableFunc(w.rows, func(a, b typeRow) int {
		if a.File != b.File {
			return strings.Compare(a.File, b.File)
		}
		return a.Line - b.Line
	})
	return nil
}

// unsupportedTypeHierarchy is exit 3 naming the capability the caller needed.
func unsupportedTypeHierarchy(s *session, why string) error {
	err := s.lsp.Check(methodPrepareTypeHierarchy)
	var un *client.UnsupportedMethodError
	if errors.As(err, &un) {
		return render.Errorf(render.CodeUnsupportedMethod, "%s; %s", un.Error(), why).
			WithDetails(map[string]any{"method": un.Method, "capability": un.Capability, "server": un.ServerName})
	}
	return render.Errorf(render.CodeUnsupportedMethod, "%s", why)
}

// decodeTypeItems decodes a prepareTypeHierarchy, supertypes or subtypes answer:
// an array of TypeHierarchyItem, which has the shape of a CallHierarchyItem.
func decodeTypeItems(raw json.RawMessage) ([]callItem, error) {
	if isJSONNull(raw) {
		return nil, nil
	}
	var elems []json.RawMessage
	if err := json.Unmarshal(raw, &elems); err != nil {
		return nil, protocolError("typeHierarchy items", err)
	}
	out := make([]callItem, 0, len(elems))
	for i, elem := range elems {
		it, err := decodeCallItem(elem)
		if err != nil {
			return nil, protocolError(fmt.Sprintf("typeHierarchyItem[%d]", i), err)
		}
		out = append(out, it)
	}
	return out, nil
}

// writeTypeHierarchyText is `file:line:col: <indent><arrow> Name (kind) [evidence]`,
// with `^` for a supertype and `v` for a subtype, so that the tree survives a
// grep.
func writeTypeHierarchyText(w io.Writer, d typeHierarchyData, warnings []string) {
	for _, r := range d.Rows {
		arrow := "^"
		if r.Direction == "subtype" {
			arrow = "v"
		}
		where := r.File
		if r.Line > 0 {
			where += fmt.Sprintf(":%d", r.Line)
			if r.Column > 0 {
				where += fmt.Sprintf(":%d", r.Column)
			}
		}
		label := strings.Repeat("  ", r.Depth-1) + arrow + " " + r.Name
		if r.Kind != "" {
			label += " (" + r.Kind + ")"
		}
		fmt.Fprintf(w, "%s: %s  [%s]\n", where, label, r.Evidence)
	}
	writeWarningLines(w, warnings)
}
