package serverdef

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// A Severity grades one [Check]. It is a string so that it survives the
// JSON envelope unchanged.
type Severity string

// The severities, increasing.
const (
	// SeverityOK: nothing to do.
	SeverityOK Severity = "ok"
	// SeverityInfo: worth knowing, not worth acting on.
	SeverityInfo Severity = "info"
	// SeverityWarn: lightspeed will work, but less well than it could.
	SeverityWarn Severity = "warn"
	// SeverityError: something will not work.
	SeverityError Severity = "error"
)

var severityRank = map[Severity]int{SeverityOK: 0, SeverityInfo: 1, SeverityWarn: 2, SeverityError: 3}

// Worse reports whether s is more severe than other.
func (s Severity) Worse(other Severity) bool { return severityRank[s] > severityRank[other] }

// The [Check] identifiers `doctor` emits. They are stable: an agent may
// branch on them, so new checks get new ids rather than reusing one.
const (
	// CheckConfigLayer: one configuration layer was consulted.
	CheckConfigLayer = "config_layer"
	// CheckConfigFile: one configuration file could not be used.
	CheckConfigFile = "config_file"
	// CheckLayerOverride: a stronger layer overrode a weaker one. Not
	// a fault — the point of the layers — but never silent.
	CheckLayerOverride = "layer_override"
	// CheckServerBinary: a server's executable, found or not.
	CheckServerBinary = "server_binary"
	// CheckMise: whether installation can be delegated.
	CheckMise = "mise"
	// CheckOffline: whether the network kill switch is on.
	CheckOffline = "offline"
	// CheckPathRouting: whether a path given to doctor has a server.
	CheckPathRouting = "path_routing"
	// CheckServersConfigured: whether any server is configured at all.
	CheckServersConfigured = "servers_configured"
)

// A Check is one diagnosis: what was looked at, what was found, and the
// command that would fix it. It is data — `doctor` renders it, it does
// not print itself.
type Check struct {
	// ID is one of the Check* constants.
	ID string `json:"id"`
	// Severity grades the finding.
	Severity Severity `json:"severity"`
	// Subject is what the check is about: a server name, a file, a
	// path, or empty for a global check.
	Subject string `json:"subject,omitempty"`
	// Message is one line, for humans.
	Message string `json:"message"`
	// Detail is the longer explanation, empty when there is nothing to
	// add.
	Detail string `json:"detail,omitempty"`
	// Fix is the argv that would resolve the finding, if a single
	// command would.
	Fix []string `json:"fix,omitempty"`
}

func (c Check) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s] %s", c.Severity, c.ID)
	if c.Subject != "" {
		fmt.Fprintf(&b, " %s", c.Subject)
	}
	fmt.Fprintf(&b, ": %s", c.Message)
	if len(c.Fix) > 0 {
		fmt.Fprintf(&b, " — run: %s", strings.Join(c.Fix, " "))
	}
	return b.String()
}

// A ServerStatus is one line of `lightspeed servers`: what is
// configured, what it resolved to, which layer won, and whether it is
// installed.
type ServerStatus struct {
	// Name is the server's name.
	Name string `json:"name"`
	// Def is the effective definition.
	Def *ServerDef `json:"definition"`
	// Origin is the layer that won.
	Origin Origin `json:"origin"`
	// Overrides is every contributing layer, strongest first.
	Overrides []Override `json:"overrides"`
	// Shadowed is the contributions the winner overrode, strongest
	// first, so that a report can show them without recomputing.
	Shadowed []Override `json:"shadowed,omitempty"`
	// Binary is where the executable is, and whether it runs.
	Binary Binary `json:"binary"`
	// Installed is [Binary.Runnable], hoisted because it is the field
	// every consumer wants.
	Installed bool `json:"installed"`
	// InstallCommand is the argv that would install it, nil when the
	// definition names no install spec.
	InstallCommand []string `json:"install_command,omitempty"`
}

// A ServersReport is the structured answer of `lightspeed servers`.
type ServersReport struct {
	// Servers are the configured servers, ordered by name.
	Servers []ServerStatus `json:"servers"`
	// Layers records what each layer contributed, strongest first.
	Layers []LayerStatus `json:"layers"`
	// Mise is the installer's status.
	Mise MiseStatus `json:"mise"`
	// Offline reports the kill switch.
	Offline bool `json:"offline"`
	// Problems are configuration files that could not be used. The
	// report is still returned: knowing that five servers resolved and
	// one file is broken beats knowing nothing.
	Problems []Problem `json:"problems,omitempty"`
}

// Installed counts the servers whose executable was found and runs.
func (r *ServersReport) Installed() int {
	n := 0
	for _, s := range r.Servers {
		if s.Installed {
			n++
		}
	}
	return n
}

