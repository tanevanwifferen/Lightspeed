package serverdef

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// isMiseShim reports whether path is one of mise's shims: a file in a
// directory called `shims` that is mise's — under a `mise` directory, under
// $MISE_DATA_DIR, or a link to the mise executable.
//
// A shim is an executable that exists and can be run, and is still no
// evidence that the tool behind it can: it asks mise which version applies
// to the directory it is run in, and with none active it prints
// `mise ERROR No version is set for shim` and exits. The file test that
// [executable] makes passes, which is how such a server was once reported
// as installed while every query failed.
func isMiseShim(path string, opts Options) bool {
	dir := filepath.Dir(path)
	if filepath.Base(dir) != "shims" {
		return false
	}
	if data := opts.getenv("MISE_DATA_DIR"); data != "" && filepath.Join(data, "shims") == dir {
		return true
	}
	if target, err := os.Readlink(path); err == nil && filepath.Base(target) == MiseName {
		return true
	}
	for _, part := range strings.Split(filepath.ToSlash(dir), "/") {
		if part == MiseName {
			return true
		}
	}
	return false
}

// verifyShim decides what a mise shim on PATH really is, for the
// workspace lightspeed will run the server in. It asks mise — the shim's
// owner, and the only thing that knows — `mise which <binary>` from the
// workspace directory, which answers exactly what the shim would do there
// and runs no tool.
//
// The three outcomes are the ones PLAN §6 leaves room for. The shim
// resolves: the server is what it always was. It does not, but mise has a
// version of the tool installed: that installed binary is launched, and
// the note says so, because a shell would still fail. Nothing is
// installed, or mise cannot be asked: the server is not usable, with the
// command that fixes it. Nothing here touches the network; installing a
// version is `mise use`, which only the user runs.
func verifyShim(ctx context.Context, def *ServerDef, shim Binary, opts Options, mise MiseStatus) Binary {
	if !mise.Available || mise.Path == "" {
		// Nobody to ask. The start probe of doctor and servers still
		// runs the shim and sees what it says.
		return shim
	}
	name := shim.Name
	args := []string{"which", name}
	if dir := opts.WorkspaceRoot; dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	stdout, stderr, err := opts.runner()(ctx, miseProbeEnv, mise.Path, args...)
	if err == nil {
		if p := firstLine(stdout); p != "" {
			if ok, _ := executable(p); ok {
				return shim
			}
		}
	}
	reason := "no version of " + name + " is active"
	if dir := opts.WorkspaceRoot; dir != "" {
		reason += " in " + dir
	}
	if line := firstLine(stderr); line != "" {
		reason += " (" + line + ")"
	}
	unusable := func(fix []string, why string) Binary {
		b := shim
		b.Source = BinaryUnusable
		b.Runnable = false
		b.Problem = "is a mise shim and " + reason
		if why != "" {
			b.Problem += "; " + why
		}
		b.Fix = fix
		return b
	}

	spec := def.Install.Mise
	if spec == "" {
		return unusable(nil, "")
	}
	tool, pinned := splitSpec(spec)
	version, ok := installedVersion(ctx, tool, pinned, opts, mise)
	if !ok {
		return unusable(installCommandOf(def), "mise has no version of "+tool+" installed")
	}
	use := []string{MiseName, "use", "-g", tool + "@" + version}
	which, _, err := opts.runner()(ctx, miseProbeEnv, mise.Path, "which", "--tool", tool+"@"+version, name)
	actual := firstLine(which)
	if err != nil || actual == "" {
		return unusable(use, fmt.Sprintf("mise has %s@%s but could not say where its %s is", tool, version, name))
	}
	if ok, problem := executable(actual); !ok {
		return unusable(use, fmt.Sprintf("mise's %s@%s is at %s but %s", tool, version, actual, problem))
	}
	return Binary{
		Name: name, Path: actual, Source: BinaryMise, Runnable: true, Probed: true, Fix: use,
		Note: fmt.Sprintf("the mise shim %s has no active version, so this is the installed %s@%s; `%s` makes it the default",
			shim.Path, tool, version, strings.Join(use, " ")),
	}
}

// installCommandOf is the argv that installs def, nil without a spec.
func installCommandOf(def *ServerDef) []string {
	if def.Install.Mise == "" {
		return nil
	}
	return []string{MiseName, "use", "-g", def.Install.Mise}
}

// miseInstall is the part of one `mise ls --json` entry this package reads.
type miseInstall struct {
	Version   string `json:"version"`
	Installed *bool  `json:"installed"`
}

// installedVersion asks mise (offline) which installed versions of tool it
// has and picks one: the version the definition pins if that is installed,
// otherwise the highest.
func installedVersion(ctx context.Context, tool, pinned string, opts Options, mise MiseStatus) (string, bool) {
	stdout, _, err := opts.runner()(ctx, miseProbeEnv, mise.Path, "ls", "--json", "--installed", tool)
	if err != nil {
		return "", false
	}
	var entries []miseInstall
	if json.Unmarshal([]byte(stdout), &entries) != nil {
		// `mise ls --json` without a filter is an object keyed by tool.
		var byTool map[string][]miseInstall
		if json.Unmarshal([]byte(stdout), &byTool) != nil {
			return "", false
		}
		entries = byTool[tool]
	}
	var versions []string
	for _, e := range entries {
		if e.Version != "" && (e.Installed == nil || *e.Installed) {
			versions = append(versions, e.Version)
		}
	}
	if len(versions) == 0 {
		return "", false
	}
	if pinned != "" {
		for _, v := range versions {
			if strings.TrimPrefix(v, "v") == strings.TrimPrefix(pinned, "v") {
				return v, true
			}
		}
	}
	best := versions[0]
	for _, v := range versions[1:] {
		if versionLess(best, v) {
			best = v
		}
	}
	return best, true
}

