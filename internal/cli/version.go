package cli

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/daemon"
)

// version is stamped by the linker (`-ldflags "-X
// github.com/tanevanwifferen/Lightspeed/internal/cli.version=v0.1.0"`, which the
// Makefile does). Left empty, the version is what the Go toolchain embedded: the
// module version, or the VCS revision.
var version string

// buildInfo is what `lightspeed version` reports about this executable.
type buildInfo struct {
	// Version is the stamped version, else the module version or VCS revision
	// the toolchain embedded, else "dev".
	Version string `json:"version"`
	// Build is the build identity the daemon compares (internal/daemon Build.ID):
	// protocol version, executable size and modification time. It changes with
	// every rebuild, which is what makes it an identity.
	Build    string `json:"build"`
	Protocol int    `json:"protocol"`
	// Guide is the version of the agent guide this binary generates.
	Guide      int    `json:"guide"`
	Go         string `json:"go"`
	Module     string `json:"module,omitempty"`
	Executable string `json:"executable,omitempty"`
	// VCS* are embedded by `go build` inside a repository (not by `go run`).
	VCSRevision string `json:"vcs_revision,omitempty"`
	VCSTime     string `json:"vcs_time,omitempty"`
	VCSModified bool   `json:"vcs_modified,omitempty"`
}

// currentBuildInfo reads the running executable's identity.
func currentBuildInfo() buildInfo {
	b := daemon.SelfBuild()
	info := buildInfo{
		Version: version, Build: b.ID(), Protocol: b.Protocol, Guide: guideVersion,
		Go: runtime.Version(), Executable: b.Executable,
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		info.Module = bi.Main.Path
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				info.VCSRevision = s.Value
			case "vcs.time":
				info.VCSTime = s.Value
			case "vcs.modified":
				info.VCSModified = s.Value == "true"
			}
		}
	}
	if info.Version == "" {
		info.Version = embeddedVersion()
		if info.Version == "dev" && len(info.VCSRevision) >= 12 {
			info.Version = "dev+" + info.VCSRevision[:12]
		}
		// Go stamps a module version of a modified tree with +dirty itself.
		if info.VCSModified && info.Version != "dev" && !strings.HasSuffix(info.Version, "+dirty") {
			info.Version += "+dirty"
		}
	}
	return info
}

func writeVersionText(w io.Writer, b buildInfo) {
	fmt.Fprintf(w, "lightspeed %s\n", b.Version)
	fmt.Fprintf(w, "  build       %s\n", b.Build)
	fmt.Fprintf(w, "  guide       v%d\n", b.Guide)
	fmt.Fprintf(w, "  go          %s\n", b.Go)
	if b.Module != "" {
		fmt.Fprintf(w, "  module      %s\n", b.Module)
	}
	if b.VCSRevision != "" {
		mod := ""
		if b.VCSModified {
			mod = " (modified)"
		}
		fmt.Fprintf(w, "  revision    %s%s %s\n", b.VCSRevision, mod, b.VCSTime)
	}
	if b.Executable != "" {
		fmt.Fprintf(w, "  executable  %s\n", b.Executable)
	}
}

// embeddedVersion is the module version the toolchain embedded (a tag or a
// pseudo-version when installed with `go install ...@version`), or "dev" for a
// build from a checkout, where it is "(devel)".
func embeddedVersion() string {
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