// Servers reports what is configured and what resolved, for
// `lightspeed servers`. It probes for executables, so it runs mise
// (locally, never the network) unless [Options.SkipMise] is set.
//
// A broken configuration file does not fail the report — it is listed in
// Problems — because the command's whole job is to explain the current
// state, including a bad one.
func Servers(ctx context.Context, opts Options) (*ServersReport, error) {
	res := load(opts)
	res.Probe(ctx, opts)
	return res.ServersReport(), nil
}

// ServersReport turns a resolution into the report of [Servers].
func (r *Resolution) ServersReport() *ServersReport {
	report := &ServersReport{
		Layers:   r.Layers,
		Mise:     r.Mise,
		Offline:  r.Offline,
		Problems: r.Problems,
	}
	for _, s := range r.Servers {
		report.Servers = append(report.Servers, ServerStatus{
			Name:           s.Name(),
			Def:            s.Def,
			Origin:         s.Origin,
			Overrides:      s.Overrides,
			Shadowed:       s.Shadowed(),
			Binary:         s.Binary,
			Installed:      s.Installed(),
			InstallCommand: s.InstallCommand(),
		})
	}
	return report
}

// A DoctorReport is the structured answer of `lightspeed doctor`: every
// check, plus the same server table `servers` returns, so that a caller
// needs one invocation and not two.
type DoctorReport struct {
	// Checks are the findings, in the order they were made: global
	// state first, then per layer, then per server, then per path.
	Checks []Check `json:"checks"`
	// Servers is the resolved server table.
	Servers []ServerStatus `json:"servers"`
	// Layers records what each layer contributed, strongest first.
	Layers []LayerStatus `json:"layers"`
	// Mise is the installer's status.
	Mise MiseStatus `json:"mise"`
	// Offline reports the kill switch.
	Offline bool `json:"offline"`
}

// Worst is the highest severity in the report, [SeverityOK] if all is
// well. A CLI maps it to an exit code; the report itself does not.
func (r *DoctorReport) Worst() Severity {
	worst := SeverityOK
	for _, c := range r.Checks {
		if c.Severity.Worse(worst) {
			worst = c.Severity
		}
	}
	return worst
}

// OK reports that nothing worse than information was found.
func (r *DoctorReport) OK() bool { return !r.Worst().Worse(SeverityInfo) }

// Doctor diagnoses a machine's server setup, for `lightspeed doctor`.
// It answers, in one pass: is there a server for these paths, is each
// configured server's binary there and runnable, is mise present, do the
// configuration layers conflict, and is offline mode on.
//
// paths are optional; each is checked against [Options.PathCheck], which
// the CLI wires to internal/router. Without a PathCheck a path can only
// be reported as unchecked, which is itself a finding rather than a
// silent omission.
//
// Doctor never fails on a broken configuration: reporting the breakage
// is the job. It returns an error only if it could not run at all.
func Doctor(ctx context.Context, paths []string, opts Options) (*DoctorReport, error) {
	res := load(opts)
	res.Probe(ctx, opts)

	report := &DoctorReport{
		Servers: res.ServersReport().Servers,
		Layers:  res.Layers,
		Mise:    res.Mise,
		Offline: res.Offline,
	}
	report.Checks = append(report.Checks, offlineCheck(res))
	report.Checks = append(report.Checks, miseCheck(res))
	report.Checks = append(report.Checks, layerChecks(res)...)
	report.Checks = append(report.Checks, serverChecks(res)...)
	report.Checks = append(report.Checks, pathChecks(paths, opts)...)
	return report, nil
}

func offlineCheck(res *Resolution) Check {
	if res.Offline {
		return Check{
			ID:       CheckOffline,
			Severity: SeverityInfo,
			Message:  "offline mode is active: nothing will be downloaded",
			Detail:   "cleared by unsetting " + EnvOffline + " and dropping --offline; resolution and queries are unaffected, only installation is refused",
		}
	}
	return Check{ID: CheckOffline, Severity: SeverityOK, Message: "offline mode is off"}
}

func miseCheck(res *Resolution) Check {
	switch {
	case res.Mise.Available:
		return Check{
			ID:       CheckMise,
			Severity: SeverityOK,
			Subject:  MiseName,
			Message:  fmt.Sprintf("mise %s is available at %s", res.Mise.Version, res.Mise.Path),
		}
	case res.Mise.Skipped:
		return Check{
			ID:       CheckMise,
			Severity: SeverityInfo,
			Subject:  MiseName,
			Message:  "mise was not consulted",
			Detail:   res.Mise.Problem,
		}
	default:
		return Check{
			ID:       CheckMise,
			Severity: SeverityWarn,
			Subject:  MiseName,
			Message:  "mise is not available, so `lightspeed install` cannot install anything",
			Detail:   res.Mise.Problem + "; lightspeed never downloads servers itself (PLAN §6), so servers must come from mise or from PATH",
		}
	}
}

