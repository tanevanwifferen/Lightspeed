package serverdef

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Options configures loading, probing and installing. The zero value is
// the production default: the real environment, the real PATH, the real
// mise, no workspace file and no offline switch beyond what the
// environment says.
type Options struct {
	// WorkspaceRoot is the directory searched for [WorkspaceFile].
	// Empty skips the workspace layer, which is what a command that
	// has no workspace yet (`lightspeed servers` from anywhere) wants.
	WorkspaceRoot string

	// ConfigDir is the user configuration directory whose
	// [ServersDir] subdirectory is the user layer. Empty means
	// $LIGHTSPEED_CONFIG_DIR, else $XDG_CONFIG_HOME/lightspeed, else
	// ~/.config/lightspeed.
	ConfigDir string

	// SkipBuiltins drops the generated defaults, so that a config can
	// be inspected on its own.
	SkipBuiltins bool

	// SkipUser drops the user layer, for the same reason.
	SkipUser bool

	// SkipMise stops this package from executing mise at all. PATH
	// sniffing still happens; a mise-only tool simply looks missing.
	SkipMise bool

	// Offline is PLAN §6's kill switch, as passed by --offline. It is
	// ORed with [EnvOffline], so an environment that says offline can
	// never be argued out of it by a flag.
	Offline bool

	// Getenv reads the environment. Nil means os.Getenv.
	Getenv func(string) string

	// LookPath finds an executable on PATH. Nil means exec.LookPath.
	LookPath func(string) (string, error)

	// Run runs a command. Nil means a real os/exec. Only mise is ever
	// run through it.
	Run Runner

	// PathCheck reports which servers claim a path, for [Doctor]. It
	// is a function rather than a router because internal/router
	// imports this package, and the dependency must not point both
	// ways. The CLI supplies a router-backed closure.
	PathCheck func(path string) ([]string, error)
}

func (o Options) getenv(key string) string {
	if o.Getenv != nil {
		return o.Getenv(key)
	}
	return os.Getenv(key)
}

func (o Options) lookPath(name string) (string, error) {
	if o.LookPath != nil {
		return o.LookPath(name)
	}
	return exec.LookPath(name)
}

func (o Options) runner() Runner {
	if o.Run != nil {
		return o.Run
	}
	return execRunner
}

// offline reports whether the network kill switch is on, from either the
// flag or the environment.
func (o Options) offline() bool {
	return o.Offline || envTruthy(o.getenv(EnvOffline))
}

// envTruthy reads the loose booleans an environment variable carries in
// practice. Anything unrecognised and non-empty counts as set, because
// LIGHTSPEED_OFFLINE=yolo means the user wanted offline, not a debate.
func envTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// UserConfigDir reports the user configuration directory the user layer
// lives in, and how it was chosen. It is exported because `doctor` has
// to be able to say which directory it looked in, including when the
// answer is "none, there is no home".
func (o Options) UserConfigDir() (dir, why string) {
	if o.ConfigDir != "" {
		return o.ConfigDir, "set by the caller"
	}
	if v := o.getenv(EnvConfigDir); v != "" {
		return v, "from " + EnvConfigDir
	}
	if v := o.getenv(EnvConfigHome); v != "" {
		return filepath.Join(v, ConfigSubdir), "from " + EnvConfigHome
	}
	if home := o.getenv("HOME"); home != "" {
		return filepath.Join(home, ".config", ConfigSubdir), "from $HOME"
	}
	return "", "no " + EnvConfigHome + " and no $HOME"
}

// A Problem is one thing wrong with the configuration, kept with the
// resolution rather than thrown, so that `doctor` can report every
// problem instead of only the first.
type Problem struct {
	// Origin is the file at fault.
	Origin Origin `json:"origin"`
	// Err is what is wrong.
	Err error `json:"-"`
	// Message is Err as text, for the JSON envelope.
	Message string `json:"message"`
}

