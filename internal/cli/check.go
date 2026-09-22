package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/gopls/protocol"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// `lightspeed check [path...]` — diagnostics, and the one command in
// this package whose answer is not a reply to a request.
//
// Diagnostics reach a client two ways. The old, universal way is the
// push model: the server sends textDocument/publishDiagnostics
// whenever it feels like it, for whichever files it feels like, and
// never says "that is all of them". The newer way is the pull model
// (textDocument/diagnostic), which is an ordinary request with an
// ordinary answer. Where a server advertises the pull model this
// command uses it, because a request that returns is worth more than
// any amount of inference. Otherwise it has to decide when it has
// them all, and that decision is the substance of this file.
//
// The rule is: a push-mode answer is reportable only when
//
//  1. the readiness gate says the workspace is loaded — the same gate
//     and the same evidence as every other command (PLAN §5.2), and
//  2. every file this command opened has been published *about* at
//     least once, an empty array included, and
//  3. no diagnostic has arrived for the settle window.
//
// (2) is the interesting one. A file the server has never mentioned is
// not a clean file; it is a file we know nothing about. Reporting a
// clean tree on that basis is precisely the failure PLAN §5.2 exists
// to prevent, only worse — an agent that believes `check` is silent
// will commit. So a silent file is exit 5 with the file named, and
// --allow-silent is the explicit, warned-about way to accept it.

// The LSP methods behind `check`.
const (
	methodPublishDiagnostics = "textDocument/publishDiagnostics"
	methodDocumentDiagnostic = "textDocument/diagnostic"
)

// How diagnostics are collected, i.e. the --diagnostics flag.
const (
	// collectAuto uses the pull model where the server advertises it.
	collectAuto = "auto"
	// collectPull insists on textDocument/diagnostic, and fails with
	// exit 3 if the server does not advertise it.
	collectPull = "pull"
	// collectPush insists on publishDiagnostics, even from a server
	// that offers the pull model. Useful for reproducing what an
	// editor sees, and for debugging this file.
	collectPush = "push"
)

// defaultCheckMaxFiles bounds how many files one `check` opens.
//
// It exists because `check .` on a monorepo would otherwise open every
// file in it, and a language server holds every open document in
// memory. Truncation is reported rather than silent, as everywhere
// else in the output contract.
const defaultCheckMaxFiles = 200

// checkPollInterval is how often the push collector is inspected. The
// gate's own interval, for the same reason: it is short enough not to
// add measurable latency and long enough not to spin.
const checkPollInterval = client.DefaultPollInterval

// checkCommand implements `lightspeed check [path...]`.
func checkCommand(e *env, c *command, args []string) int {
	var (
		mode        string
		allowSilent bool
		maxFiles    int
		language    string
	)
	common, positional, err := parseFlagsRange(e, c, args, 0, -1, func(fs *flag.FlagSet) {
		fs.StringVar(&mode, "diagnostics", collectAuto,
			"how to collect diagnostics: auto, pull (textDocument/diagnostic) or push (publishDiagnostics)")
		fs.BoolVar(&allowSilent, "allow-silent", false,
			"report even when the server never published anything about some files; they are named in the warnings")
		fs.IntVar(&maxFiles, "max-files", defaultCheckMaxFiles,
			"open at most this many files, reporting truncation")
		fs.StringVar(&language, "language", "",
			"language id of the tree, when no file in it identifies one")
	})
	if err != nil {
		return e.flagError(err)
	}
	switch mode {
	case collectAuto, collectPull, collectPush:
	default:
		return e.usagef("check: --diagnostics must be one of %s, %s, %s (got %q)",
			collectAuto, collectPull, collectPush, mode)
	}
	if maxFiles <= 0 {
		return e.usagef("check: --max-files must be positive (got %d)", maxFiles)
	}
	format, err := checkFormat(common, e.stdout)
	if err != nil {
		return e.fail(err)
	}

	targets := positional
	if len(targets) == 0 {
		targets = []string{"."}
	}
	files, match, warnings, err := checkTargets(e, targets, language, common.server, maxFiles)
	if err != nil {
		return e.fail(err)
	}

	connectCtx, cancelConnect := context.WithTimeout(e.base(), common.timeout)
	defer cancelConnect()
	s, err := startSessionWith(connectCtx, e, match, sessionOptions{gate: common.gateOptions()})
	if err != nil {
		return e.fail(err)
	}
	defer s.close()

	// Opening the documents is what starts the diagnostics flowing in
	// push mode, and is required before either model answers about a
	// file at all (PLAN §5.4).
	uris := make([]protocol.DocumentURI, 0, len(files))
	for _, f := range files {
		doc, err := s.openAs(f.path, f.languageID)
		if err != nil {
			return e.fail(err)
		}
		uris = append(uris, doc.URI)
	}

	pull := mode == collectPull || (mode == collectAuto && s.lsp.Supports(methodDocumentDiagnostic))
	var (
		byURI map[protocol.DocumentURI][]lspDiagnostic
		notes []string
	)
	if pull {
		byURI, notes, err = pullDiagnostics(s, common, uris)
	} else {
		byURI, notes, err = pushDiagnostics(s, common, uris, allowSilent)
	}
	warnings = append(warnings, notes...)
	if err != nil {
		return e.fail(err)
	}

	ds, notes := diagnosticSet(s, byURI)
	warnings = append(warnings, notes...)
	ds.Sort()

	// The exit code is decided on the whole set, before --limit can
	// cut it down: a limit is a display concession, and letting it
	// turn exit 1 into exit 0 would make `check --limit 1` a way to
	// pass CI.
	problems := ds.HasErrors()

	// Diagnostics are workspace-wide, so paths are reported relative
	// to the workspace root — shorter output, and a SARIF run that
	// says what its URIs are relative to.
	opts := e.rootNote(format, common.renderOptions(match.Root, warnings))
	if err := render.Diagnostics(e.stdout, format, ds, opts); err != nil {
		return e.fail(err)
	}
	if problems {
		return render.ExitProblems
	}
	return render.ExitOK
}

