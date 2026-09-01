package gen

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"slices"
)

// The vendored corpus, embedded so that generation reads no path that
// depends on where the command was run from, and needs no network. See
// corpus/nvim-lspconfig/PROVENANCE.md for the snapshot's commit and
// license, and ATTRIBUTION at the repository root for the notice.
//
//go:embed corpus
var corpusFS embed.FS

// Where the snapshot came from. These are the same values PROVENANCE.md
// records; corpus_test.go asserts they agree, so the two cannot drift.
const (
	// CorpusName is the upstream project the defaults derive from.
	CorpusName = "nvim-lspconfig"
	// CorpusUpstream is its repository.
	CorpusUpstream = "https://github.com/neovim/nvim-lspconfig"
	// CorpusCommit is the exact commit vendored.
	CorpusCommit = "bff1bd61cb1455040533201ca1edf1e84efa578f"
	// CorpusLicense is the upstream license. Apache-2.0 has no
	// copyleft clause, which is why this corpus was chosen over
	// Helix's MPL-2.0 languages.toml (PLAN §9.3).
	CorpusLicense = "Apache-2.0"
	// CorpusSnapshot is where the vendored files live in this repo.
	CorpusSnapshot = "internal/gen/corpus/" + CorpusName
	// corpusRoot is the embedded directory holding the server files.
	corpusRoot = "corpus/" + CorpusName + "/lsp"
)

// CorpusFile reads one vendored server file. The name is the corpus's
// own, without the .lua suffix ("rust_analyzer").
func CorpusFile(name string) ([]byte, error) {
	data, err := corpusFS.ReadFile(path.Join(corpusRoot, name+".lua"))
	if err != nil {
		return nil, fmt.Errorf("corpus: %w", err)
	}
	return data, nil
}

// CorpusFiles lists the vendored server names, sorted.
func CorpusFiles() ([]string, error) {
	entries, err := corpusFS.ReadDir(corpusRoot)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, trimLua(e.Name()))
	}
	slices.Sort(names)
	return names, nil
}

func trimLua(name string) string {
	if len(name) > 4 && name[len(name)-4:] == ".lua" {
		return name[:len(name)-4]
	}
	return name
}

// CorpusDigests returns the sha256 of every vendored file, keyed by its
// path relative to the snapshot directory. It exists so that a test can
// hold the snapshot to the digests PROVENANCE.md publishes: an
// in-place edit of a vendored file turns the snapshot into an
// unattributed fork, which is precisely what a vendored corpus must not
// silently become.
func CorpusDigests() (map[string]string, error) {
	out := map[string]string{}
	root := "corpus/" + CorpusName
	err := fs.WalkDir(corpusFS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := fs.Sub(corpusFS, root)
		if err != nil {
			return err
		}
		name := p[len(root)+1:]
		if name == "PROVENANCE.md" {
			// Ours, not upstream's: it is the file that records the
			// digests, so it cannot be one of them.
			return nil
		}
		data, err := fs.ReadFile(rel, name)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		out[name] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