// A LayerStatus records what one layer contributed, including when the
// answer is nothing: `doctor` has to be able to say "I looked here and
// found no file", which is different from "I never looked".
type LayerStatus struct {
	// Layer is which layer this is.
	Layer Layer `json:"layer"`
	// Path is the file or directory consulted, empty for the built-in
	// table.
	Path string `json:"path,omitempty"`
	// Exists reports that Path was there.
	Exists bool `json:"exists"`
	// Files are the files actually read, in load order.
	Files []string `json:"files,omitempty"`
	// Servers are the names this layer mentioned, sorted.
	Servers []string `json:"servers,omitempty"`
	// Skipped says why the layer was not consulted, empty when it was.
	Skipped string `json:"skipped,omitempty"`
}

// A Resolved is one server after layering: the effective definition,
// every layer that contributed to it, and where its executable is.
type Resolved struct {
	// Def is the merged definition. It is a private copy; callers may
	// keep it and hand it to internal/router.
	Def *ServerDef `json:"definition"`

	// Origin is the strongest layer that mentioned this server, and
	// therefore the answer to "where does this come from?".
	Origin Origin `json:"origin"`

	// Overrides is every contribution, strongest first. The first
	// entry's origin is [Resolved.Origin]; the rest are what it
	// overrode, key by key, which is the shadowing PLAN §6 requires
	// to be visible.
	Overrides []Override `json:"overrides"`

	// Binary is the result of PATH sniffing (and of mise, if it was
	// consulted). Its zero value means the probe never ran.
	Binary Binary `json:"binary"`
}

// Name reports the server's name.
func (r *Resolved) Name() string { return r.Def.Name }

// Installed reports that the server's executable was found and can be
// run. A definition is useless until this is true, which is why every
// report carries it.
func (r *Resolved) Installed() bool { return r.Binary.Runnable }

// Shadowed lists the contributions the winning layer overrode, strongest
// first. Empty means nothing was overridden.
func (r *Resolved) Shadowed() []Override {
	if len(r.Overrides) < 2 {
		return nil
	}
	return r.Overrides[1:]
}

// InstallCommand is the argv that would install this server, or nil if
// its definition names no install spec. It is data, never run here.
func (r *Resolved) InstallCommand() []string {
	if r.Def.Install.Mise == "" {
		return nil
	}
	return []string{MiseName, "use", "-g", r.Def.Install.Mise}
}

// NotInstalledError describes this server's missing executable, or nil
// if it is installed. offline is threaded through so the message can
// warn that even the install command will refuse.
//
// This is PLAN §6's "nothing downloads implicitly": the caller gets an
// error carrying exit code 3 and the exact command to run.
func (r *Resolved) NotInstalledError(offline bool) error {
	if r.Installed() {
		return nil
	}
	reason := r.Binary.Problem
	if r.Binary.Path != "" {
		reason = fmt.Sprintf("%s %s", r.Binary.Path, r.Binary.Problem)
	}
	return &NotInstalledError{
		Name:           r.Name(),
		Binary:         r.Binary.Name,
		Reason:         reason,
		InstallCommand: r.InstallCommand(),
		Offline:        offline,
	}
}

// A Resolution is the whole answer of the layered load: every server
// that is configured, where each came from, and — after probing — where
// each executable is.
type Resolution struct {
	// Servers are the effective definitions, ordered by name.
	Servers []*Resolved `json:"servers"`

	// Layers records what each layer contributed, strongest first.
	Layers []LayerStatus `json:"layers"`

	// Problems are the configuration files that could not be used.
	// [Load] refuses to return a resolution while this is non-empty;
	// [Doctor] reads it instead, so that one broken file does not hide
	// the rest of the diagnosis.
	Problems []Problem `json:"problems,omitempty"`

	// Mise is what was learned about the installer, zero until the
	// resolution has been probed.
	Mise MiseStatus `json:"mise"`

	// Offline reports that the network kill switch was on.
	Offline bool `json:"offline"`

	// Probed reports that executables have been looked for, for every
	// server. [Resolution.ProbeServer] leaves it false while filling in
	// one server's [Binary], so the per-server [Binary.Probed] is the
	// authority on whether an answer exists.
	Probed bool `json:"probed"`

	byName      map[string]*Resolved
	miseChecked bool
}