// checkFormat resolves --format for diagnostics: json, text or sarif.
// diff describes edits and has nothing to say here.
func checkFormat(common *commonFlags, w io.Writer) (render.Format, error) {
	f, err := common.resolveFormat(w)
	if err != nil {
		return "", err
	}
	switch f {
	case render.FormatJSON, render.FormatText, render.FormatSARIF:
		return f, nil
	default:
		return "", render.Errorf(render.CodeUnsupportedFormat,
			"format %q has no meaning for diagnostics (want one of json, text, sarif)", f)
	}
}

// checkCapabilities are the client capabilities `check` advertises.
//
// Both are load-bearing rather than decorative: a server may only
// advertise diagnosticProvider to a client that claims
// textDocument.diagnostic, so without it the pull model would never be
// available; and tagSupport is what earns the `unnecessary` and
// `deprecated` tags that the SARIF output carries in its property bag.
// Nothing here is claimed that this command does not use.
func checkCapabilities() map[string]any {
	caps := client.DefaultClientCapabilities()
	textDocument := subMap(caps, "textDocument")
	textDocument["publishDiagnostics"] = map[string]any{
		"relatedInformation":     false,
		"versionSupport":         false,
		"codeDescriptionSupport": false,
		"dataSupport":            false,
		"tagSupport":             map[string]any{"valueSet": []int{1, 2}},
	}
	textDocument["diagnostic"] = map[string]any{
		"dynamicRegistration":    false,
		"relatedDocumentSupport": true,
	}
	return caps
}

// checkTargets turns the path arguments into the list of files to open,
// together with the one server and root they share.
//
// A directory is walked; a file is taken as named. Files claimed by a
// different server than the first one are dropped with a warning:
// merging several servers' diagnostics into one report is explicitly
// deferred (PLAN §8), and quietly reporting a Go tree's diagnostics as
// if they covered its TypeScript half would be worse than saying so.
// A checkFile is one file to open, with the language id of the server
// definition that claimed it — which is not necessarily the language
// id of the file that resolved the server, since one workspace holds
// several kinds of file.
type checkFile struct {
	path       string
	languageID string
}