// versionLess orders two dotted versions by their numeric parts, falling
// back to string order for the rest. It only has to choose sensibly among
// the few versions one tool has installed.
func versionLess(a, b string) bool {
	pa, pb := strings.Split(strings.TrimPrefix(a, "v"), "."), strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < len(pa) && i < len(pb); i++ {
		na, ea := strconv.Atoi(pa[i])
		nb, eb := strconv.Atoi(pb[i])
		switch {
		case ea == nil && eb == nil && na != nb:
			return na < nb
		case (ea != nil || eb != nil) && pa[i] != pb[i]:
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}

// A StartRunner runs an executable once, in dir, to see whether it starts.
// A non-nil err means it could not be started at all (no such file, not
// executable, wrong format); a process that ran and exited non-zero is
// reported through exit and is not an error. It is [Runner]'s sibling and
// exists for the same reason: the one seam through which the start probe
// touches a process, so that tests stay hermetic.
type StartRunner func(ctx context.Context, dir string, env []string, path string, args ...string) (output string, exit int, err error)

// startProbeTimeout bounds one start probe. A server that does not exit on
// `--version` (some read stdin, some wait for a client) has started, which
// is all the probe asks; it is killed at this deadline rather than waited
// for.
var startProbeTimeout = 2 * time.Second

func (o Options) starter() StartRunner {
	if o.Start != nil {
		return o.Start
	}
	return execStart
}

// execStart is the production StartRunner: stdin is empty, output is
// bounded, and the environment is [miseProbeEnv]'s, so that a shim that
// would install what it is missing (mise's not_found_auto_install) does
// not.
func execStart(ctx context.Context, dir string, env []string, path string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{w: &out, max: 4096}
	cmd.Stderr = cmd.Stdout
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0, nil
	case errors.As(err, &exitErr):
		return out.String(), exitErr.ExitCode(), nil
	default:
		return out.String(), -1, err
	}
}

// limitedWriter keeps the first max bytes and discards the rest, reporting
// success so that the child never blocks on a full pipe.
type limitedWriter struct {
	w   *bytes.Buffer
	max int
	mu  sync.Mutex
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if room := l.max - l.w.Len(); room > 0 {
		l.w.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

// brokenInstall are the words a process says when it could not start even
// though its file is there: a version manager's shim with nothing behind
// it, and a binary whose dynamic libraries are missing.
var brokenInstall = regexp.MustCompile(`(?i)\bmise ERROR\b|error while loading shared libraries|no version is set for shim|cannot execute binary file`)

// StartProbe tries to start every server's executable, for `servers` and
// `doctor`: it runs `<binary> --version` in the workspace directory,
// bounded by two seconds, with an empty stdin and mise told not to
// download. It is what PLAN §6's "never report ok for what cannot run"
// needs beyond the file checks of [probeBinary], which pass for a shim
// pointing at nothing, a binary with a missing library and a wrapper
// script that fails.
//
// What counts as failure is deliberately narrow, because `--version` is
// not a convention every server follows (gopls says `flag provided but not
// defined` and exits 2; pyright's launcher wants a client). A process that
// ran is a server that can start, whatever it exited with. Failure is:
// the exec itself failing, exit 126 or 127 (the shell's "cannot execute" and
// "not found", from a wrapper), or output with the words of [brokenInstall].
// The probe records "ok", "started" (ran, non-zero, nothing wrong said) or
// "failed" in [Binary.StartProbe]; a failure makes the binary not runnable.
//
// It is not part of [Resolution.Probe] or [Resolution.ProbeServer]: those
// run before every query, and the query starting the server is itself the
// probe there (a spawn that fails says why: the process's status and
// stderr are in the error).
func (r *Resolution) StartProbe(ctx context.Context, opts Options) {
	var wg sync.WaitGroup
	for _, s := range r.Servers {
		if !s.Binary.Runnable {
			continue
		}
		wg.Add(1)
		go func(s *Resolved) {
			defer wg.Done()
			s.Binary = startProbe(ctx, s.Binary, opts)
		}(s)
	}
	wg.Wait()
}

func startProbe(ctx context.Context, b Binary, opts Options) Binary {
	ctx, cancel := context.WithTimeout(ctx, startProbeTimeout)
	defer cancel()
	out, exit, err := opts.starter()(ctx, opts.WorkspaceRoot, miseProbeEnv, b.Path, "--version")
	line := firstLine(out)
	switch {
	case err != nil:
		return failStart(b, err.Error())
	case exit == 0:
		b.StartProbe = "ok"
	case ctx.Err() != nil:
		// Killed at the deadline: it was running, which is all we wanted.
		b.StartProbe = "started"
	case exit == 126 || exit == 127 || brokenInstall.MatchString(out):
		if line == "" {
			line = fmt.Sprintf("exit status %d", exit)
		}
		return failStart(b, line)
	default:
		b.StartProbe = "started"
	}
	return b
}

func failStart(b Binary, why string) Binary {
	b.Runnable = false
	b.Source = BinaryUnusable
	b.StartProbe = "failed"
	b.Problem = "cannot be started: " + why
	return b
}
