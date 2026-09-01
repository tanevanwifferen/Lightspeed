package cli

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/tanevanwifferen/Lightspeed/internal/client"
	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// gateSlack is how much longer than --timeout the request context is
// given. The readiness gate of PLAN §5.2 owns the deadline that
// matters: when a workspace never becomes ready the answer must be
// exit 5, and a context that expired first would turn that into exit 4
// — "we don't know" instead of "it is still indexing". The slack keeps
// the gate's clock the one that runs out.
const gateSlack = 2 * time.Second

// commonFlags are the flags every query command shares: the output
// contract of PLAN §4 plus the knobs that decide when an answer may be
// believed.
type commonFlags struct {
	format  string
	context int
	limit   int
	indent  bool
	timeout time.Duration
	settle  time.Duration
	server  string
}

// register adds the common flags to fs.
func (f *commonFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.format, "format", "", "output format: json, text or diff (default json, text on a terminal, diff for an edit preview)")
	fs.IntVar(&f.context, "context", 0, "lines of source to print around each result")
	fs.IntVar(&f.limit, "limit", 0, "maximum number of results (0 = no limit); truncation is always reported")
	fs.BoolVar(&f.indent, "indent", false, "pretty-print JSON output")
	fs.DurationVar(&f.timeout, "timeout", client.DefaultTimeout, "how long to wait for the workspace to become ready")
	fs.DurationVar(&f.settle, "settle", client.DefaultSettle, "how long a result must be unchanged before it is believed")
	fs.StringVar(&f.server, "server", "", "name of the server to use when several claim the file")
}

// renderOptions maps the flags onto the renderer's options, folding in
// the readiness warnings the gate attached to the answer.
func (f *commonFlags) renderOptions(warnings []string) render.Options {
	return render.Options{
		Context:  f.context,
		Limit:    f.limit,
		Indent:   f.indent,
		Warnings: warnings,
	}
}

// gateOptions maps the flags onto the readiness gate's options.
func (f *commonFlags) gateOptions() client.GateOptions {
	return client.GateOptions{Timeout: f.timeout, Settle: f.settle}
}

// validate rejects flag values that are impossible rather than merely
// unusual, before a server is started. A non-positive --timeout would
// make every deadline already expired, which surfaces as a confusing
// exit 5 instead of the usage error it is.
func (f *commonFlags) validate() error {
	switch {
	case f.timeout <= 0:
		return render.Errorf(render.CodeUsage, "--timeout must be positive (got %s)", f.timeout)
	case f.settle < 0:
		return render.Errorf(render.CodeUsage, "--settle must not be negative (got %s)", f.settle)
	case f.context < 0:
		return render.Errorf(render.CodeUsage, "--context must not be negative (got %d)", f.context)
	case f.limit < 0:
		return render.Errorf(render.CodeUsage, "--limit must not be negative (got %d)", f.limit)
	}
	return nil
}

// resolveFormat validates --format against the output stream. diff is
// a valid format that no read-only command can produce; render says so
// itself when the result set is handed to it, so nothing is rejected
// here beyond an outright unknown name.
func (f *commonFlags) resolveFormat(w io.Writer) (render.Format, error) {
	return render.ResolveFormat(f.format, w)
}

// parseFlags builds a flag set for a subcommand, parses args and
// enforces the positional-argument count.
//
// Go's flag package stops at the first non-flag argument, which would
// make `lightspeed references foo.go:1:1 --limit 5` silently ignore
// the limit. Positional arguments are therefore pulled out first and
// the remainder parsed as flags, so flags may appear on either side of
// them — which is what every caller, human or agent, expects.
func parseFlags(e *env, c *command, args []string, want int, extra func(*flag.FlagSet)) (*commonFlags, []string, error) {
	return parseFlagsRange(e, c, args, want, want, extra)
}

// parseFlagsRange is parseFlags for a command that takes a variable
// number of positional arguments: `format` takes one path or twenty.
// A max of -1 means "no upper bound".
func parseFlagsRange(e *env, c *command, args []string, min, max int, extra func(*flag.FlagSet)) (*commonFlags, []string, error) {
	fs := flag.NewFlagSet(c.Name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	fs.Usage = func() {
		fmt.Fprintf(e.stderr, "usage: lightspeed %s [flags] %s\n\n%s\n\nflags:\n",
			c.Name, c.Args, c.Summary)
		fs.PrintDefaults()
	}

	common := &commonFlags{}
	common.register(fs)
	if extra != nil {
		extra(fs)
	}

	positional, flagArgs := splitPositional(fs, args)
	if err := fs.Parse(flagArgs); err != nil {
		// flag has already printed the error and the usage.
		return nil, nil, render.Errorf(render.CodeUsage, "%s: %v", c.Name, err)
	}
	positional = append(positional, fs.Args()...)

	if err := common.validate(); err != nil {
		return nil, nil, err
	}
	if len(positional) < min || (max >= 0 && len(positional) > max) {
		fs.Usage()
		return nil, nil, render.Errorf(render.CodeUsage,
			"%s: expected %s argument(s) %s, got %d", c.Name, countRange(min, max), c.Args, len(positional))
	}
	return common, positional, nil
}

// countRange describes an accepted positional-argument count for an
// error message.
func countRange(min, max int) string {
	switch {
	case min == max:
		return fmt.Sprintf("%d", min)
	case max < 0:
		return fmt.Sprintf("at least %d", min)
	default:
		return fmt.Sprintf("%d to %d", min, max)
	}
}

// splitPositional separates positional arguments from flag arguments,
// so that flags may follow positionals. A "--" terminator makes
// everything after it positional, which is how a location that starts
// with a dash is passed.
func splitPositional(fs *flag.FlagSet, args []string) (positional, flags []string) {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			return append(positional, args[i+1:]...), flags
		case strings.HasPrefix(arg, "-") && arg != "-":
			flags = append(flags, arg)
			// A flag spelled `-name value` consumes the next
			// argument; `-name=value` and booleans do not.
			if name, _, hasEq := strings.Cut(strings.TrimLeft(arg, "-"), "="); !hasEq && takesValue(fs, name) && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		default:
			positional = append(positional, arg)
		}
	}
	return positional, flags
}

// takesValue reports whether a flag needs a separate value argument.
// Boolean flags do not, and treating one as if it did would swallow a
// positional argument.
func takesValue(fs *flag.FlagSet, name string) bool {
	f := fs.Lookup(name)
	if f == nil {
		return false // unknown: let flag.Parse produce the error
	}
	boolFlag, ok := f.Value.(interface{ IsBoolFlag() bool })
	return !ok || !boolFlag.IsBoolFlag()
}
