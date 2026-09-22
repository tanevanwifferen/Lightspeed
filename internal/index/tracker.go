package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// racyWindow is how close to "now" a modification time must be for the stat of
// a file not to be trusted. A file edited twice within the resolution of the
// file system's clock keeps its size and mtime when the second edit is the same
// length (git calls such a file "racily clean"); so a file whose mtime is
// within racyWindow of the moment it was recorded or scanned is compared by its
// hash and not by its stat until it has aged past the window.
const racyWindow = 2 * time.Second

// hashLimit bounds the size of a file that is hashed only to settle a racy
// stat. A larger file is compared by stat alone, and is a file the index would
// skip as too large anyway.
const hashLimit = 4 << 20

// HashBytes is the hash the index records for a file's content.
func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hashFile reads and hashes a file.
func hashFile(path string, limit int64) (hash string, content []byte, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	if limit > 0 {
		content, err = io.ReadAll(io.LimitReader(f, limit+1))
	} else {
		content, err = io.ReadAll(f)
	}
	if err != nil {
		return "", nil, err
	}
	return HashBytes(content), content, nil
}

// A FileState is what a stat says about a file.
type FileState struct {
	Size    int64 `json:"size"`
	MTimeNs int64 `json:"mtime_ns"`
	// Racy is set when the mtime was within racyWindow of the scan, in which
	// case Hash holds the content's hash (when the file was small enough to
	// hash) and is what two states are compared by.
	Racy bool   `json:"racy,omitempty"`
	Hash string `json:"hash,omitempty"`
}

// A Snapshot is the workspace's file list at one moment, with the state of
// every file on it.
type Snapshot struct {
	Root string
	// Files are the regular files, sorted, relative to Root with forward
	// slashes. Anything else the listing named is in Skipped.
	Files []string
	State map[string]FileState
	// Skipped are listed paths that are not indexable files, with the reason:
	// "symlink", "not_regular", "unreadable". A path that is gone or a
	// directory (a submodule) is dropped without a reason: it is not there.
	Skipped map[string]string
	// Source is how the list was made ("git" or "walk"), Warnings what it
	// said.
	Source   string
	Warnings []string
	Taken    time.Time
	// Listed reports that this scan ran the enumeration; false means the
	// previous listing was reused because no directory or listed file moved.
	Listed bool

	dirs map[string]int64 // directory (relative, "." for the root) -> mtime
	// youngDirs is set when a directory's mtime was within racyWindow of the
	// scan: a file created in it a moment after the listing was made would
	// leave the same mtime, so the next scan lists again instead of trusting
	// it.
	youngDirs bool
}

// A Change is one file that differs between two snapshots.
type Change struct {
	Path string
	Kind ChangeKind
}

// ChangeKind is what happened to a file. The values are those of LSP's
// FileChangeType, so a reconciler can hand them to a server as they are.
type ChangeKind int

const (
	Created ChangeKind = 1
	Changed ChangeKind = 2
	Deleted ChangeKind = 3
)

func (k ChangeKind) String() string {
	switch k {
	case Created:
		return "created"
	case Changed:
		return "changed"
	case Deleted:
		return "deleted"
	}
	return "unknown"
}

// A Tracker scans a workspace for its files and their state. It is the one
// change detection the daemon's reconciliation of its servers and the index's
// revalidation both stand on (D33), so that "the file changed" means the same
// thing to both.
//
// A scan is cheap when nothing moved: the file list is reused unless a
// directory of the workspace — empty ones too, see [watchDirs] — changed its
// mtime (something was created, deleted or renamed in it) or a listed file
// vanished or is a .gitignore that
// changed, and then it costs one lstat per directory and per file — about a
// millisecond for a few hundred files — and no process spawn. The list is
// re-made (one `git ls-files`) exactly when something in it may have changed.
type Tracker struct {
	root string
	list func(ctx context.Context, root string) (Listing, error)

	mu   sync.Mutex
	last *Snapshot
}

// NewTracker returns a tracker for the workspace at root, listing with
// [ListFiles].
func NewTracker(root string) *Tracker {
	return &Tracker{root: root, list: ListFiles}
}

// Root is the workspace the tracker scans.
func (t *Tracker) Root() string { return t.root }

// Scan lists the workspace and stats every file on the list.
func (t *Tracker) Scan(ctx context.Context) (*Snapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := time.Now()

	var files, watch []string
	var listing Listing
	relist := t.last == nil
	if !relist {
		relist = t.dirsMoved()
	}
	for pass := 0; ; pass++ {
		if relist {
			var err error
			listing, err = t.list(ctx, t.root)
			if err != nil {
				return nil, err
			}
			files = listing.Files
			var capped bool
			watch, capped = watchDirs(ctx, t.root, files)
			if capped {
				listing.Warnings = append(append([]string(nil), listing.Warnings...),
					fmt.Sprintf("more than %d directories: new files in the ones past that are not noticed until something else changes", dirWatchLimit))
			}
		} else {
			files, listing = t.last.candidateFiles(), Listing{Source: t.last.Source, Warnings: t.last.Warnings}
			watch = t.last.watchedDirs()
		}
		snap, moved := t.statAll(ctx, files, watch, now)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if moved && !relist && pass == 0 {
			// A listed file vanished, or a .gitignore changed: the list is
			// not trustworthy any more.
			relist = true
			continue
		}
		snap.Source, snap.Warnings, snap.Listed = listing.Source, listing.Warnings, relist
		t.last = snap
		return snap, nil
	}
}

