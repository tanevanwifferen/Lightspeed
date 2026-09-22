package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// The three commands of PLAN §4's last line that talk about servers
// rather than to them: `servers`, `install` and `doctor`. None of them
// starts a language server or a daemon, and none needs one to be
// running; they read configuration, look for executables, and — only for
// `install --run` — hand one command to mise.

// commandWorkspace names the workspace a server-management command is
// about: the root of the one --path (default the working directory)
// belongs to, resolved exactly as the commands that start a daemon do.
func commandWorkspace(path string) (string, error) {
	root, err := daemon.Workspace(path)
	if err != nil {
		return "", render.Errorf(render.CodeUsage, "resolving %s: %v", path, err)
	}
	return root, nil
}

// managementFormat resolves --format for the commands whose answer is a
// table rather than a result set: json or text.
func managementFormat(common *commonFlags, name string, w io.Writer) (render.Format, error) {
	format, err := common.resolveFormat(w)
	if err != nil {
		return "", err
	}
	if format != render.FormatJSON && format != render.FormatText {
		return "", render.Errorf(render.CodeUnsupportedFormat,
			"format %q has no meaning for %s (want one of json, text)", format, name)
	}
	return format, nil
}

// writeData writes one successful envelope, the way `daemon status`
// does, with the exit code the caller decided.
func (e *env) writeData(common *commonFlags, data any, warnings []string, exit int) int {
	err := render.WriteEnvelope(e.stdout, render.Envelope{
		Version: render.EnvelopeVersion, OK: true, Data: data, Warnings: warnings,
	}, render.Options{Indent: common.indent})
	if err != nil {
		return e.fail(err)
	}
	return exit
}

// --- servers ---

// serverEntry is one server of `lightspeed servers`: serverdef's status
// plus, for every key of the effective definition, the file that set it.
// serverdef records what each layer contributed and in what order;
// resolving that to "which one won this key" is the question a user
// actually asks, so it is answered once here rather than by every reader.
type serverEntry struct {
	serverdef.ServerStatus
	// KeyOrigins maps each dotted key of the definition to the layer
	// and file whose value is in effect.
	KeyOrigins map[string]serverdef.Origin `json:"key_origins"`
}

// serversData is the payload of `lightspeed servers`.
type serversData struct {
	// Workspace is the root whose .lightspeed.toml was read.
	Workspace string `json:"workspace"`
	// Installed is how many of the servers have a runnable executable.
	Installed  int                     `json:"installed"`
	Servers    []serverEntry           `json:"servers"`
	Layers     []serverdef.LayerStatus `json:"layers"`
	Mise       serverdef.MiseStatus    `json:"mise"`
	Provenance serverdef.Provenance    `json:"provenance"`
	Offline    bool                    `json:"offline"`
	// Problems are configuration files that could not be used. The
	// report is still produced — five servers resolved and one file
	// broken is worth knowing — and the exit code is 1.
	Problems []serverdef.Problem `json:"problems,omitempty"`
}

// keyOrigins resolves a definition's per-key provenance from the
// contributions, strongest first: the first layer to mention a key won it.
func keyOrigins(overrides []serverdef.Override) map[string]serverdef.Origin {
	out := map[string]serverdef.Origin{}
	for _, o := range overrides {
		for _, key := range o.Keys {
			if _, done := out[key]; !done {
				out[key] = o.Origin
			}
		}
	}
	return out
}

