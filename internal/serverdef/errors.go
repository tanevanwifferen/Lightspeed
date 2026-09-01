package serverdef

import (
	"errors"
	"fmt"
	"strings"
)

// Exit codes of PLAN §4, duplicated here as unexported constants so
// that this package needs no import to participate in the taxonomy.
// Errors below expose them through an ExitCode method, which
// internal/render finds by type-asserting an anonymous interface (see
// docs/DECISIONS.md D7).
const (
	exitUsage    = 2 // the invocation or the configuration is wrong
	exitNoServer = 3 // no server available, and installing one would fix it
	exitCrash    = 4 // an operation we delegated failed
)

// Sentinels, so that a caller can classify without naming a type.
var (
	// ErrInvalidConfig is behind every configuration-file failure.
	ErrInvalidConfig = errors.New("invalid server configuration")
	// ErrNotInstalled is behind every "the server binary is missing"
	// failure — PLAN §6's "nothing downloads implicitly".
	ErrNotInstalled = errors.New("language server is not installed")
	// ErrOffline is behind every refusal caused by the offline kill
	// switch.
	ErrOffline = errors.New("offline mode is active")
	// ErrMiseUnavailable is behind every failure to find or run mise.
	ErrMiseUnavailable = errors.New("mise is not available")
	// ErrNoSuchServer is behind a request naming a server that no
	// configuration layer knows.
	ErrNoSuchServer = errors.New("no such server")
	// ErrInstallFailed is behind a mise invocation that ran and failed.
	ErrInstallFailed = errors.New("install failed")
)

// A ConfigError reports one unusable configuration file, naming the
// file and the layer it belongs to. A broken config is never skipped:
// a user override that does nothing because of a typo is exactly the
// silent shadowing PLAN §6 forbids.
type ConfigError struct {
	// Origin is the file (or the built-in table) at fault.
	Origin Origin
	// Err is the underlying parse or validation failure.
	Err error
}

func (e *ConfigError) Error() string {
	return fmt.Sprintf("%s: %v", e.Origin, e.Err)
}

func (e *ConfigError) Unwrap() error { return e.Err }

// Is makes errors.Is(err, ErrInvalidConfig) true for any config error
// while keeping the wrapped cause reachable.
func (e *ConfigError) Is(target error) bool { return target == ErrInvalidConfig }

// ExitCode reports PLAN §4's usage code: nothing was asked of a server,
// the inputs are wrong.
func (e *ConfigError) ExitCode() int { return exitUsage }

// Code is the machine-readable code for the JSON envelope.
func (e *ConfigError) Code() string { return "invalid_config" }

// A ConflictError reports two definitions of the same server inside one
// layer — two files in servers.d, say. Across layers an override is the
// design and not an error; within a layer there is no rule that would
// pick a winner, so lightspeed refuses instead of guessing.
type ConflictError struct {
	Name   string
	First  Origin
	Second Origin
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("server %q is defined twice in the same configuration layer: %s and %s (rename or remove one)",
		e.Name, e.First, e.Second)
}

func (e *ConflictError) Is(target error) bool { return target == ErrInvalidConfig }

func (e *ConflictError) ExitCode() int { return exitUsage }

func (e *ConflictError) Code() string { return "config_conflict" }

// A NotInstalledError reports that a server is configured but its
// executable could not be found or could not be run. It carries the
// exact command that would fix it, because PLAN §6 requires that a
// missing server exits 3 with the command to run and never installs
// anything by itself.
type NotInstalledError struct {
	// Name is the server definition's name.
	Name string
	// Binary is command[0], the executable that was looked for.
	Binary string
	// Reason says what was found instead: not on PATH at all, or
	// present and unusable.
	Reason string
	// InstallCommand is the argv to run, or nil when the definition
	// has no install spec.
	InstallCommand []string
	// Offline reports that the offline kill switch is active, so the
	// install command will refuse too until it is cleared.
	Offline bool
}

func (e *NotInstalledError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s", e.Name, ErrNotInstalled)
	if e.Reason != "" {
		fmt.Fprintf(&b, " (%s)", e.Reason)
	}
	if len(e.InstallCommand) > 0 {
		fmt.Fprintf(&b, "; install it with: %s", strings.Join(e.InstallCommand, " "))
	} else if e.Binary != "" {
		fmt.Fprintf(&b, "; put %s on PATH or set server.command in .lightspeed.toml", e.Binary)
	}
	if e.Offline {
		b.WriteString(" (offline mode is active: nothing will be downloaded until it is cleared)")
	}
	return b.String()
}

