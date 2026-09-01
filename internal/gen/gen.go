// Package gen is lightspeed's build-time importer: it turns the
// vendored nvim-lspconfig snapshot under corpus/ into the embedded
// built-in server table of PLAN §6 item 3.
//
// It exists as a generator rather than a runtime dependency for three
// reasons. The corpus is Lua, and lightspeed will not carry a Lua
// interpreter. The corpus is a snapshot, so the table it produces
// should be reviewable in a diff rather than materialised on every
// start. And PLAN §6's security posture forbids implicit downloads,
// which rules out fetching the corpus at build time — so it is checked
// in, and generation is offline and re-runnable.
//
// The pipeline, and where each stage's rules live:
//
//	corpus.go   the vendored snapshot, embedded, with its digests
//	lua.go      a reader for the declarative subset of Lua — literals
//	            only, never an interpreter
//	extract.go  one corpus module reduced to declarative facts, with a
//	            note for everything it could not import
//	curate.go   the hand-written half: globs, install specs, the root
//	            markers upstream computes in code, and the policy on
//	            what not to import
//	emit.go     deterministic Go source for internal/serverdef
//
// Run it with `go generate ./internal/serverdef/`.
package gen

import (
	"fmt"
	"os"
	"path/filepath"
)

// Generate produces the source of internal/serverdef/builtins_gen.go.
// It is separate from [Write] so that a test can assert the checked-in
// file is exactly what the generator produces — the only way a
// generated file that nobody regenerates stays trustworthy.
func Generate() ([]byte, error) {
	builtins, err := BuildAll()
	if err != nil {
		return nil, err
	}
	return Emit(builtins)
}

// Write generates the table and writes it to path, atomically enough
// for a build step: a failed generation leaves the previous file alone,
// because a half-written builtins_gen.go breaks the build of the
// generator itself and there would be no way back.
func Write(path string) error {
	src, err := Generate()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, src, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// DefaultOutput is where the generated table belongs, relative to the
// repository root.
const DefaultOutput = "internal/serverdef/builtins_gen.go"

// ResolveOutput turns a possibly-relative output path into an absolute
// one, so that the generator behaves the same run from the package
// directory (as `go generate` does) or from the repository root.
func ResolveOutput(path string) (string, error) {
	if path == "" {
		path = DefaultOutput
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("output path: %w", err)
	}
	return abs, nil
}