func checkTargets(e *env, targets []string, language, serverName string, maxFiles int) (
	[]checkFile, router.Match, []string, error) {
	var (
		none     router.Match
		warnings []string
	)
	seen := map[string]bool{}
	var candidates []string
	for _, target := range targets {
		abs, err := filepath.Abs(target)
		if err != nil {
			return nil, none, nil, render.Errorf(render.CodeUsage, "resolving %s: %v", target, err)
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, none, nil, render.Errorf(render.CodeNoSuchFile, "%s: no such file or directory", target)
		}
		if !info.IsDir() {
			if !seen[abs] {
				seen[abs] = true
				candidates = append(candidates, abs)
			}
			continue
		}
		// Each directory is listed with the definitions of its own
		// workspace, which is the router resolveTarget will use for the
		// files found in it.
		cfg, err := e.workspaceConfig(abs)
		if err != nil {
			return nil, none, nil, err
		}
		found, err := sourceFiles(cfg.router, abs)
		if err != nil {
			return nil, none, nil, err
		}
		for _, path := range found {
			if !seen[path] {
				seen[path] = true
				candidates = append(candidates, path)
			}
		}
	}
	if len(candidates) == 0 {
		return nil, none, nil, render.Errorf(render.CodeNoServer,
			"no server handles any file in %s; name its language with --language",
			strings.Join(targets, ", ")).WithDetails(map[string]any{"paths": targets})
	}
	slices.Sort(candidates)

	match, err := e.resolveTarget(candidates[0], language, serverName)
	if err != nil {
		return nil, none, nil, err
	}
	files := make([]checkFile, 0, len(candidates))
	elsewhere := 0
	for _, path := range candidates {
		m, err := e.resolveTarget(path, language, serverName)
		if err != nil || m.Server.Name != match.Server.Name || m.Root != match.Root {
			elsewhere++
			continue
		}
		files = append(files, checkFile{path: path, languageID: m.LanguageID})
	}
	if elsewhere > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d file(s) are handled by another server or belong to another workspace root and were skipped; run check once per workspace (merging several servers' diagnostics is not implemented)",
			elsewhere))
	}
	if len(files) > maxFiles {
		warnings = append(warnings, fmt.Sprintf(
			"checked %d of %d files (--max-files %d); the report is incomplete",
			maxFiles, len(files), maxFiles))
		files = files[:maxFiles]
	}
	return files, match, warnings, nil
}

// sourceFiles lists the files under dir that some server claims, in
// lexical order. It is anchorFile's walk with every match kept: the
// same skip list, the same bound, and the same refusal to let an
// unreadable subtree fail the whole command.
func sourceFiles(r *router.Router, dir string) ([]string, error) {
	var (
		out     []string
		scanned int
	)
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if scanned++; scanned > anchorScanLimit {
			return filepath.SkipAll
		}
		if d.IsDir() {
			if path != dir && skipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if _, err := r.Resolve(path); err != nil {
			return nil
		}
		out = append(out, path)
		return nil
	})
	if err != nil {
		return nil, render.Errorf(render.CodeIOError, "walking %s: %v", dir, err)
	}
	return out, nil
}

// pullDiagnostics collects diagnostics with textDocument/diagnostic:
// one gated request per file, with an answer that ends.
//
// relatedDocuments are kept. A server is allowed to answer about a
// file's dependents in the same breath — that is what the capability
// this command advertises invites — and dropping them would lose
// diagnostics that were handed to us.
func pullDiagnostics(s *session, common *commonFlags, uris []protocol.DocumentURI) (
	map[protocol.DocumentURI][]lspDiagnostic, []string, error) {
	byURI := map[protocol.DocumentURI][]lspDiagnostic{}
	var warnings []string
	for _, uri := range uris {
		ctx, cancel := context.WithTimeout(s.base, common.timeout+gateSlack)
		res, err := s.query(ctx, methodDocumentDiagnostic, map[string]any{
			"textDocument": map[string]any{"uri": string(uri)},
		})
		cancel()
		if err != nil {
			return nil, warnings, err
		}
		warnings = append(warnings, res.Warnings...)
		reports, err := decodeDiagnosticReport(res.Result, uri)
		if err != nil {
			return nil, warnings, err
		}
		for reportURI, diags := range reports {
			byURI[reportURI] = append(byURI[reportURI], diags...)
		}
		// An "unchanged" report carries no items, but the file has
		// still been reported on: recording the key is what keeps a
		// clean file distinguishable from a silent one.
		if _, ok := byURI[uri]; !ok {
			byURI[uri] = nil
		}
	}
	return byURI, warnings, nil
}

