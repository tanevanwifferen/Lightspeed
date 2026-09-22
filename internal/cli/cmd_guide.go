package cli

import (
	"flag"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// guideCommands are `guide` and `version`: the two commands that describe this
// binary to whoever is about to use it. Neither needs a language server or a
// daemon.
func guideCommands() []*command {
	return []*command{
		{
			Name:    "guide",
			Args:    "[--compact]",
			Summary: "print the agent policy for this build, generated from the command table (--format claude-md for a CLAUDE.md section)",
			Run:     guideCommand,
			MCP: oneTool("guide", "How to use these tools: which for what, naming symbols, apply rules, exit codes. Generated from this server's own tool table.",
				[]param{{Flag: "compact", Type: typeBool, Desc: "The short form (the one in the server instructions) instead of the full guide."}},
				commonParams()),
		},
		{
			Name:    "version",
			Args:    "",
			Summary: "print the version, build identity, guide version and Go toolchain of this executable",
			Run:     versionCommand,
			MCP: oneTool("version", "Version and build identity of this lightspeed, and the version of its guide.",
				commonParams()),
		},
	}
}

// The formats `guide` prints besides json.
const (
	guideFormatMarkdown = "markdown"
	guideFormatClaudeMD = "claude-md"
)

// guideCommand implements `lightspeed guide`.
//
// Unlike the query commands it does not default to JSON off a terminal: the
// guide is prose, and `lightspeed guide > file` should be markdown. json is the
// envelope with the text and the groups; claude-md is the section with markers.
func guideCommand(e *env, c *command, args []string) int {
	var compact bool
	common, _, err := parseFlagsRange(e, c, args, 0, 0, func(fs *flag.FlagSet) {
		fs.BoolVar(&compact, "compact", false, "the short form used as the MCP server's instructions")
	})
	if err != nil {
		return e.flagError(err)
	}
	// Over MCP the guide names what the agent can call: tools and their
	// parameters, not commands and flags.
	style := guideText
	if e.overMCP {
		style = guideMCP
	}
	format := strings.ToLower(common.format)
	switch format {
	case "", "text", guideFormatMarkdown:
		e.stdout.Write([]byte(buildGuide(compact, style)))
		e.stdout.Write([]byte("\n"))
		return ExitOK
	case guideFormatClaudeMD:
		if compact {
			return e.usagef("guide: --compact has no CLAUDE.md form; the compact guide is the MCP server's instructions")
		}
		e.stdout.Write([]byte(buildGuide(false, guideClaudeMD)))
		return ExitOK
	case string(render.FormatJSON):
		return e.writeData(common, guidePayload(compact, style), nil, ExitOK)
	}
	return e.fail(render.Errorf(render.CodeUnsupportedFormat,
		"format %q has no meaning for guide (want one of json, text, markdown, claude-md)", common.format))
}

// versionCommand implements `lightspeed version`.
func versionCommand(e *env, c *command, args []string) int {
	common, _, err := parseFlagsRange(e, c, args, 0, 0, nil)
	if err != nil {
		return e.flagError(err)
	}
	format, err := managementFormat(common, "version", e.stdout)
	if err != nil {
		return e.fail(err)
	}
	info := currentBuildInfo()
	if format == render.FormatText {
		writeVersionText(e.stdout, info)
		return ExitOK
	}
	return e.writeData(common, info, nil, ExitOK)
}