// serversCommand implements `lightspeed servers [--path DIR]`: every
// configured server, which layer each key came from, and whether its
// executable is there.
func serversCommand(e *env, c *command, args []string) int {
	var path string
	common, _, err := parseFlagsRange(e, c, args, 0, 0, func(fs *flag.FlagSet) {
		fs.StringVar(&path, "path", ".", "any file or directory inside the workspace whose .lightspeed.toml applies")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "servers", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	root, err := commandWorkspace(path)
	if err != nil {
		return e.fail(err)
	}

	ctx, cancel := context.WithTimeout(e.base(), common.timeout)
	defer cancel()
	report, err := serverdef.Servers(ctx, e.serverdefOptions(root))
	if err != nil {
		return e.fail(serverdefFailure(err))
	}

	data := serversData{
		Workspace:  root,
		Installed:  report.Installed(),
		Servers:    make([]serverEntry, 0, len(report.Servers)),
		Layers:     report.Layers,
		Mise:       report.Mise,
		Provenance: report.Provenance,
		Offline:    report.Offline,
		Problems:   report.Problems,
	}
	for _, s := range report.Servers {
		data.Servers = append(data.Servers, serverEntry{ServerStatus: s, KeyOrigins: keyOrigins(s.Overrides)})
	}

	var warnings []string
	for _, p := range report.Problems {
		warnings = append(warnings, "unusable configuration: "+p.Message)
	}
	exit := ExitOK
	if len(report.Problems) > 0 {
		exit = ExitProblems
	}

	if format == render.FormatText {
		writeServersText(e.stdout, data)
		return exit
	}
	return e.writeData(common, data, warnings, exit)
}

// writeServersText is the human reading of `lightspeed servers`: one line
// per server, then, indented, every key that a stronger layer took from
// the one that names the server.
func writeServersText(w io.Writer, d serversData) {
	fmt.Fprintf(w, "workspace: %s\n", d.Workspace)
	fmt.Fprintf(w, "%s; %d of %d servers installed", d.Mise, d.Installed, len(d.Servers))
	if d.Offline {
		fmt.Fprint(w, "; offline")
	}
	fmt.Fprintln(w)
	for _, l := range d.Layers {
		status := "not present"
		switch {
		case l.Skipped != "":
			status = "skipped: " + l.Skipped
		case l.Exists && len(l.Files) > 0:
			status = strings.Join(l.Files, ", ")
		case l.Exists:
			status = "present, no files"
		}
		fmt.Fprintf(w, "  layer %-9s %s\n", l.Layer, status)
	}
	for _, p := range d.Problems {
		fmt.Fprintf(w, "  UNUSABLE %s: %s\n", p.Origin, p.Message)
	}
	fmt.Fprintln(w)
	for _, s := range d.Servers {
		state := "missing"
		where := s.Binary.Name
		if s.Installed {
			state = "installed"
			where = fmt.Sprintf("%s (%s)", s.Binary.Path, s.Binary.Source)
		} else if s.Binary.Path != "" {
			// Something is there and does not run: not "missing", and the
			// reason is what the reader needs before the command.
			state = "unusable"
			where = fmt.Sprintf("%s %s", s.Binary.Path, s.Binary.Problem)
			if len(s.InstallCommand) > 0 {
				where += "; run: " + strings.Join(s.InstallCommand, " ")
			}
		} else if len(s.InstallCommand) > 0 {
			where = strings.Join(s.InstallCommand, " ")
		}
		fmt.Fprintf(w, "%-15s %-9s %s\n", s.Name, state, where)
		if s.Binary.Note != "" {
			fmt.Fprintf(w, "    note: %s\n", s.Binary.Note)
		}
		fmt.Fprintf(w, "    defined by %s\n", s.Origin)
		for _, key := range sortedKeys(s.KeyOrigins) {
			if origin := s.KeyOrigins[key]; origin != s.Origin {
				fmt.Fprintf(w, "    %-30s %s\n", key, origin)
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// --- install ---

// defaultInstallTimeout is how long `install --run` lets mise work. A
// language server built from source (gopls through `go install`) takes
// minutes, and the 30 seconds that bound a query would kill it half done.
const defaultInstallTimeout = 15 * time.Minute

// installData is the payload of `lightspeed install`: serverdef's result
// unchanged, so that what was planned and what ran are the same fields.
type installData = serverdef.InstallResult

// installCommand implements `lightspeed install <name>`.
//
// By default it plans: it prints the exact mise command and runs nothing.
// PLAN §6's posture is that nothing downloads implicitly, and a command an
// agent may try "to see what it does" is an implicit download if it does
// one. `--run` is the explicit request, and `--offline` refuses it.
func installCommand(e *env, c *command, args []string) int {
	var (
		path           string
		version        string
		run            bool
		installTimeout time.Duration
	)
	common, positional, err := parseFlags(e, c, args, 1, func(fs *flag.FlagSet) {
		fs.StringVar(&path, "path", ".", "any file or directory inside the workspace whose .lightspeed.toml applies")
		fs.StringVar(&version, "version", "", "install this version instead of the one the definition names")
		fs.BoolVar(&run, "run", false, "actually run the install (default: print the plan and change nothing)")
		fs.DurationVar(&installTimeout, "install-timeout", defaultInstallTimeout, "how long to let mise work when --run is given")
	})
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "install", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if installTimeout <= 0 {
		return e.usagef("install: --install-timeout must be positive (got %s)", installTimeout)
	}
	root, err := commandWorkspace(path)
	if err != nil {
		return e.fail(err)
	}

	opts := e.serverdefOptions(root)
	res, err := serverdef.Load(opts)
	if err != nil {
		return e.fail(serverdefFailure(err))
	}
	ctx, cancel := context.WithTimeout(e.base(), installTimeout)
	defer cancel()
	result, err := res.Install(ctx, serverdef.InstallRequest{Name: positional[0], Version: version, DryRun: !run}, opts)
	if err != nil {
		return e.fail(serverdefFailure(err))
	}

	var warnings []string
	if !result.Ran {
		warnings = append(warnings, fmt.Sprintf(
			"nothing was installed: this is the plan. Run it yourself, or repeat with --run: lightspeed install %s --run",
			positional[0]))
	}
	warnings = append(warnings, result.Warnings...)

	if format == render.FormatText {
		writeInstallText(e.stdout, result)
		return ExitOK
	}
	return e.writeData(common, installData(*result), warnings, ExitOK)
}

// writeInstallText is the human reading of `lightspeed install`.
func writeInstallText(w io.Writer, r *serverdef.InstallResult) {
	if !r.Ran {
		fmt.Fprintf(w, "would run: %s\n", strings.Join(r.Plan.Use, " "))
		fmt.Fprintf(w, "then find it with: %s\n", strings.Join(r.Plan.Which, " "))
		fmt.Fprintln(w, "nothing was installed; repeat with --run to do it")
		return
	}
	fmt.Fprintf(w, "ran: %s\n", strings.Join(r.Plan.Use, " "))
	if r.Output != "" {
		fmt.Fprintln(w, r.Output)
	}
	if r.Path != "" {
		fmt.Fprintf(w, "%s is at %s\n", r.Plan.Binary, r.Path)
	}
	for _, warning := range r.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warning)
	}
}

// --- doctor ---

// doctorData is the payload of `lightspeed doctor`: serverdef's report,
// and the workspace it was read for.
type doctorData struct {
	Workspace string `json:"workspace"`
	// Worst is the highest severity found: ok, info, warn or error. The
	// exit code is 1 for error and 0 otherwise.
	Worst serverdef.Severity `json:"worst"`
	*serverdef.DoctorReport
}

// doctorCommand implements `lightspeed doctor [path...]`: is there a
// server for these paths, is each configured server's executable there
// and runnable, is mise, do the layers agree, and is offline on. Exit 1
// when any finding is an error, so that it can gate a script.
func doctorCommand(e *env, c *command, args []string) int {
	common, paths, err := parseFlagsRange(e, c, args, 0, -1, nil)
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "doctor", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	first := "."
	if len(paths) > 0 {
		first = paths[0]
	}
	root, err := commandWorkspace(first)
	if err != nil {
		return e.fail(err)
	}

	opts := e.serverdefOptions(root)
	opts.PathCheck = e.pathCheck
	ctx, cancel := context.WithTimeout(e.base(), common.timeout)
	defer cancel()
	report, err := serverdef.Doctor(ctx, paths, opts)
	if err != nil {
		return e.fail(serverdefFailure(err))
	}

	data := doctorData{Workspace: root, Worst: report.Worst(), DoctorReport: report}
	exit := ExitOK
	if report.Worst() == serverdef.SeverityError {
		exit = ExitProblems
	}
	if format == render.FormatText {
		writeDoctorText(e.stdout, data)
		return exit
	}
	return e.writeData(common, data, nil, exit)
}

// pathCheck answers serverdef.Doctor's "which servers claim this path",
// with the router built from the definitions of the path's own workspace.
// serverdef cannot do this itself: internal/router imports it.
func (e *env) pathCheck(path string) ([]string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg, err := e.workspaceConfig(abs)
	if err != nil {
		return nil, err
	}
	matches, err := cfg.router.Resolve(abs)
	if err != nil {
		var none *router.NoServerError
		if errors.As(err, &none) {
			return nil, nil // "no server handles this path" is doctor's own wording
		}
		return nil, err
	}
	names := make([]string, len(matches))
	for i, m := range matches {
		names[i] = m.Server.Name
	}
	return names, nil
}

// writeDoctorText is the human reading of `lightspeed doctor`: one line
// per finding, worst last summarised.
func writeDoctorText(w io.Writer, d doctorData) {
	fmt.Fprintf(w, "workspace: %s\n", d.Workspace)
	for _, check := range d.Checks {
		fmt.Fprintln(w, check)
		if check.Detail != "" {
			fmt.Fprintf(w, "    %s\n", check.Detail)
		}
	}
	fmt.Fprintf(w, "result: %s\n", d.Worst)
}
