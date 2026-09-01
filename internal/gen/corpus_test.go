package gen

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestCorpusDigestsMatchProvenance holds the vendored snapshot to the
// digests PROVENANCE.md publishes. Editing a vendored file in place is
// the tempting way to "fix" a built-in, and it would quietly turn an
// attributed snapshot into an unattributed fork; this test makes that
// fail instead.
func TestCorpusDigestsMatchProvenance(t *testing.T) {
	published, err := publishedDigests()
	if err != nil {
		t.Fatal(err)
	}
	if len(published) == 0 {
		t.Fatal("PROVENANCE.md publishes no digests")
	}
	actual, err := CorpusDigests()
	if err != nil {
		t.Fatal(err)
	}
	if len(actual) != len(published) {
		t.Errorf("the snapshot holds %d files, PROVENANCE.md lists %d", len(actual), len(published))
	}
	for name, want := range published {
		got, ok := actual[name]
		if !ok {
			t.Errorf("%s is listed in PROVENANCE.md but not in the snapshot", name)
			continue
		}
		if got != want {
			t.Errorf("%s has digest %s, PROVENANCE.md says %s: the vendored file was edited", name, got, want)
		}
	}
	for name := range actual {
		if _, ok := published[name]; !ok {
			t.Errorf("%s is in the snapshot but not listed in PROVENANCE.md", name)
		}
	}
}

// publishedDigests reads the "<sha256>  <path>" lines of PROVENANCE.md.
func publishedDigests() (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join("corpus", CorpusName, "PROVENANCE.md"))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != 64 {
			continue
		}
		if strings.ContainsAny(fields[0], "ghijklmnopqrstuvwxyz") {
			continue
		}
		out[fields[1]] = fields[0]
	}
	return out, nil
}

// TestProvenanceAgreesWithConstants keeps the file a human reads and the
// constants the generator emits from drifting apart.
func TestProvenanceAgreesWithConstants(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("corpus", CorpusName, "PROVENANCE.md"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{CorpusCommit, CorpusUpstream, CorpusLicense} {
		if !strings.Contains(text, want) {
			t.Errorf("PROVENANCE.md does not mention %q", want)
		}
	}
}

// TestCorpusLicenseIsPresentAndPermissive: Apache-2.0 requires the
// license to travel with the copy, and PLAN §9.3's open question is only
// closed as long as the corpus is not copyleft.
func TestCorpusLicenseIsPresentAndPermissive(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("corpus", CorpusName, "LICENSE.md"))
	if err != nil {
		t.Fatalf("the vendored corpus has no LICENSE file: %v", err)
	}
	if !strings.Contains(string(data), "Apache License") {
		t.Error("LICENSE.md is not the Apache License; the corpus choice of PLAN §9.3 depends on it")
	}
	if strings.Contains(string(data), "Mozilla Public License") {
		t.Error("the snapshot carries an MPL licence, which PLAN §9.3 deliberately avoided")
	}
}

// TestCorpusFilesCoverTheCurations: every curated server has a vendored
// file, and nothing is vendored that no curation uses — a snapshot with
// dead files invites the next reader to wonder which half is live.
func TestCorpusFilesCoverTheCurations(t *testing.T) {
	files, err := CorpusFiles()
	if err != nil {
		t.Fatal(err)
	}
	var wanted []string
	for _, c := range curations {
		wanted = append(wanted, c.Corpus)
		if _, err := CorpusFile(c.Corpus); err != nil {
			t.Errorf("curation %q: %v", c.Name, err)
		}
	}
	slices.Sort(wanted)
	if !slices.Equal(files, wanted) {
		t.Errorf("vendored files = %v, want exactly the curated ones %v", files, wanted)
	}
}

func TestCorpusFileMissing(t *testing.T) {
	if _, err := CorpusFile("no_such_server"); err == nil {
		t.Error("CorpusFile() found a file that is not there")
	}
}

// TestCorpusParses is the smoke test that matters when the snapshot is
// refreshed: every vendored file still yields the declarative facts the
// generator needs.
func TestCorpusParses(t *testing.T) {
	files, err := CorpusFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		t.Run(name, func(t *testing.T) {
			src, err := CorpusFile(name)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := ExtractCorpus(name, src)
			if err != nil {
				t.Fatalf("ExtractCorpus() = %v", err)
			}
			if len(cfg.Cmd) == 0 || cfg.Cmd[0] == "" {
				t.Errorf("cmd = %v", cfg.Cmd)
			}
			if len(cfg.Filetypes) == 0 {
				t.Errorf("filetypes = %v", cfg.Filetypes)
			}
			// Every file in this snapshot has upstream behaviour that
			// cannot be imported; the notes are how that stays visible.
			if len(cfg.Notes) == 0 {
				t.Error("no notes at all: either upstream became purely declarative, or the reader stopped noticing")
			}
		})
	}
}

// TestExtractRootMarkerNotes pins which servers' markers come from the
// corpus and which had to be curated, because that split is the honest
// part of a generated table.
func TestExtractRootMarkerNotes(t *testing.T) {
	fromCorpus := map[string]bool{"clangd": true, "pyright": true}
	for _, c := range curations {
		src, err := CorpusFile(c.Corpus)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := ExtractCorpus(c.Corpus, src)
		if err != nil {
			t.Fatal(err)
		}
		declarative := len(cfg.RootMarkers) > 0
		if declarative != fromCorpus[c.Corpus] {
			t.Errorf("%s: corpus root markers present = %t, want %t", c.Corpus, declarative, fromCorpus[c.Corpus])
		}
		if declarative && len(c.RootMarkers) > 0 {
			t.Errorf("%s: curation overrides root markers the corpus supplies; drop one or say why", c.Corpus)
		}
		if !declarative && c.RootMarkerReason == "" {
			t.Errorf("%s: root markers are curated with no reason recorded", c.Corpus)
		}
	}
}

func TestExtractErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"no table", "local x = 1\n", "does not return a table"},
		{"no cmd", "return { filetypes = { 'go' } }", "cmd is missing"},
		{"computed cmd", "return { cmd = vim.g.cmd, filetypes = { 'go' } }", "cmd: a computed expression"},
		{"no filetypes", "return { cmd = { 'x' } }", "filetypes is missing"},
		{"bad cmd entry", "return { cmd = { 'x', 1 }, filetypes = { 'go' } }", "cmd: list entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ExtractCorpus("t", []byte(tt.src))
			if err == nil {
				t.Fatalf("ExtractCorpus() = %+v, want an error mentioning %q", cfg, tt.want)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to mention %q", err, tt.want)
			}
		})
	}
}

func TestExtractNotesUnsupportedRootMarkers(t *testing.T) {
	cfg, err := ExtractCorpus("t", []byte("return { cmd = { 'x' }, filetypes = { 'go' }, root_markers = vim.g.markers }"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.RootMarkers) != 0 {
		t.Errorf("root markers = %v, want none imported", cfg.RootMarkers)
	}
	reason, ok := cfg.Note("root_markers")
	if !ok || reason == "" {
		t.Error("no note recorded for the unimportable root markers")
	}
}
