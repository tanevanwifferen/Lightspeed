package serverdef

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// MiseName is the installer lightspeed delegates every download to
// (PLAN §1, §6). lightspeed never fetches anything itself: mise owns
// the backends, the checksums and the lockfile, and this package owns
// exactly two invocations of it.
const MiseName = "mise"

// A Runner runs one command and returns what it said. It is the single
// seam through which this package touches processes, so that every test
// in it can stay hermetic.
//
// env carries "KEY=VALUE" entries added to the inherited environment,
// later entries winning. It is a parameter rather than a detail of the
// production runner because what is in it — see [miseProbeEnv] — is
// part of PLAN §6's security posture, and a test must be able to assert
// it rather than trust it.
type Runner func(ctx context.Context, env []string, name string, args ...string) (stdout, stderr string, err error)

// execRunner is the production Runner.
func execRunner(ctx context.Context, env []string, name string, args ...string) (string, string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.String(), errb.String(), err
}

// miseProbeEnv is the environment every mise call that only asks a
// question runs with. Probing must never install anything: PLAN §6 says
// nothing downloads implicitly, and answering "where is gopls?" by
// fetching gopls would be exactly the implicit fetch it forbids.
//
// mise as shipped today does not install from `which` or `--version`,
// so this changes no observed behaviour. It is here because the
// guarantee belongs to lightspeed and should not rest on an installer's
// current defaults: mise's own not_found_auto_install defaults to true,
// and one release could make that apply somewhere it does not today.
//
// It is deliberately not conditional on [Options.Offline]. A probe has
// no business downloading whether or not the kill switch is set, and
// `mise use` — the one call that may download — is unaffected because
// it does not run with this environment.
var miseProbeEnv = []string{
	"MISE_OFFLINE=1",
	"MISE_NOT_FOUND_AUTO_INSTALL=0",
}

// A MiseStatus is what was learned about the installer. It is part of
// every report, because "no server and no installer" and "no server but
// one command away" are different situations for an agent.
type MiseStatus struct {
	// Available reports that mise was found and answered.
	Available bool `json:"available"`
	// Path is the mise executable, when it was found.
	Path string `json:"path,omitempty"`
	// Version is mise's own version string, first line only.
	Version string `json:"version,omitempty"`
	// Skipped reports that mise was deliberately not consulted.
	Skipped bool `json:"skipped,omitempty"`
	// Problem is why mise is unavailable, empty when it is available.
	Problem string `json:"problem,omitempty"`
}

func (m MiseStatus) String() string {
	switch {
	case m.Skipped:
		return "mise: not consulted"
	case m.Available:
		return fmt.Sprintf("mise %s at %s", m.Version, m.Path)
	default:
		return "mise: unavailable (" + m.Problem + ")"
	}
}

// DetectMise looks for mise and asks it its version. It runs no network
// operation: `mise --version` is local, and so is every other call this
// package makes, which is what lets resolution work with --offline.
func DetectMise(ctx context.Context, opts Options) MiseStatus {
	if opts.SkipMise {
		return MiseStatus{Skipped: true, Problem: "disabled by the caller"}
	}
	path, err := opts.lookPath(MiseName)
	if err != nil {
		return MiseStatus{Problem: "not on PATH"}
	}
	stdout, stderr, err := opts.runner()(ctx, miseProbeEnv, path, "--version")
	if err != nil {
		return MiseStatus{Path: path, Problem: strings.TrimSpace(firstLine(stderr) + " " + err.Error())}
	}
	return MiseStatus{Available: true, Path: path, Version: firstLine(stdout)}
}

// miseWhich asks mise where a tool's executable is. It is the second
// half of PLAN §1's recipe — `mise use -g …`, then `mise which` — and
// is also how a tool installed by lightspeed is found in a shell whose
// PATH mise never touched.
func miseWhich(ctx context.Context, binary string, opts Options, mise MiseStatus) (string, bool) {
	if !mise.Available || mise.Path == "" {
		return "", false
	}
	stdout, _, err := opts.runner()(ctx, miseProbeEnv, mise.Path, "which", binary)
	if err != nil {
		return "", false
	}
	path := firstLine(stdout)
	if path == "" {
		return "", false
	}
	return path, true
}

// An InstallPlan is what `lightspeed install` would run, as data. It is
// produced without touching the network or the installer, so it is also
// the exact command a "server not installed" error quotes.
type InstallPlan struct {
	// Name is the server definition's name.
	Name string `json:"name"`
	// Binary is server.command[0], what the install must end up
	// providing.
	Binary string `json:"binary"`
	// Spec is the mise tool spec from install.mise, with any
	// requested version applied.
	Spec string `json:"spec"`
	// Use is the argv that installs the tool globally.
	Use []string `json:"use"`
	// Which is the argv that resolves the installed executable.
	Which []string `json:"which"`
}

func (p *InstallPlan) String() string { return strings.Join(p.Use, " ") }

// An InstallRequest names what to install.
type InstallRequest struct {
	// Name is the server to install, as the configuration calls it.
	Name string
	// Version overrides the version in the definition's install spec.
	// Empty keeps whatever the definition pinned, which for the
	// built-in table is usually nothing, leaving the choice to mise.
	Version string
	// DryRun returns the plan without running anything. It is not the
	// same as offline: a dry run is a question, offline is a refusal.
	DryRun bool
}

