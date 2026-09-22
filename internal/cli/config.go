package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
	"github.com/tanevanwifferen/Lightspeed/internal/serverdef"
)

// A workspaceConfig is the server definitions of one workspace after the
// layering of PLAN §6: .lightspeed.toml in the workspace root over the
// user's servers.d over the generated defaults, with PATH sniffing (and
// mise, when it is already installed) deciding where each executable is.
//
// It is what internal/cli resolves a path against, what the pool in the
// daemon — or in this process, with --no-daemon — routes with, and what
// `servers`, `install` and `doctor` report on. There is exactly one of
// them per workspace root per command, so the client and the daemon
// cannot be looking at different tables unless the files changed between
// their two loads, which the config id detects (D17).
type workspaceConfig struct {
	// root is the workspace root the workspace layer was read from.
	root string
	// opts are the serverdef options it was loaded with, kept so that
	// probing and installing use the same environment.
	opts serverdef.Options
	// res is the layered load. Executables are looked for lazily, per
	// server, by [workspaceConfig.probe].
	res    *serverdef.Resolution
	router *router.Router
	// id identifies the effective definitions; see [configID].
	id string

	// mu serialises probing: serverdef fills a Resolved in place, and a
	// pool may launch two servers at once.
	mu sync.Mutex
}

// configCache holds the workspace configs one invocation has loaded.
type configCache struct {
	mu     sync.Mutex
	byRoot map[string]*workspaceConfig
}

// configCache returns the invocation's cache, making it on first use. A
// batch hands its own to the queries it runs, so that a hundred queries
// against one workspace read its configuration once.
func (e *env) configCache() *configCache {
	if e.configs == nil {
		e.configs = &configCache{byRoot: map[string]*workspaceConfig{}}
	}
	return e.configs
}

// serverdefOptions is the one place lightspeed's own settings become
// serverdef's: the workspace to read, and the --offline flag, which
// serverdef ORs with $LIGHTSPEED_OFFLINE so that an environment that says
// offline cannot be argued out of it by a flag.
func (e *env) serverdefOptions(root string) serverdef.Options {
	return serverdef.Options{WorkspaceRoot: root, Offline: e.offline}
}

// workspaceConfig loads the definitions that apply to path: those of the
// workspace root that path belongs to, which is the same root that keys
// its daemon (daemon.Workspace), so the two always read the same
// .lightspeed.toml.
func (e *env) workspaceConfig(path string) (*workspaceConfig, error) {
	root, err := daemon.Workspace(path)
	if err != nil {
		return nil, render.Errorf(render.CodeUsage, "resolving %s: %v", path, err)
	}
	return e.configForRoot(root)
}

// configForRoot loads the definitions of a workspace root, once.
func (e *env) configForRoot(root string) (*workspaceConfig, error) {
	cache := e.configCache()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	if cfg, ok := cache.byRoot[root]; ok {
		return cfg, nil
	}
	cfg, err := loadWorkspaceConfig(e.serverdefOptions(root))
	if err != nil {
		return nil, err
	}
	cache.byRoot[root] = cfg
	return cfg, nil
}

// loadWorkspaceConfig reads the layers. A file that cannot be used is an
// error, not a skipped file: an override that silently does nothing is
// the failure PLAN §6 exists to prevent. Commands that exist to explain
// a broken configuration (`servers`, `doctor`) do not come through here.
func loadWorkspaceConfig(opts serverdef.Options) (*workspaceConfig, error) {
	res, err := serverdef.Load(opts)
	if err != nil {
		return nil, serverdefFailure(err)
	}
	defs := res.Definitions()
	r, err := router.New(defs...)
	if err != nil {
		// A definition that loads and validates but that the router
		// rejects has a glob it cannot compile: the user's, since the
		// generated table is tested against the router.
		return nil, render.Errorf(render.CodeInvalidConfig, "the server definitions are unusable: %v", err)
	}
	id, err := configID(defs)
	if err != nil {
		return nil, render.Errorf(render.CodeInternal, "identifying the server definitions: %v", err)
	}
	return &workspaceConfig{root: opts.WorkspaceRoot, opts: opts, res: res, router: r, id: id}, nil
}