func (e *NotInstalledError) Is(target error) bool { return target == ErrNotInstalled }

// ExitCode reports PLAN §4's no-server code.
func (e *NotInstalledError) ExitCode() int { return exitNoServer }

func (e *NotInstalledError) Code() string { return "server_not_installed" }

// An OfflineError reports an operation refused by --offline or
// LIGHTSPEED_OFFLINE=1. It exists so that "nothing happened" is never
// mistaken for "nothing needed to happen".
type OfflineError struct {
	// Action is what was refused, in the imperative: "install gopls".
	Action string
	// Command is the argv that would have run, for the message.
	Command []string
}

func (e *OfflineError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "refusing to %s: %s", e.Action, ErrOffline)
	fmt.Fprintf(&b, " (%s=1 or --offline)", EnvOffline)
	if len(e.Command) > 0 {
		fmt.Fprintf(&b, "; run this yourself when online: %s", strings.Join(e.Command, " "))
	}
	return b.String()
}

func (e *OfflineError) Is(target error) bool { return target == ErrOffline }

func (e *OfflineError) ExitCode() int { return exitNoServer }

func (e *OfflineError) Code() string { return "offline" }

// A MiseUnavailableError reports that installation cannot be delegated
// because mise is not there. lightspeed never downloads anything itself
// (PLAN §1, §6), so this is a dead end by design, and the message says
// what to do instead.
type MiseUnavailableError struct {
	// Action is what could not be done.
	Action string
	// Err is the lookup or execution failure, if any.
	Err error
	// Binary is the server executable the user can install by hand.
	Binary string
}

func (e *MiseUnavailableError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "cannot %s: %s", e.Action, ErrMiseUnavailable)
	if e.Err != nil {
		fmt.Fprintf(&b, " (%v)", e.Err)
	}
	b.WriteString("; install mise (https://mise.jdx.dev)")
	if e.Binary != "" {
		fmt.Fprintf(&b, " or put %s on PATH yourself", e.Binary)
	}
	return b.String()
}

func (e *MiseUnavailableError) Unwrap() error { return e.Err }

func (e *MiseUnavailableError) Is(target error) bool { return target == ErrMiseUnavailable }

func (e *MiseUnavailableError) ExitCode() int { return exitNoServer }

func (e *MiseUnavailableError) Code() string { return "mise_unavailable" }

// A NoSuchServerError reports a request for a server no layer defines.
// It lists what is defined, because the usual cause is a spelling.
type NoSuchServerError struct {
	Name  string
	Known []string
}

func (e *NoSuchServerError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("%s: %q (no servers are configured at all)", ErrNoSuchServer, e.Name)
	}
	return fmt.Sprintf("%s: %q (configured: %s)", ErrNoSuchServer, e.Name, strings.Join(e.Known, ", "))
}

func (e *NoSuchServerError) Is(target error) bool { return target == ErrNoSuchServer }

func (e *NoSuchServerError) ExitCode() int { return exitUsage }

func (e *NoSuchServerError) Code() string { return "no_such_server" }

// An InstallFailedError reports that mise ran and failed. The output is
// mise's, trimmed: lightspeed has no better diagnosis to offer than the
// installer's own.
type InstallFailedError struct {
	Name    string
	Command []string
	Err     error
	Output  string
}

func (e *InstallFailedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "installing %s failed: %s", e.Name, strings.Join(e.Command, " "))
	if e.Err != nil {
		fmt.Fprintf(&b, ": %v", e.Err)
	}
	if e.Output != "" {
		fmt.Fprintf(&b, "\n%s", e.Output)
	}
	return b.String()
}

func (e *InstallFailedError) Unwrap() error { return e.Err }

func (e *InstallFailedError) Is(target error) bool { return target == ErrInstallFailed }

// ExitCode reports PLAN §4's crash code: an operation lightspeed
// delegated did not complete. It is deliberately not exitProblems,
// which means "we asked and got an answer".
func (e *InstallFailedError) ExitCode() int { return exitCrash }

func (e *InstallFailedError) Code() string { return "install_failed" }