// An InstallResult reports what happened.
type InstallResult struct {
	// Plan is what was (or would have been) run.
	Plan *InstallPlan `json:"plan"`
	// Ran reports that mise was actually invoked.
	Ran bool `json:"ran"`
	// Path is where the server ended up, if mise could say.
	Path string `json:"path,omitempty"`
	// Output is mise's own output, trimmed. lightspeed adds nothing to
	// it: the installer's diagnosis is better than ours.
	Output string `json:"output,omitempty"`
	// Warnings are non-fatal remarks, such as an install that
	// succeeded while `mise which` still could not find the binary.
	Warnings []string `json:"warnings,omitempty"`
}

// PlanInstall returns the mise commands that would install the named
// server, without running anything. A definition with no install spec
// is a [NotInstalledError] carrying no command, because there is
// nothing to suggest — that is the honest answer, not a guess at a
// package name.
func (r *Resolution) PlanInstall(name, version string) (*InstallPlan, error) {
	resolved, ok := r.Server(name)
	if !ok {
		return nil, &NoSuchServerError{Name: name, Known: r.Names()}
	}
	// The definition, not the probe: InstallServer loads without
	// probing, so resolved.Binary is empty on that path and the
	// message would lose the name of the thing to install.
	binary := ""
	if len(resolved.Def.Server.Command) > 0 {
		binary = resolved.Def.Server.Command[0]
	}
	spec := resolved.Def.Install.Mise
	if spec == "" {
		return nil, &NotInstalledError{
			Name:   name,
			Binary: binary,
			Reason: "its definition has no install.mise spec, so there is nothing to delegate",
		}
	}
	if version != "" {
		spec = withVersion(spec, version)
	}
	return &InstallPlan{
		Name:   name,
		Binary: binary,
		Spec:   spec,
		Use:    []string{MiseName, "use", "-g", spec},
		Which:  []string{MiseName, "which", binary},
	}, nil
}

// InstallServer delegates installation to mise: `mise use -g <spec>` and then
// `mise which <binary>` to learn where it landed (PLAN §1). It is the
// only function in lightspeed that may cause a download, and it only
// ever does so on an explicit request — never as a side effect of
// resolving or of answering a query.
//
// It refuses, without running anything, when the offline kill switch is
// set or when mise is absent; PLAN §6 leaves lightspeed no fallback
// that downloads, and inventing one would be exactly the implicit
// fetch the plan forbids.
func InstallServer(ctx context.Context, req InstallRequest, opts Options) (*InstallResult, error) {
	// Load, not Resolve: installing one server is no reason to ask mise
	// where the other five are.
	res, err := Load(opts)
	if err != nil {
		return nil, err
	}
	return res.Install(ctx, req, opts)
}

// Install is [InstallServer] against an already-probed resolution, so a
// command that has just reported what is installed can act on it
// without loading the layers twice.
func (r *Resolution) Install(ctx context.Context, req InstallRequest, opts Options) (*InstallResult, error) {
	plan, err := r.PlanInstall(req.Name, req.Version)
	if err != nil {
		return nil, err
	}
	if req.DryRun {
		return &InstallResult{Plan: plan}, nil
	}
	if opts.offline() {
		return nil, &OfflineError{Action: "install " + req.Name, Command: plan.Use}
	}
	mise := r.Mise
	if !r.Probed {
		mise = DetectMise(ctx, opts)
	}
	if !mise.Available {
		return nil, &MiseUnavailableError{
			Action: "install " + req.Name,
			Err:    errorFromProblem(mise.Problem),
			Binary: plan.Binary,
		}
	}

	// No miseProbeEnv here: this is the one call in lightspeed that is
	// allowed to download, and it is only reached on an explicit
	// request with the offline switch clear.
	stdout, stderr, err := opts.runner()(ctx, nil, mise.Path, plan.Use[1:]...)
	output := trimOutput(stdout, stderr)
	if err != nil {
		return nil, &InstallFailedError{Name: req.Name, Command: plan.Use, Err: err, Output: output}
	}
	result := &InstallResult{Plan: plan, Ran: true, Output: output}

	if plan.Binary != "" {
		if path, ok := miseWhich(ctx, plan.Binary, opts, mise); ok {
			result.Path = path
		} else {
			result.Warnings = append(result.Warnings,
				fmt.Sprintf("%s use succeeded but `%s which %s` could not resolve the binary; check `mise ls -g`",
					MiseName, MiseName, plan.Binary))
		}
	}
	return result, nil
}

// withVersion replaces (or adds) the version of a mise tool spec.
//
// The separator is the last '@' that comes after the last '/', so that
// the leading '@' of a scoped npm package — "npm:@vtsls/language-server"
// — is not mistaken for a version.
func withVersion(spec, version string) string {
	tool, _ := splitSpec(spec)
	return tool + "@" + version
}

func splitSpec(spec string) (tool, version string) {
	at := strings.LastIndex(spec, "@")
	if at <= 0 {
		return spec, ""
	}
	if at < strings.LastIndex(spec, "/") {
		return spec, ""
	}
	if colon := strings.Index(spec, ":"); at <= colon+1 {
		return spec, ""
	}
	return spec[:at], spec[at+1:]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// trimOutput joins what mise said on both streams, in the order a shell
// would have shown it.
func trimOutput(stdout, stderr string) string {
	parts := make([]string, 0, 2)
	for _, s := range []string{stdout, stderr} {
		if t := strings.TrimSpace(s); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n")
}

func errorFromProblem(problem string) error {
	if problem == "" {
		return nil
	}
	return fmt.Errorf("%s", problem)
}