// Server returns the resolved server with the given name.
func (r *Resolution) Server(name string) (*Resolved, bool) {
	res, ok := r.byName[name]
	return res, ok
}

// Names lists the configured server names, sorted.
func (r *Resolution) Names() []string {
	out := make([]string, len(r.Servers))
	for i, s := range r.Servers {
		out[i] = s.Name()
	}
	return out
}

// Definitions returns the effective definitions, ready to be handed to
// internal/router. The copies are the resolution's own; a router does
// not mutate them.
func (r *Resolution) Definitions() []*ServerDef {
	out := make([]*ServerDef, len(r.Servers))
	for i, s := range r.Servers {
		out[i] = s.Def
	}
	return out
}

// Require returns the named server, insisting that it is usable: an
// unknown name is a [NoSuchServerError] (exit 2) and a missing
// executable a [NotInstalledError] (exit 3, with the install command).
// It is the one call a command that is about to spawn a server needs.
func (r *Resolution) Require(name string) (*Resolved, error) {
	resolved, ok := r.Server(name)
	if !ok {
		return nil, &NoSuchServerError{Name: name, Known: r.Names()}
	}
	if !resolved.Binary.Probed {
		return nil, fmt.Errorf("server %q: resolution has not been probed for executables; call Resolve, Probe or ProbeServer", name)
	}
	if err := resolved.NotInstalledError(r.Offline); err != nil {
		return nil, err
	}
	return resolved, nil
}

// Err is the first configuration problem as an error, or nil. It is
// what turns a tolerant load into the strict one [Load] performs.
func (r *Resolution) Err() error {
	if len(r.Problems) == 0 {
		return nil
	}
	return r.Problems[0].Err
}

