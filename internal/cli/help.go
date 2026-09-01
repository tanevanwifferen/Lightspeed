package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
	"github.com/tanevanwifferen/Lightspeed/internal/router"
)

// commandInfo is one entry of the capability-derived command surface,
// as it appears in the JSON envelope.
type commandInfo struct {
	Name string `json:"name"`
	// Method is the LSP request behind the command, "" for the
	// commands that need no server.
	Method string `json:"method,omitempty"`
	// Capability is the InitializeResult path the server must
	// advertise, "" for an unguarded command.
	Capability string `json:"capability,omitempty"`
	Summary    string `json:"summary"`
}

// surfaceData is the payload of `lightspeed help <target>`.
type surfaceData struct {
	Server        string        `json:"server"`
	ServerName    string        `json:"server_name,omitempty"`
	ServerVersion string        `json:"server_version,omitempty"`
	Root          string        `json:"root"`
	Available     []commandInfo `json:"available"`
	Unavailable   []commandInfo `json:"unavailable"`
}

// helpCommand implements `lightspeed help [<file>|<dir>]`.
//
// Without a target it prints the static surface: every command and the
// capability it needs. With one it starts the server that handles that
// target and prints what the server actually advertises — PLAN §4's
// requirement that the command surface be derived from capabilities at
// runtime, so `--help` cannot promise a command that would fail.
func helpCommand(e *env, c *command, args []string) int {
	fs := flag.NewFlagSet("help", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "usage: lightspeed help [flags] %s\n\n%s\n\nflags:\n", c.Args, c.Summary)
		fs.PrintDefaults()
	}
	common := &commonFlags{}
	common.register(fs)
	language := fs.String("language", "", "language id of the target, when nothing in it identifies one")

	positional, flagArgs := splitPositional(fs, args)
	if err := fs.Parse(flagArgs); err != nil {
		return e.flagError(render.Errorf(render.CodeUsage, "help: %v", err))
	}
	positional = append(positional, fs.Args()...)
	if err := common.validate(); err != nil {
		return e.fail(err)
	}
	switch len(positional) {
	case 0:
		writeUsage(e.stderr)
		return render.ExitOK
	case 1:
	default:
		fs.Usage()
		return e.usagef("help: expected at most one target, got %d", len(positional))
	}

	format, err := common.resolveFormat(e.stdout)
	if err != nil {
		return e.fail(err)
	}
	if err := checkResultsFormat(format); err != nil {
		return e.fail(err)
	}

	match, err := resolveHelpTarget(positional[0], *language, common.server)
	if err != nil {
		return e.fail(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), common.timeout)
	defer cancel()
	s, err := startSession(ctx, e, match, common.gateOptions())
	if err != nil {
		return e.fail(err)
	}
	defer s.close()

	caps := s.lsp.Capabilities()
	available, unavailable := partitionByCapabilities(caps)

	fmt.Fprintf(e.stderr, "%s (%s) in %s answers:\n", match.Server.Name, describeServer(s), match.Root)
	writeCommandTable(e.stderr, available, false)
	if len(unavailable) > 0 {
		fmt.Fprintln(e.stderr, "\nnot available from this server:")
		writeCommandTable(e.stderr, unavailable, true)
	}

	data := surfaceData{
		Server:        match.Server.Name,
		ServerName:    caps.ServerName(),
		ServerVersion: caps.ServerVersion(),
		Root:          match.Root,
		Available:     describeCommands(available),
		Unavailable:   describeCommands(unavailable),
	}
	if format == render.FormatText {
		for _, info := range data.Available {
			fmt.Fprintf(e.stdout, "%s\n", info.Name)
		}
		return render.ExitOK
	}
	if err := render.OK(e.stdout, data); err != nil {
		return e.fail(err)
	}
	return render.ExitOK
}

// resolveHelpTarget resolves a file or a directory to a server, so
// that `help` accepts either.
func resolveHelpTarget(target, language, serverName string) (router.Match, error) {
	abs, err := filepath.Abs(target)
	if err != nil {
		return router.Match{}, render.Errorf(render.CodeUsage, "resolving %s: %v", target, err)
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		return resolveWorkspace(abs, language, serverName)
	}
	return resolveTarget(abs, language, serverName)
}

// describeServer names the server as it introduced itself, which is
// not always the name of the definition that started it.
func describeServer(s *session) string {
	name := s.lsp.ServerName()
	if name == "" {
		return "unnamed server"
	}
	if version := s.lsp.Capabilities().ServerVersion(); version != "" {
		return name + " " + version
	}
	return name
}

func describeCommands(cmds []*command) []commandInfo {
	out := make([]commandInfo, 0, len(cmds))
	for _, c := range cmds {
		capability, _ := c.Capability()
		out = append(out, commandInfo{
			Name:       c.Name,
			Method:     c.Method,
			Capability: capability,
			Summary:    c.Summary,
		})
	}
	return out
}
