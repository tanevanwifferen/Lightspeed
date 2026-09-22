package index

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The cache lives under $XDG_CACHE_HOME/lightspeed/<workspace-hash>/ (CacheDir),
// one file per *part*: the files one server handles, or, under the part name
// "-", the files no server handles that are indexed for their imports alone.
// Splitting by server is what makes "any mismatch = rebuild that part" real: a
// gopls upgrade throws away Go's outlines and leaves Python's alone.
//
// A part file is a JSON object whose `files` member is checked by a SHA-256 in
// `checksum`, so that a truncated or hand-edited file is recognised as such
// and discarded whole, never half-trusted. It is written to a temporary file in
// the same directory, synced, and renamed over the old one: a reader sees the
// old file or the new one, and two writers (two daemons, a daemon and a
// `--no-daemon` command) each leave a complete file behind — the last rename
// wins, and what the loser had that the winner lacks is simply rebuilt.

// partFormat names the file format, so that a foreign JSON file in the
// directory is not mistaken for a part.
const partFormat = "lightspeed-index-part"

// noServerPart is the part name of files that no server handles.
const noServerPart = "-"

// A partKey is what a persisted part must match to be believed.
type partKey struct {
	Schema int `json:"schema"`
	// Build is the lightspeed build that wrote it: the id of the executable.
	// A new build may extract differently (a doc-comment rule, an import
	// scanner), so it starts from nothing.
	Build string `json:"build"`
	// Server is the definition name ("-" for the no-server part) and
	// ServerKey the cheap identity of its executable (path, size, mtime), so
	// that upgrading the server invalidates its outlines without starting it.
	Server    string `json:"server"`
	ServerKey string `json:"server_key,omitempty"`
}

// partFile is the persisted form of one part.
type partFile struct {
	Format string `json:"format"`
	partKey
	// ServerVersion is what the server called itself in `initialize`, when it
	// was running when the part was written. It is compared, when known, the
	// next time the server is running (Manager.checkVersion).
	ServerVersion string          `json:"server_version,omitempty"`
	Root          string          `json:"root"`
	Written       string          `json:"written"`
	Checksum      string          `json:"checksum"`
	Files         json.RawMessage `json:"files"`
}

// partName is the file name of a part.
func partName(server string) string {
	if server == "" || server == noServerPart {
		return "part--.json"
	}
	var b strings.Builder
	for _, r := range server {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return "part-" + b.String() + ".json"
}

// A loadedPart is a part read from disk and found to be valid.
type loadedPart struct {
	partKey
	serverVersion string
	files         []*File
	bytes         int64
}

// A discard says a part on disk was not used, and why. It is reported, not
// hidden: an index that silently throws its work away is one nobody can debug.
type discard struct {
	Name   string `json:"name"`
	Reason string `json:"reason"`
}

// readParts reads every part in dir. valid says whether a part with that key is
// acceptable — the caller knows the current build and server keys; parts that
// fail it are returned as discards, as are corrupt ones. A missing directory is
// an empty cache.
func readParts(dir string, valid func(partKey) (ok bool, why string)) (parts map[string]*loadedPart, discards []discard, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, err
	}
	parts = map[string]*loadedPart{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "part-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		p, why := readPart(filepath.Join(dir, name), valid)
		if p == nil {
			discards = append(discards, discard{Name: name, Reason: why})
			continue
		}
		parts[p.Server] = p
	}
	return parts, discards, nil
}

// readPart reads and verifies one part file; the reason is set when it is not
// usable.
func readPart(path string, valid func(partKey) (bool, string)) (*loadedPart, string) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "unreadable: " + err.Error()
	}
	var pf partFile
	if err := json.Unmarshal(raw, &pf); err != nil {
		return nil, "corrupt (not valid JSON: " + err.Error() + ")"
	}
	if pf.Format != partFormat {
		return nil, "not an index part"
	}
	sum := sha256.Sum256(pf.Files)
	if hex.EncodeToString(sum[:]) != pf.Checksum {
		return nil, "corrupt (checksum mismatch: truncated or edited)"
	}
	if ok, why := valid(pf.partKey); !ok {
		return nil, why
	}
	var files []*File
	if err := json.Unmarshal(pf.Files, &files); err != nil {
		return nil, "corrupt (files: " + err.Error() + ")"
	}
	for _, f := range files {
		if f == nil || f.Path == "" {
			return nil, "corrupt (entry without a path)"
		}
	}
	return &loadedPart{partKey: pf.partKey, serverVersion: pf.ServerVersion, files: files, bytes: int64(len(raw))}, ""
}

// writePart writes one part atomically: temp file in the same directory,
// fsync, rename.
func writePart(dir, root string, key partKey, version string, files []*File, now time.Time) (int64, error) {
	sorted := make([]*File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })
	body, err := json.Marshal(sorted)
	if err != nil {
		return 0, err
	}
	sum := sha256.Sum256(body)
	pf := partFile{
		Format: partFormat, partKey: key, ServerVersion: version, Root: root,
		Written: now.UTC().Format(time.RFC3339), Checksum: hex.EncodeToString(sum[:]), Files: body,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	if err := enc.Encode(&pf); err != nil {
		return 0, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-part-*")
	if err != nil {
		return 0, err
	}
	name := tmp.Name()
	cleanup := func() { _ = os.Remove(name) }
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		tmp.Close()
		cleanup()
		return 0, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return 0, err
	}
	if err := os.Rename(name, filepath.Join(dir, partName(key.Server))); err != nil {
		cleanup()
		return 0, err
	}
	return int64(buf.Len()), nil
}

// sweepTemp removes temporary files a killed writer left behind, if they are
// old enough not to be another writer's in-flight one.
func sweepTemp(dir string, olderThan time.Duration) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".tmp-part-") {
			continue
		}
		if fi, err := e.Info(); err == nil && time.Since(fi.ModTime()) > olderThan {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// dirSize is the total size of the part files in dir.
func dirSize(dir string) int64 {
	var n int64
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "part-") {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
	}
	return n
}

// removeParts deletes every part file in dir and the directory if it is then
// empty; it reports how many files and bytes went.
func removeParts(dir string) (files int, bytes int64, err error) {
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		if errors.Is(rerr, fs.ErrNotExist) {
			return 0, 0, nil
		}
		return 0, 0, rerr
	}
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), "part-") && !strings.HasPrefix(e.Name(), ".tmp-part-") {
			continue
		}
		if fi, ierr := e.Info(); ierr == nil {
			bytes += fi.Size()
		}
		if rmerr := os.Remove(filepath.Join(dir, e.Name())); rmerr != nil && !errors.Is(rmerr, fs.ErrNotExist) {
			err = fmt.Errorf("removing %s: %w", e.Name(), rmerr)
			continue
		}
		files++
	}
	_ = os.Remove(dir) // only succeeds when empty
	return files, bytes, err
}
