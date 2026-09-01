package serverdef

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A BinarySource says how a server's executable was found. It is
// PLAN §6 item 4 — PATH sniffing — plus the cases that item leaves
// implicit: an absolute command, a mise-managed tool, and the two
// failures worth telling apart.
type BinarySource int

const (
	// BinaryNotFound: nothing with that name exists anywhere we
	// looked.
	BinaryNotFound BinarySource = iota
	// BinaryCommandPath: server.command[0] was itself a path, so no
	// searching was needed.
	BinaryCommandPath
	// BinaryPATH: found by sniffing PATH. This is the zero-config
	// case of PLAN §6, and the common one.
	BinaryPATH
	// BinaryMise: not on PATH, but `mise which` knew it. A tool
	// installed by `lightspeed install` without mise's shims
	// activated in this shell looks like this.
	BinaryMise
	// BinaryUnusable: something with that name is on PATH but cannot
	// be executed — a directory, a file without the executable bit,
	// or a shim pointing at a version that is not installed. Told
	// apart from BinaryNotFound because the fix is different.
	BinaryUnusable
)

func (s BinarySource) String() string {
	switch s {
	case BinaryCommandPath:
		return "command_path"
	case BinaryPATH:
		return "path"
	case BinaryMise:
		return "mise"
	case BinaryUnusable:
		return "unusable"
	default:
		return "not_found"
	}
}

// binarySources is every source, so that the JSON name can be decoded
// back without a second switch that could drift from String.
var binarySources = []BinarySource{BinaryNotFound, BinaryCommandPath, BinaryPATH, BinaryMise, BinaryUnusable}

// MarshalJSON renders a source as its name, for the same reason
// [Layer.MarshalJSON] does: an agent reading the envelope of PLAN §4
// should not have to know our iota order.
func (s BinarySource) MarshalJSON() ([]byte, error) { return json.Marshal(s.String()) }

// UnmarshalJSON accepts the name MarshalJSON produces, falling back to
// [BinaryNotFound] for a name this build does not know.
func (s *BinarySource) UnmarshalJSON(data []byte) error {
	var name string
	if err := json.Unmarshal(data, &name); err != nil {
		return err
	}
	for _, candidate := range binarySources {
		if candidate.String() == name {
			*s = candidate
			return nil
		}
	}
	*s = BinaryNotFound
	return nil
}

// A Binary is the outcome of looking for one server's executable.
type Binary struct {
	// Name is server.command[0], the executable that was looked for.
	Name string `json:"name"`
	// Path is where it was found, absolute where possible, empty when
	// it was not found at all.
	Path string `json:"path,omitempty"`
	// Source says how it was found.
	Source BinarySource `json:"source"`
	// Runnable reports that Path exists and is executable. A false
	// Runnable with a non-empty Path is the "installed but broken"
	// case `doctor` has to explain.
	Runnable bool `json:"runnable"`
	// Problem is why it is not runnable, empty when it is.
	Problem string `json:"problem,omitempty"`
	// Probed reports that the lookup actually happened, so that a
	// zero Binary is not mistaken for "missing".
	Probed bool `json:"probed"`
}

func (b Binary) String() string {
	switch {
	case !b.Probed:
		return b.Name + ": not looked for"
	case b.Runnable:
		return fmt.Sprintf("%s: %s (%s)", b.Name, b.Path, b.Source)
	case b.Path != "":
		return fmt.Sprintf("%s: %s is not runnable (%s)", b.Name, b.Path, b.Problem)
	default:
		return fmt.Sprintf("%s: not found (%s)", b.Name, b.Problem)
	}
}

// probeBinary looks for def's executable: the command itself if it is a
// path, then PATH, then mise. mise is consulted last and only when it
// is already available, so that resolution never depends on a network
// or on an installer being present.
func probeBinary(ctx context.Context, def *ServerDef, opts Options, mise MiseStatus) Binary {
	if len(def.Server.Command) == 0 || def.Server.Command[0] == "" {
		return Binary{Probed: true, Problem: "definition has no server.command"}
	}
	name := def.Server.Command[0]
	b := Binary{Name: name, Probed: true}

	// A command that is a path is used verbatim: the user said where.
	if strings.ContainsRune(name, filepath.Separator) || filepath.IsAbs(name) {
		abs := name
		if a, err := filepath.Abs(name); err == nil {
			abs = a
		}
		b.Source = BinaryCommandPath
		b.Path = abs
		b.Runnable, b.Problem = executable(abs)
		return b
	}

	if path, err := opts.lookPath(name); err == nil {
		b.Source = BinaryPATH
		b.Path = path
		b.Runnable, b.Problem = executable(path)
		if b.Runnable {
			return b
		}
		// Fall through: a broken shim on PATH may still have a real
		// binary behind it in mise.
	}

	if path, ok := miseWhich(ctx, name, opts, mise); ok {
		if runnable, problem := executable(path); runnable {
			return Binary{Name: name, Path: path, Source: BinaryMise, Runnable: true, Probed: true}
		} else if b.Path == "" {
			b.Source = BinaryMise
			b.Path = path
			b.Runnable, b.Problem = false, problem
			return b
		}
	}

	if b.Path != "" {
		// PATH had it and it was not runnable.
		b.Source = BinaryUnusable
		return b
	}

	// Nothing usable was found. Look once more with Lstat, which sees
	// what an exec lookup cannot: a dangling symlink, a directory, a
	// file whose executable bit is missing.
	if path, problem, ok := scanPathForName(name, opts.getenv("PATH")); ok {
		b.Source = BinaryUnusable
		b.Path = path
		b.Problem = problem
		return b
	}

	b.Source = BinaryNotFound
	b.Problem = "not found on PATH"
	return b
}

// executable reports whether path can be executed, and why not if it
// cannot. The failures it names are the ones seen in practice: a
// version manager's shim pointing at an uninstalled tool, a downloaded
// archive extracted without the executable bit, a directory shadowing
// a binary name.
func executable(path string) (bool, string) {
	fi, err := os.Stat(path)
	if err != nil {
		if _, lerr := os.Lstat(path); lerr == nil {
			return false, "symlink target is missing"
		}
		if os.IsNotExist(err) {
			return false, "does not exist"
		}
		return false, err.Error()
	}
	if fi.IsDir() {
		return false, "is a directory"
	}
	if fi.Mode().Perm()&0o111 == 0 {
		return false, fmt.Sprintf("is not executable (mode %s)", fi.Mode().Perm())
	}
	return true, ""
}

// scanPathForName walks PATH with Lstat looking for an entry called
// name, so that "there is a gopls on your PATH and it is broken" can be
// said instead of "gopls is not installed".
func scanPathForName(name, pathEnv string) (path, problem string, found bool) {
	for _, dir := range filepath.SplitList(pathEnv) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, name)
		if _, err := os.Lstat(candidate); err != nil {
			continue
		}
		runnable, problem := executable(candidate)
		if runnable {
			// Runnable but not returned by the exec lookup: possible
			// if PATH changed under us. Report it as found anyway.
			return candidate, "", true
		}
		return candidate, problem, true
	}
	return "", "", false
}

// dirExists is a small helper the layer discovery shares with the
// binary probe.
func dirExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// fileExists reports whether path is a regular file (or a symlink to
// one).
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	return fi.Mode().IsRegular()
}

// tomlFilesIn lists the *.toml files of dir, sorted, so that the user
// layer is loaded in one deterministic order on every machine.
func tomlFilesIn(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".toml") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out, nil
}