// candidateFiles are the paths the previous scan listed, skipped ones
// included: a path that was a symlink may have become a file.
func (s *Snapshot) candidateFiles() []string {
	out := make([]string, 0, len(s.Files)+len(s.Skipped))
	out = append(out, s.Files...)
	for p := range s.Skipped {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// dirWatchLimit bounds how many directories a tracker watches, so that a
// directory that is not a repository is not stat'ed to the end on every scan.
const dirWatchLimit = 50000

// watchDirs are the directories whose mtime says that the file list may have
// changed: every directory of the workspace, including the ones that hold no
// listed file — creating the first file in an empty directory changes only
// that directory's mtime, and a tracker that watched only the ancestors of
// listed files would never see it. The walk does not enter .git, nor the
// hidden and build directories [SkipDir] names unless a listed file lives in
// them (git may track .github/ or vendor/); ignore rules are not applied,
// which errs toward a needless relist, never a missed file. The paths are
// relative, with forward slashes, "." the root.
func watchDirs(ctx context.Context, root string, listed []string) (dirs []string, capped bool) {
	holds := map[string]bool{}
	for _, rel := range listed {
		for d := path.Dir(rel); d != "." && !holds[d]; d = path.Dir(d) {
			holds[d] = true
		}
	}
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return filepath.SkipAll
		}
		if !d.IsDir() {
			return nil
		}
		rel := "."
		if p != root {
			r, err := filepath.Rel(root, p)
			if err != nil {
				return filepath.SkipDir
			}
			rel = filepath.ToSlash(r)
			if d.Name() == ".git" || (SkipDir(d.Name()) && !holds[rel]) {
				return filepath.SkipDir
			}
		}
		if len(dirs) >= dirWatchLimit {
			capped = true
			return filepath.SkipAll
		}
		dirs = append(dirs, rel)
		return nil
	})
	return dirs, capped
}

// watchedDirs are the directories the snapshot recorded an mtime for.
func (s *Snapshot) watchedDirs() []string {
	out := make([]string, 0, len(s.dirs))
	for d := range s.dirs {
		out = append(out, d)
	}
	return out
}

// dirsMoved reports whether the mtime of any watched directory differs from the
// previous scan's, or a watched directory is gone.
func (t *Tracker) dirsMoved() bool {
	if t.last.youngDirs {
		return true
	}
	for dir, was := range t.last.dirs {
		fi, err := os.Lstat(filepath.Join(t.root, filepath.FromSlash(dir)))
		if err != nil || fi.ModTime().UnixNano() != was {
			return true
		}
	}
	return false
}

// statAll stats every listed file and every watched directory (and the
// ancestors of the files, which are watched too). moved is set when the
// previous list can no longer be reused: a file it named is gone, or a
// .gitignore changed.
func (t *Tracker) statAll(ctx context.Context, listed, watch []string, now time.Time) (snap *Snapshot, moved bool) {
	snap = &Snapshot{
		Root:    t.root,
		Files:   make([]string, 0, len(listed)),
		State:   make(map[string]FileState, len(listed)),
		Skipped: map[string]string{},
		Taken:   now,
		dirs:    map[string]int64{},
	}
	dirs := map[string]bool{".": true}
	for _, d := range watch {
		dirs[d] = true
	}
	for _, rel := range listed {
		if ctx.Err() != nil {
			return snap, false
		}
		abs := filepath.Join(t.root, filepath.FromSlash(rel))
		fi, why := statRegular(abs)
		switch why {
		case "":
		case "gone":
			moved = true
			continue
		case "directory":
			continue
		default:
			snap.Skipped[rel] = why
			for d := path.Dir(rel); ; d = path.Dir(d) {
				dirs[d] = true
				if d == "." {
					break
				}
			}
			continue
		}
		st := FileState{Size: fi.Size(), MTimeNs: fi.ModTime().UnixNano()}
		if now.Sub(fi.ModTime()) < racyWindow {
			st.Racy = true
			if fi.Size() <= hashLimit {
				if h, _, err := hashFile(abs, hashLimit); err == nil {
					st.Hash = h
				}
			}
		}
		if t.last != nil && path.Base(rel) == ".gitignore" {
			if was, ok := t.last.State[rel]; !ok || was != st {
				moved = true
			}
		}
		snap.Files = append(snap.Files, rel)
		snap.State[rel] = st
		for d := path.Dir(rel); ; d = path.Dir(d) {
			dirs[d] = true
			if d == "." {
				break
			}
		}
	}
	for d := range dirs {
		if fi, err := os.Lstat(filepath.Join(t.root, filepath.FromSlash(d))); err == nil {
			snap.dirs[d] = fi.ModTime().UnixNano()
			if now.Sub(fi.ModTime()) < racyWindow {
				snap.youngDirs = true
			}
		}
	}
	sort.Strings(snap.Files)
	return snap, moved
}

// Same reports whether two states of a file are the same file content as far as
// a stat can tell: equal size and mtime, and, when either was taken racily, an
// equal hash. A racy state with no hash (a large file) is never "the same".
func (a FileState) Same(b FileState) bool {
	if a.Size != b.Size || a.MTimeNs != b.MTimeNs {
		return false
	}
	if a.Racy || b.Racy {
		return a.Hash != "" && a.Hash == b.Hash
	}
	return true
}

// Diff is what changed between two snapshots of the same workspace: files that
// are new, files whose state differs, and files that are gone, sorted by path.
// A nil previous snapshot is a baseline and has no changes.
func Diff(prev, cur *Snapshot) []Change {
	if prev == nil || cur == nil {
		return nil
	}
	var out []Change
	for _, p := range cur.Files {
		was, ok := prev.State[p]
		switch {
		case !ok:
			out = append(out, Change{p, Created})
		case !was.Same(cur.State[p]):
			out = append(out, Change{p, Changed})
		}
	}
	for _, p := range prev.Files {
		if _, ok := cur.State[p]; !ok {
			out = append(out, Change{p, Deleted})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}