// Load folds the definition layers of PLAN §6 together — the workspace
// file over the user's servers.d over the built-in defaults, key by key
// — and returns the effective set with full provenance. It reads files
// and nothing else: no executable is looked for, no process is run.
//
// Any unusable configuration file is an error rather than a skipped
// file. A user override that silently does nothing because of a typo is
// the failure mode PLAN §6 exists to prevent.
func Load(opts Options) (*Resolution, error) {
	res := load(opts)
	if err := res.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// Resolve is [Load] followed by [Resolution.Probe]: the layers, then
// PATH sniffing and — only if mise is already installed — `mise which`.
// It is what every command that needs to run a server calls.
func Resolve(ctx context.Context, opts Options) (*Resolution, error) {
	res, err := Load(opts)
	if err != nil {
		return nil, err
	}
	res.Probe(ctx, opts)
	return res, nil
}

// Probe fills in [Resolved.Binary] for every server, and
// [Resolution.Mise]. It executes mise (`--version`, and `which` for
// each server not found on PATH) unless [Options.SkipMise] is set, and
// never touches the network.
//
// It is what `servers` and `doctor` want, since they report on
// everything. A command that needs one server should call
// [Resolution.ProbeServer] instead and not spend a subprocess per
// server it will not run.
func (r *Resolution) Probe(ctx context.Context, opts Options) {
	r.ensureMise(ctx, opts)
	for _, s := range r.Servers {
		s.Binary = probeBinary(ctx, s.Def, opts, r.Mise)
	}
	r.Probed = true
}

// ProbeServer looks for one server's executable, leaving the others
// unprobed. It is the hot path: routing has already picked a server, and
// the question is only whether it can be run.
//
// [Resolution.Require] refuses on a server that has not been probed, so
// a caller that probes one server must ask about that one.
func (r *Resolution) ProbeServer(ctx context.Context, name string, opts Options) (*Resolved, error) {
	resolved, ok := r.Server(name)
	if !ok {
		return nil, &NoSuchServerError{Name: name, Known: r.Names()}
	}
	r.ensureMise(ctx, opts)
	resolved.Binary = probeBinary(ctx, resolved.Def, opts, r.Mise)
	return resolved, nil
}

// ensureMise detects the installer once per resolution: `servers` and
// `doctor` probe every server, and asking mise its version six times
// would be six subprocesses for one answer.
func (r *Resolution) ensureMise(ctx context.Context, opts Options) {
	if r.miseChecked {
		return
	}
	r.Mise = DetectMise(ctx, opts)
	r.miseChecked = true
}

// accumulator is one server under construction while the layers are
// folded together.
type accumulator struct {
	def       *ServerDef
	overrides []Override // weakest first, reversed at the end
	origins   map[Layer]Origin
}

// load does the work of [Load] without failing on problems, which is
// what [Doctor] needs.
func load(opts Options) *Resolution {
	res := &Resolution{Offline: opts.offline(), byName: map[string]*Resolved{}}
	accs := map[string]*accumulator{}

	// Weakest layer first, so that each layer folds onto what the
	// weaker ones established.
	statuses := []LayerStatus{
		res.loadBuiltins(opts, accs),
		res.loadUser(opts, accs),
		res.loadWorkspace(opts, accs),
	}
	slices.Reverse(statuses)
	res.Layers = statuses

	names := make([]string, 0, len(accs))
	for name := range accs {
		names = append(names, name)
	}
	slices.Sort(names)

	for _, name := range names {
		acc := accs[name]
		overrides := slices.Clone(acc.overrides)
		slices.Reverse(overrides) // strongest first
		if err := acc.def.Validate(); err != nil {
			res.problem(overrides[0].Origin, incompleteError(err, overrides))
			continue
		}
		resolved := &Resolved{Def: acc.def, Origin: overrides[0].Origin, Overrides: overrides}
		res.Servers = append(res.Servers, resolved)
		res.byName[name] = resolved
	}
	return res
}

// incompleteError explains a definition that does not validate. The
// common cause is an override for a server no weaker layer defines, in
// which case saying so is more useful than the bare missing key.
func incompleteError(err error, overrides []Override) error {
	for _, o := range overrides {
		if o.Whole {
			return err
		}
	}
	return fmt.Errorf("%w; this file overrides a server no other layer defines, so it must describe it completely", err)
}

func (r *Resolution) problem(origin Origin, err error) {
	cfgErr := &ConfigError{Origin: origin, Err: err}
	r.Problems = append(r.Problems, Problem{Origin: origin, Err: cfgErr, Message: cfgErr.Error()})
}

// loadBuiltins folds in the generated defaults, PLAN §6 item 3.
func (r *Resolution) loadBuiltins(opts Options, accs map[string]*accumulator) LayerStatus {
	status := LayerStatus{Layer: LayerBuiltin, Exists: true}
	if opts.SkipBuiltins {
		status.Exists = false
		status.Skipped = "disabled by the caller"
		return status
	}
	origin := Origin{Layer: LayerBuiltin}
	for _, def := range Builtins() {
		if err := def.Validate(); err != nil {
			// A broken built-in is a bug in the generator, not in the
			// user's setup; it is reported the same way so that it
			// cannot hide.
			r.problem(origin, err)
			continue
		}
		r.fold(accs, origin, NewFragment(def), true)
		status.Servers = append(status.Servers, def.Name)
	}
	slices.Sort(status.Servers)
	return status
}

// loadUser folds in $XDG_CONFIG_HOME/lightspeed/servers.d/*.toml,
// PLAN §6 item 2.
func (r *Resolution) loadUser(opts Options, accs map[string]*accumulator) LayerStatus {
	status := LayerStatus{Layer: LayerUser}
	if opts.SkipUser {
		status.Skipped = "disabled by the caller"
		return status
	}
	dir, why := opts.UserConfigDir()
	if dir == "" {
		status.Skipped = why
		return status
	}
	status.Path = filepath.Join(dir, ServersDir)
	status.Exists = dirExists(status.Path)

	files, err := tomlFilesIn(status.Path)
	if err != nil {
		r.problem(Origin{Layer: LayerUser, File: status.Path}, err)
		return status
	}
	for _, file := range files {
		names := r.loadFile(accs, Origin{Layer: LayerUser, File: file})
		status.Files = append(status.Files, file)
		status.Servers = append(status.Servers, names...)
	}
	slices.Sort(status.Servers)
	status.Servers = slices.Compact(status.Servers)
	return status
}

// loadWorkspace folds in the workspace's own .lightspeed.toml, PLAN §6
// item 1 and the strongest layer.
func (r *Resolution) loadWorkspace(opts Options, accs map[string]*accumulator) LayerStatus {
	status := LayerStatus{Layer: LayerWorkspace}
	if opts.WorkspaceRoot == "" {
		status.Skipped = "no workspace root given"
		return status
	}
	status.Path = filepath.Join(opts.WorkspaceRoot, WorkspaceFile)
	status.Exists = fileExists(status.Path)
	if !status.Exists {
		return status
	}
	origin := Origin{Layer: LayerWorkspace, File: status.Path}
	status.Files = []string{status.Path}
	status.Servers = r.loadFile(accs, origin)
	slices.Sort(status.Servers)
	return status
}

// loadFile reads and folds one configuration file, returning the names
// it mentioned.
func (r *Resolution) loadFile(accs map[string]*accumulator, origin Origin) []string {
	data, err := os.ReadFile(origin.File)
	if err != nil {
		r.problem(origin, err)
		return nil
	}
	frags, err := ParseFragments(data)
	if err != nil {
		r.problem(origin, err)
		return nil
	}
	names := make([]string, 0, len(frags))
	for _, frag := range frags {
		if frag.Empty() {
			r.problem(origin, fmt.Errorf("server %q: the file mentions this server and then sets nothing; remove the table or say what to change", frag.Name))
			continue
		}
		if conflict := r.fold(accs, origin, frag, false); conflict != nil {
			r.Problems = append(r.Problems, Problem{Origin: origin, Err: conflict, Message: conflict.Error()})
			continue
		}
		names = append(names, frag.Name)
	}
	return names
}

// fold applies one fragment onto the accumulator for its server. Two
// definitions of one server inside the same layer are refused: across
// layers an override is the design, within a layer there is no rule
// that would pick a winner.
func (r *Resolution) fold(accs map[string]*accumulator, origin Origin, frag *Fragment, whole bool) *ConflictError {
	acc, ok := accs[frag.Name]
	if !ok {
		acc = &accumulator{origins: map[Layer]Origin{}}
		accs[frag.Name] = acc
	}
	if previous, clash := acc.origins[origin.Layer]; clash && previous.File != origin.File {
		return &ConflictError{Name: frag.Name, First: previous, Second: origin}
	}
	acc.origins[origin.Layer] = origin
	acc.def = frag.ApplyTo(acc.def)
	acc.overrides = append(acc.overrides, Override{Origin: origin, Keys: frag.Keys(), Whole: whole})
	return nil
}

// FindWorkspaceRoot walks up from start looking for a directory holding
// [WorkspaceFile], and returns it. It is how a CLI turns "the file the
// user named" into [Options.WorkspaceRoot] when no root has been
// resolved yet; the router's own root markers answer the different
// question of where a *server* should run.
//
// It returns an empty string and no error when there is no such file
// anywhere up to the filesystem root: having no workspace config is the
// normal, zero-config case.
func FindWorkspaceRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	if fi, err := os.Stat(dir); err == nil && !fi.IsDir() {
		dir = filepath.Dir(dir)
	}
	for {
		if fileExists(filepath.Join(dir, WorkspaceFile)) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", nil
		}
		dir = parent
	}
}

// assert at compile time that the error helpers keep satisfying the
// contract internal/render relies on (docs/DECISIONS.md D7).
var (
	_ interface{ ExitCode() int } = (*ConfigError)(nil)
	_ interface{ ExitCode() int } = (*ConflictError)(nil)
	_ interface{ ExitCode() int } = (*NotInstalledError)(nil)
	_ interface{ ExitCode() int } = (*OfflineError)(nil)
	_ interface{ ExitCode() int } = (*MiseUnavailableError)(nil)
	_ interface{ ExitCode() int } = (*NoSuchServerError)(nil)
	_ interface{ ExitCode() int } = (*InstallFailedError)(nil)
	_ error                       = (*ConfigError)(nil)
)