// configID identifies a set of effective definitions: a digest of their
// canonical JSON (encoding/json sorts map keys, and the set is ordered by
// name). What it deliberately leaves out is anything about the machine —
// where an executable is, whether mise is present — because the daemon's
// staleness (D17) is about the *configuration* having changed under it,
// and a daemon's environment is documented to be that of the command that
// started it (D14).
func configID(defs []*serverdef.ServerDef) (string, error) {
	data, err := json.Marshal(defs)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8]), nil
}

// probe looks for one server's executable. It leaves the other servers
// alone: a command needs to know about the server it is about to run, not
// spend a subprocess per definition asking mise about the rest.
func (c *workspaceConfig) probe(ctx context.Context, name string) (*serverdef.Resolved, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rs, err := c.res.ProbeServer(ctx, name, c.opts)
	if err != nil {
		return nil, serverdefFailure(err)
	}
	return rs, nil
}

// require is exit code 3 with the exact command that would fix it, or
// nil when the server can be run. It runs before a daemon is contacted,
// so that "not installed" always reads the same however the server would
// have been started — and so that it is a client-side answer that costs
// no daemon spawn.
func (c *workspaceConfig) require(ctx context.Context, name string) error {
	rs, err := c.probe(ctx, name)
	if err != nil {
		return err
	}
	if rs.Installed() {
		return nil
	}
	problem := ""
	if rs.Binary.Path != "" {
		problem = rs.Binary.Path + " " + rs.Binary.Problem
	}
	return notInstalledError(rs.Def, rs.Binary.Name, problem, rs.Binary.Fix, c.res.Offline)
}

// executable is where the server's binary was found, for the launcher. It
// is empty when the server is not runnable, in which case the launcher
// leaves the command alone and the launch fails the way it always did.
func (c *workspaceConfig) executable(ctx context.Context, name string) string {
	rs, err := c.probe(ctx, name)
	if err != nil || !rs.Installed() {
		return ""
	}
	return rs.Binary.Path
}

// poolOptions configures the server pool that the daemon — or the
// in-process mode — owns: this workspace's definitions, a launcher that
// starts each server from the executable that was found for it, and one
// client-capability set that is the union of what every command can
// honour (D15).
func (c *workspaceConfig) poolOptions(serverStderr io.Writer) daemon.PoolOptions {
	return daemon.PoolOptions{
		Router:          c.router,
		Launcher:        c.launcher(serverStderr),
		Capabilities:    sessionCapabilities(),
		ShutdownTimeout: shutdownGrace,
		Stderr:          serverStderr,
		ConfigID:        c.id,
	}
}

// launcher starts a language server the way `daemon.ExecLauncher` does,
// except that command[0] is the executable the probe found rather than
// whatever the launching process's PATH says. That is what lets a server
// that is only reachable through `mise which` run at all, and it makes
// the process that answers the one `servers` reported. A definition whose
// command is already a path is untouched.
func (c *workspaceConfig) launcher(stderr io.Writer) daemon.Launcher {
	launch := daemon.ExecLauncher(stderr)
	return func(ctx context.Context, def *serverdef.ServerDef, root string) (*daemon.Instance, error) {
		if path := c.executable(ctx, def.Name); path != "" && len(def.Server.Command) > 0 && def.Server.Command[0] != path {
			def = def.Clone()
			def.Server.Command[0] = path
		}
		return launch(ctx, def, root)
	}
}

// serverdefFailure is an error from internal/serverdef as the renderer
// wants to see it: with the machine code and the exit status serverdef
// decided, both of which the renderer would otherwise have to guess. The
// codes are declared in internal/render, whose taxonomy this is.
func serverdefFailure(err error) error {
	var coded interface {
		error
		Code() string
		ExitCode() int
	}
	if errors.As(err, &coded) {
		return configFailure{err: err, code: render.Code(coded.Code()), exit: coded.ExitCode()}
	}
	return render.Errorf(render.CodeInternal, "%v", err)
}

// configFailure carries serverdef's classification through render's
// Coder and exit-code interfaces, unchanged. TestServerdefCodesAreRenderCodes
// keeps its codes in step with render's table.
type configFailure struct {
	err  error
	code render.Code
	exit int
}

func (f configFailure) Error() string          { return f.err.Error() }
func (f configFailure) Unwrap() error          { return f.err }
func (f configFailure) ErrorCode() render.Code { return f.code }
func (f configFailure) ExitCode() int          { return f.exit }