// pushDiagnostics collects diagnostics from publishDiagnostics, and
// decides when they are all in. See the rule at the top of this file.
func pushDiagnostics(s *session, common *commonFlags,
	uris []protocol.DocumentURI, allowSilent bool) (
	map[protocol.DocumentURI][]lspDiagnostic, []string, error) {
	var warnings []string

	// Condition (1): the same readiness question every other command
	// asks, answered by the same gate. A workspace that never loads is
	// exit 5 here exactly as it is for `references`.
	readyCtx, cancelReady := context.WithTimeout(s.base, common.timeout+gateSlack)
	err := s.lsp.AwaitReady(readyCtx)
	cancelReady()
	if err != nil {
		return nil, warnings, s.translate(methodPublishDiagnostics, err)
	}

	// Conditions (2) and (3).
	settle := common.settle
	deadline := time.Now().Add(common.timeout)
	var (
		missing   []protocol.DocumentURI
		publishes int
	)
	for {
		st, err := s.diagnosticsState(uris, false)
		if err != nil {
			return nil, warnings, err
		}
		missing = missing[:0]
		for _, raw := range st.Missing {
			missing = append(missing, protocol.DocumentURI(raw))
		}
		publishes = st.Publishes
		// Silence before the first word is not the same as silence
		// after the last: a session that has published nothing at all
		// is not quiet, it is unheard from.
		if len(missing) == 0 && st.Publishes > 0 && st.Quiet >= settle {
			return snapshotDiagnostics(s, uris, warnings)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		select {
		case <-time.After(min(checkPollInterval, remaining)):
		case <-s.base.Done():
			return nil, warnings, s.translate(methodPublishDiagnostics, s.base.Err())
		}
	}

	if len(missing) == 0 {
		// Every file was reported on, but the reports never stopped
		// arriving. That is a workspace still working, not an answer.
		return nil, warnings, render.Errorf(render.CodeNotReady,
			"%s kept publishing diagnostics for %s without pausing for %s; the report never settled",
			s.match.Server.Name, common.timeout, settle).
			WithDetails(map[string]any{"reason": "unstable", "publishes": publishes})
	}

	names := shortPaths(missing)
	if allowSilent {
		warnings = append(warnings, fmt.Sprintf(
			"%s never published diagnostics for %d file(s) (%s); --allow-silent means they are reported as clean, which is an assumption and not an answer",
			s.match.Server.Name, len(missing), strings.Join(names, ", ")))
		return snapshotDiagnostics(s, uris, warnings)
	}
	return nil, warnings, render.Errorf(render.CodeNotReady,
		"%s published no diagnostics for %d of %d file(s) (%s) within %s; a file the server never mentioned is unknown, not clean — pass --allow-silent to accept the silence, or --timeout to wait longer",
		s.match.Server.Name, len(missing), len(uris), strings.Join(names, ", "), common.timeout).
		WithDetails(map[string]any{
			"reason":  "no_diagnostics_published",
			"silent":  names,
			"checked": len(uris),
		})
}

// snapshotDiagnostics reads out everything the session has published.
func snapshotDiagnostics(s *session, uris []protocol.DocumentURI, warnings []string) (
	map[protocol.DocumentURI][]lspDiagnostic, []string, error) {
	st, err := s.diagnosticsState(uris, true)
	if err != nil {
		return nil, warnings, err
	}
	byURI, err := decodeDiagnosticsSnapshot(st)
	return byURI, warnings, err
}

// shortPaths renders URIs as paths a caller would recognise, capped so
// that a thousand silent files do not produce a thousand-line message.
func shortPaths(uris []protocol.DocumentURI) []string {
	const limit = 10
	out := make([]string, 0, min(len(uris), limit+1))
	for i, uri := range uris {
		if i == limit {
			out = append(out, fmt.Sprintf("… and %d more", len(uris)-limit))
			break
		}
		parsed, err := protocol.ParseDocumentURI(string(uri))
		if err != nil {
			out = append(out, string(uri))
			continue
		}
		out = append(out, shortPath(parsed.Path()))
	}
	return out
}

// diagnosticSet converts the collected LSP diagnostics into the
// renderable set, resolving every range through the document store's
// Mapper so that the reported columns are byte columns (PLAN §5.1).
//
// A diagnostic whose range does not fit its file is dropped with a
// warning rather than silently rendered at 0:0 — it means the server's
// view of the file and ours disagree, which the caller should hear
// about.
func diagnosticSet(s *session, byURI map[protocol.DocumentURI][]lspDiagnostic) (render.DiagnosticSet, []string) {
	var (
		ds       render.DiagnosticSet
		warnings []string
	)
	uris := make([]string, 0, len(byURI))
	for uri := range byURI {
		uris = append(uris, string(uri))
	}
	slices.Sort(uris)

	for _, raw := range uris {
		uri := protocol.DocumentURI(raw)
		diags := byURI[uri]
		if len(diags) == 0 {
			continue
		}
		m, err := s.docs.MapperForURI(uri)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("dropped %d diagnostic(s) in %s: %v", len(diags), uri, err))
			continue
		}
		for _, d := range diags {
			out, err := render.NewDiagnostic(m, d.Range, d.severity(), d.Message)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("dropped a diagnostic in %s: %v", uri, err))
				continue
			}
			out.Code = d.code()
			out.Source = d.Source
			out.Tags = d.tagNames()
			ds.Diagnostics = append(ds.Diagnostics, out)
		}
	}
	return ds, warnings
}

