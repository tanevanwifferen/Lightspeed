package daemon

import (
	"fmt"
	"os"
	"runtime/debug"
	"time"
)

// ProtocolVersion is the version of the wire protocol between a client and
// its daemon: the methods of service.go and the shapes of their params and
// results. Bump it when either changes incompatibly. A daemon that reports a
// different one is not spoken to, it is replaced (docs/DECISIONS.md D29).
const ProtocolVersion = 1

// A Build is the identity of one lightspeed executable: enough to tell
// whether the process on the far side of the socket is the same program as
// this one.
//
// It is the executable's size and modification time plus the protocol
// version, all read when the process starts. The path is reported and not
// compared, so that one binary reached through two names — a symlink, a
// hard link — is one build, while a rebuilt or reinstalled one, which has
// a new modification time whatever its name, is another. Hashing the file
// would be exact and would cost a read of the whole binary on every
// invocation of a tool whose selling point is that it starts at once.
type Build struct {
	// Executable is the path the process was started from.
	Executable string `json:"executable,omitempty"`
	// Size and ModTimeNS are the executable's size in bytes and its
	// modification time, in nanoseconds since the epoch.
	Size      int64 `json:"size,omitempty"`
	ModTimeNS int64 `json:"mtime_ns,omitempty"`
	// Version is what the Go toolchain embedded — the module version or the
	// VCS revision — for a human reading `daemon status`. It is not
	// compared.
	Version string `json:"version,omitempty"`
	// Protocol is [ProtocolVersion] of the process.
	Protocol int `json:"protocol"`

	// info is the file as it was at start, so that a later stat can tell
	// "the same file" from "another file at the same path".
	info os.FileInfo
}

// selfBuild is read once, at process start, and not lazily: a daemon whose
// binary is replaced while it runs must still describe the binary it
// started from, which a later stat could no longer do.
var selfBuild = readBuild()

// SelfBuild is the identity of the running executable.
func SelfBuild() Build { return selfBuild }

func readBuild() Build {
	b := Build{Protocol: ProtocolVersion}
	if bi, ok := debug.ReadBuildInfo(); ok {
		b.Version = bi.Main.Version
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 12 {
				if b.Version == "" || b.Version == "(devel)" {
					b.Version = s.Value[:12]
				} else {
					b.Version += "+" + s.Value[:12]
				}
			}
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return b
	}
	b.Executable = exe
	if fi, err := os.Stat(exe); err == nil {
		b.Size, b.ModTimeNS, b.info = fi.Size(), fi.ModTime().UnixNano(), fi
	}
	return b
}

// ID is the identity as one string, for logs and `daemon status`.
func (b Build) ID() string {
	return fmt.Sprintf("p%d:%d:%d", b.Protocol, b.Size, b.ModTimeNS)
}

// Differs says why other is not the same build as b, or returns "" when it
// is. A build that reports no size — a daemon that predates build identity —
// differs by definition: the safe direction, as in D17.
func (b Build) Differs(other Build) string {
	switch {
	case b.Protocol != other.Protocol:
		if other.Protocol == 0 {
			return "the daemon predates build identity"
		}
		return fmt.Sprintf("protocol version %d, this command speaks %d", other.Protocol, b.Protocol)
	case b.Size != other.Size || b.ModTimeNS != other.ModTimeNS:
		return fmt.Sprintf("a different lightspeed executable (%s, modified %s)", other.describe(), other.modTime())
	}
	return ""
}

func (b Build) describe() string {
	if b.Executable == "" {
		return "unknown path"
	}
	return b.Executable
}

func (b Build) modTime() string {
	if b.ModTimeNS == 0 {
		return "at an unknown time"
	}
	return time.Unix(0, b.ModTimeNS).UTC().Format(time.RFC3339)
}

// Current reports whether the executable this build describes is still the
// file at its path. A daemon asks it about itself to learn that it was
// rebuilt, reinstalled or deleted since it started: what it serves is then
// an older program than the one anybody who runs `lightspeed` gets. A build
// that could not be read never claims to be out of date.
func (b Build) Current() bool {
	if b.info == nil {
		return true
	}
	fi, err := os.Stat(b.Executable)
	if err != nil {
		return false
	}
	return os.SameFile(b.info, fi) && fi.Size() == b.Size && fi.ModTime().UnixNano() == b.ModTimeNS
}