// layerChecks reports each layer, every file that could not be used, and
// every override — the "config layer conflicts" half of the diagnosis.
func layerChecks(res *Resolution) []Check {
	var checks []Check
	for _, layer := range res.Layers {
		check := Check{ID: CheckConfigLayer, Severity: SeverityOK, Subject: layer.Layer.String()}
		switch {
		case layer.Skipped != "":
			check.Severity = SeverityInfo
			check.Message = fmt.Sprintf("%s layer not consulted: %s", layer.Layer, layer.Skipped)
		case layer.Layer == LayerBuiltin:
			check.Message = fmt.Sprintf("%d built-in default(s): %s", len(layer.Servers), strings.Join(layer.Servers, ", "))
		case !layer.Exists:
			check.Severity = SeverityInfo
			check.Message = fmt.Sprintf("%s is not there, so the %s layer contributes nothing", layer.Path, layer.Layer)
		case len(layer.Files) == 0:
			check.Severity = SeverityInfo
			check.Message = fmt.Sprintf("%s holds no *.toml file", layer.Path)
		default:
			check.Message = fmt.Sprintf("%d file(s) in %s define %s",
				len(layer.Files), layer.Path, strings.Join(layer.Servers, ", "))
		}
		checks = append(checks, check)
	}

	for _, problem := range res.Problems {
		checks = append(checks, Check{
			ID:       CheckConfigFile,
			Severity: SeverityError,
			Subject:  problem.Origin.File,
			Message:  problem.Message,
			Detail:   "this file is not being used at all; fix it or remove it",
		})
	}

	for _, s := range res.Servers {
		shadowed := s.Shadowed()
		if len(shadowed) == 0 {
			continue
		}
		var parts []string
		for _, o := range shadowed {
			parts = append(parts, o.String())
		}
		keys := s.Overrides[0].Keys
		message := fmt.Sprintf("%s overrides %s", s.Origin, strings.Join(parts, ", then "))
		if !s.Overrides[0].Whole {
			message = fmt.Sprintf("%s sets %s, overriding %s", s.Origin, strings.Join(keys, ", "), strings.Join(parts, ", then "))
		}
		checks = append(checks, Check{
			ID:       CheckLayerOverride,
			Severity: SeverityInfo,
			Subject:  s.Name(),
			Message:  message,
		})
	}
	return checks
}

// serverChecks reports each server's executable: the "server on PATH but
// not runnable" and "no server installed" halves of the diagnosis.
func serverChecks(res *Resolution) []Check {
	if len(res.Servers) == 0 {
		return []Check{{
			ID:       CheckServersConfigured,
			Severity: SeverityError,
			Message:  "no server is configured at all",
			Detail:   "the built-in defaults are disabled or empty; write a " + WorkspaceFile + " or re-enable them",
		}}
	}
	checks := make([]Check, 0, len(res.Servers))
	for _, s := range res.Servers {
		check := Check{ID: CheckServerBinary, Subject: s.Name()}
		switch {
		case s.Installed():
			check.Severity = SeverityOK
			check.Message = fmt.Sprintf("%s: %s (%s)", s.Binary.Name, s.Binary.Path, s.Binary.Source)
		case s.Binary.Path != "":
			check.Severity = SeverityError
			check.Message = fmt.Sprintf("%s is at %s but cannot be run: %s", s.Binary.Name, s.Binary.Path, s.Binary.Problem)
			check.Detail = "a shim pointing at an uninstalled version, or a file without the executable bit, looks exactly like this"
			check.Fix = s.InstallCommand()
		default:
			check.Severity = SeverityWarn
			check.Message = fmt.Sprintf("%s is not installed (%s)", s.Binary.Name, s.Binary.Problem)
			check.Detail = fmt.Sprintf("queries routed to %s will exit 3 until it is installed", s.Name())
			check.Fix = s.InstallCommand()
		}
		checks = append(checks, check)
	}
	slices.SortStableFunc(checks, func(a, b Check) int { return strings.Compare(a.Subject, b.Subject) })
	return checks
}

// pathChecks answers "is there a server for this path", the first
// question a user with a silent lightspeed asks.
func pathChecks(paths []string, opts Options) []Check {
	var checks []Check
	for _, path := range paths {
		if opts.PathCheck == nil {
			checks = append(checks, Check{
				ID:       CheckPathRouting,
				Severity: SeverityWarn,
				Subject:  path,
				Message:  "routing was not checked: no router was supplied to doctor",
			})
			continue
		}
		servers, err := opts.PathCheck(path)
		switch {
		case err != nil:
			checks = append(checks, Check{
				ID:       CheckPathRouting,
				Severity: SeverityError,
				Subject:  path,
				Message:  err.Error(),
				Detail:   "add an activation glob or language id in " + WorkspaceFile + " if this file should be handled",
			})
		case len(servers) == 0:
			checks = append(checks, Check{
				ID:       CheckPathRouting,
				Severity: SeverityError,
				Subject:  path,
				Message:  "no server handles this path",
			})
		default:
			checks = append(checks, Check{
				ID:       CheckPathRouting,
				Severity: SeverityOK,
				Subject:  path,
				Message:  "handled by " + strings.Join(servers, ", "),
			})
		}
	}
	return checks
}