// lspDiagnostic is the wire shape of an LSP Diagnostic, decoded only
// as far as this command reports it.
type lspDiagnostic struct {
	Range    protocol.Range `json:"range"`
	Severity *int           `json:"severity"`
	// Code is `integer | string` in the protocol, so it stays raw
	// until it is stringified.
	Code    json.RawMessage `json:"code"`
	Source  string          `json:"source"`
	Message string          `json:"message"`
	Tags    []int           `json:"tags"`
}

// severity maps the wire severity onto the renderer's. An absent
// severity stays unset rather than becoming an error: render treats it
// as a warning, which neither hides an unlabelled problem nor lets it
// decide the exit code.
func (d lspDiagnostic) severity() render.Severity {
	if d.Severity == nil {
		return render.SeverityUnset
	}
	return render.Severity(*d.Severity)
}

// code stringifies the integer-or-string diagnostic code.
func (d lspDiagnostic) code() string {
	if isJSONNull(d.Code) {
		return ""
	}
	var s string
	if err := json.Unmarshal(d.Code, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(d.Code))
}

// tagNames renders DiagnosticTag numbers as names. An unknown tag from
// a newer protocol revision keeps its number rather than disappearing.
func (d lspDiagnostic) tagNames() []string {
	if len(d.Tags) == 0 {
		return nil
	}
	out := make([]string, 0, len(d.Tags))
	for _, tag := range d.Tags {
		switch tag {
		case 1:
			out = append(out, "unnecessary")
		case 2:
			out = append(out, "deprecated")
		default:
			out = append(out, fmt.Sprintf("tag-%d", tag))
		}
	}
	return out
}

// decodeDiagnosticReport decodes a textDocument/diagnostic answer: a
// full report with items, an unchanged report with none, and either
// one's relatedDocuments.
func decodeDiagnosticReport(raw json.RawMessage, uri protocol.DocumentURI) (
	map[protocol.DocumentURI][]lspDiagnostic, error) {
	out := map[protocol.DocumentURI][]lspDiagnostic{}
	if isJSONNull(raw) {
		return out, nil
	}
	var report struct {
		Kind             string          `json:"kind"`
		Items            []lspDiagnostic `json:"items"`
		RelatedDocuments map[string]struct {
			Kind  string          `json:"kind"`
			Items []lspDiagnostic `json:"items"`
		} `json:"relatedDocuments"`
	}
	if err := json.Unmarshal(raw, &report); err != nil {
		return nil, protocolError("diagnostic report", err)
	}
	out[uri] = report.Items
	for related, sub := range report.RelatedDocuments {
		parsed, err := protocol.ParseDocumentURI(related)
		if err != nil {
			continue // a URI we cannot read is dropped by diagnosticSet's warning path
		}
		out[parsed] = append(out[parsed], sub.Items...)
	}
	return out, nil
}

// diagnosticsState asks the pool what the session's server has
// published about some files. The record lives with the session — in
// the daemon, that outlives this command — and not here, because
// diagnostics are pushed whenever the server likes and a warm server
// has already said most of what it is going to say.
func (s *session) diagnosticsState(uris []protocol.DocumentURI, snapshot bool) (*daemon.DiagnosticsState, error) {
	names := make([]string, len(uris))
	for i, uri := range uris {
		names[i] = string(uri)
	}
	ctx, cancel := s.requestContext()
	defer cancel()
	st, err := s.h.Diagnostics(ctx, daemon.DiagnosticsRequest{Target: s.target, URIs: names, Snapshot: snapshot})
	if err != nil {
		return nil, s.translate(methodPublishDiagnostics, err)
	}
	return st, nil
}

// decodeDiagnosticsSnapshot decodes everything the session has been
// told, one array per file.
func decodeDiagnosticsSnapshot(st *daemon.DiagnosticsState) (map[protocol.DocumentURI][]lspDiagnostic, error) {
	out := make(map[protocol.DocumentURI][]lspDiagnostic, len(st.ByURI))
	for raw, diags := range st.ByURI {
		uri, err := protocol.ParseDocumentURI(raw)
		if err != nil {
			continue
		}
		var decoded []lspDiagnostic
		if err := json.Unmarshal(diags, &decoded); err != nil {
			return nil, protocolError("publishDiagnostics", err)
		}
		out[uri] = decoded
	}
	return out, nil
}
