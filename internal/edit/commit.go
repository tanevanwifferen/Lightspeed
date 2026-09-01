package edit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/tanevanwifferen/Lightspeed/internal/render"
)

// tempPattern is the name given to the temp files a commit stages
// beside their targets. The leading dot keeps them out of most globs,
// and the fixed prefix makes a leftover from a killed process
// identifiable.
const tempPattern = ".lightspeed-*.tmp"

// Apply commits the staged result to disk.
//
// The commit is ordered so that the failures that actually happen —
// a read-only directory, a full disk, a file that changed since it was
// read — happen before anything on disk has changed:
//
//  1. re-check every path against the workspace boundary, and every
//     file we are about to overwrite or remove against the bytes it
//     was staged from;
//  2. write every new file's content to a temp file in its target's
//     own directory, and fsync it;
//  3. rename each temp file over its target, atomically;
//  4. move each removed file aside, then unlink it.
//
// Steps 3 and 4 are the only ones that can fail with work already
// done, and both are undoable: the pre-edit content of every file is
// in memory, and every removal is a rename until the transaction has
// committed. A failure there rolls the whole thing back and reports
// the original error; a failure of the rollback itself is reported as
// such, loudly, because it is the one case where the tree is left in a
// state nobody asked for.
//
// A crash between steps 3 and 4 — the only window the process does not
// control — leaves the new content in place and the removed files
// still there. That is visible and recoverable; the reverse order
// would lose them.
//
// Apply commits at most once. A call that fails in step 1 has touched
// nothing and may be retried; after that the staged pre-edit content
// no longer describes the disk, and a second call is refused.
func (t *Transaction) Apply() (*Result, error) {
	if t.applied {
		return nil, render.Errorf(render.CodeInternal, "edit: transaction has already been applied")
	}

	writes, removals := t.plan()
	if err := t.preflight(writes, removals); err != nil {
		return nil, err
	}

	t.applied = true
	j := &journal{rename: t.renameFn}
	if j.rename == nil {
		j.rename = os.Rename
	}

	// (2) Stage every new content beside its target.
	temps := make(map[string]string, len(writes))
	for _, st := range writes {
		tmp, err := j.stage(st)
		if err != nil {
			j.abandon()
			return nil, err
		}
		temps[st.path] = tmp
	}

	// (3) Publish.
	for _, st := range writes {
		if err := j.rename(temps[st.path], st.path); err != nil {
			return nil, j.rollback(render.Errorf(render.CodeIOError,
				"edit: %s: %v", t.ws.rel(st.path), err))
		}
		j.published(st, temps[st.path])
	}

	// (4) Remove, reversibly.
	for _, st := range removals {
		if err := j.remove(st); err != nil {
			return nil, j.rollback(render.Errorf(render.CodeIOError,
				"edit: %s: %v", t.ws.rel(st.path), err))
		}
	}

	j.commit()
	return t.result(), nil
}

// plan splits the staged states into the files to write and the files
// to remove, each sorted by path so a commit is reproducible.
func (t *Transaction) plan() (writes, removals []*fileState) {
	for _, path := range t.order {
		st := t.states[path]
		if !st.dirty() {
			continue
		}
		if st.exists {
			writes = append(writes, st)
		} else {
			removals = append(removals, st)
		}
	}
	byPath := func(a, b *fileState) int { return strings.Compare(a.path, b.path) }
	slices.SortFunc(writes, byPath)
	slices.SortFunc(removals, byPath)
	return writes, removals
}

// preflight re-checks, immediately before writing, everything that was
// checked at staging time and could have changed since.
//
// The content check is the one that matters: staging read these files,
// possibly seconds ago, and the ranges in the edit set are offsets into
// what it read. If the bytes on disk have moved on, applying the edit
// would corrupt the file at plausible-looking positions, which is the
// failure this package exists to prevent.
func (t *Transaction) preflight(writes, removals []*fileState) error {
	for _, st := range slices.Concat(writes, removals) {
		if err := t.ws.checkSymlinks(st.path); err != nil {
			return err
		}
		name := t.ws.rel(st.path)
		onDisk, content, err := readIfExists(st.path)
		if err != nil {
			return render.Errorf(render.CodeIOError, "edit: %s: %v", name, err)
		}
		switch {
		case st.onDisk && !onDisk:
			return render.Errorf(render.CodeEditConflict,
				"edit: %s: file was deleted after the edit was computed; nothing was written", name)
		case st.onDisk && !bytes.Equal(content, st.original):
			return render.Errorf(render.CodeEditConflict,
				"edit: %s: file changed on disk after the edit was computed; nothing was written", name)
		case !st.onDisk && onDisk:
			return render.Errorf(render.CodeEditConflict,
				"edit: %s: file was created after the edit was computed; nothing was written", name)
		}
	}
	return nil
}

// result describes what the commit did.
func (t *Transaction) result() *Result {
	r := &Result{}
	renamed := t.renameSources()

	for _, path := range t.order {
		st := t.states[path]
		switch {
		case !st.dirty():
			r.Unchanged = append(r.Unchanged, st.path)
		case st.exists && !st.onDisk:
			if src, ok := t.renameOf(st); ok {
				r.Renamed = append(r.Renamed, Rename{From: src.path, To: st.path})
			} else {
				r.Created = append(r.Created, st.path)
			}
		case st.exists && st.onDisk:
			r.Modified = append(r.Modified, st.path)
		case !st.exists && st.onDisk:
			if !renamed[st.path] {
				r.Deleted = append(r.Deleted, st.path)
			}
		}
	}
	slices.Sort(r.Created)
	slices.Sort(r.Modified)
	slices.Sort(r.Deleted)
	slices.Sort(r.Unchanged)
	slices.SortFunc(r.Renamed, func(a, b Rename) int { return strings.Compare(a.From, b.From) })
	return r
}

// journal records what a commit has done, so that it can be undone.
type journal struct {
	// temps are staged files not yet renamed into place.
	temps []string
	// dirs are directories created for the commit, deepest first.
	dirs []string
	// undo restores the disk, in reverse order.
	undo []func() error
	// aside maps a removed file to where it was moved, pending unlink.
	aside map[string]string
	// dirty are the directories to fsync once the commit stands.
	dirty map[string]bool
	// rename is os.Rename, indirected so that a test can fail one
	// specific rename and watch the rollback put the tree back. Only
	// the forward path goes through it: a rollback that could be
	// disabled by the fault it is recovering from would prove
	// nothing.
	rename func(oldpath, newpath string) error
}

// stage writes one file's new content to a temp file beside its
// target, creating the target's directory if the edit set implies one.
func (j *journal) stage(st *fileState) (string, error) {
	dir := filepath.Dir(st.path)
	created, err := mkdirAll(dir)
	if err != nil {
		return "", render.Errorf(render.CodeIOError, "edit: %s: %v", dir, err)
	}
	// Deepest first: undone in this order, rmdir succeeds.
	slices.Reverse(created)
	j.dirs = append(created, j.dirs...)

	f, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return "", render.Errorf(render.CodeIOError, "edit: %s: %v", st.path, err)
	}
	tmp := f.Name()
	j.temps = append(j.temps, tmp)

	mode := st.mode
	if mode == 0 {
		mode = defaultFileMode
	}
	if err := writeAndSync(f, st.content, mode); err != nil {
		return "", render.Errorf(render.CodeIOError, "edit: %s: %v", st.path, err)
	}
	j.markDirty(dir)
	return tmp, nil
}

// published records a completed rename of a staged file over its
// target, and how to undo it. The temp file is gone — the rename
// consumed it — so it drops out of the cleanup list.
func (j *journal) published(st *fileState, tmp string) {
	path, content, mode, existed := st.path, st.original, st.mode, st.onDisk
	j.temps = slices.DeleteFunc(j.temps, func(s string) bool { return s == tmp })
	j.undo = append(j.undo, func() error {
		if !existed {
			return os.Remove(path)
		}
		return writeAtomic(path, content, mode)
	})
	j.markDirty(filepath.Dir(path))
}

// remove moves a file aside. The unlink happens only once the whole
// commit stands, so a later failure can put it back.
func (j *journal) remove(st *fileState) error {
	dir := filepath.Dir(st.path)
	f, err := os.CreateTemp(dir, tempPattern)
	if err != nil {
		return err
	}
	aside := f.Name()
	f.Close()
	// Rename replaces the empty placeholder; the placeholder existed
	// only to reserve a name nothing else holds.
	if err := j.rename(st.path, aside); err != nil {
		os.Remove(aside)
		return err
	}
	if j.aside == nil {
		j.aside = make(map[string]string)
	}
	j.aside[st.path] = aside
	path := st.path
	j.undo = append(j.undo, func() error { return os.Rename(aside, path) })
	j.markDirty(dir)
	return nil
}

func (j *journal) markDirty(dir string) {
	if j.dirty == nil {
		j.dirty = make(map[string]bool)
	}
	j.dirty[dir] = true
}

// abandon discards a commit that has not yet changed anything.
func (j *journal) abandon() {
	for _, tmp := range j.temps {
		os.Remove(tmp)
	}
	for _, dir := range j.dirs {
		os.Remove(dir) // only succeeds while empty, which is what we want
	}
	j.temps, j.dirs = nil, nil
}

// rollback undoes a partially committed transaction and returns the
// error to report: cause on success, or a compound error naming what
// could not be undone.
func (j *journal) rollback(cause error) error {
	var failures []string
	for i := len(j.undo) - 1; i >= 0; i-- {
		if err := j.undo[i](); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failures = append(failures, err.Error())
		}
	}
	j.undo = nil
	for _, aside := range j.aside {
		os.Remove(aside)
	}
	j.abandon()

	if len(failures) == 0 {
		return cause
	}
	return render.Errorf(render.CodeIOError,
		"edit: %v; rolling back left the workspace inconsistent: %s",
		cause, strings.Join(failures, "; "))
}

// commit finalises: the moved-aside files go, the temp files are gone
// already, and the directories are flushed so the renames survive a
// crash. Nothing here can fail in a way that costs data, so failures
// are not reported as transaction failures — the transaction stands.
func (j *journal) commit() {
	for _, aside := range j.aside {
		os.Remove(aside)
	}
	for _, tmp := range j.temps {
		os.Remove(tmp)
	}
	for dir := range j.dirty {
		syncDir(dir)
	}
}

// -- filesystem helpers --

// readIfExists reads a file, distinguishing "not there" from "could
// not read".
func readIfExists(path string) (bool, []byte, error) {
	content, err := os.ReadFile(path)
	switch {
	case err == nil:
		return true, content, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil, nil
	default:
		return false, nil, err
	}
}

// mkdirAll creates dir and returns the directories it had to create,
// shallowest first, so a failed commit can remove exactly those.
func mkdirAll(dir string) ([]string, error) {
	var missing []string
	for d := dir; ; d = filepath.Dir(d) {
		info, err := os.Lstat(d)
		if err == nil {
			// A symlinked directory is still a directory to write
			// into; the workspace check has already established that
			// it does not leave the tree.
			if info.Mode()&fs.ModeSymlink != 0 {
				if info, err = os.Stat(d); err != nil {
					return nil, err
				}
			}
			if !info.IsDir() {
				return nil, fmt.Errorf("%s is not a directory", d)
			}
			break
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		missing = append(missing, d)
		if parent := filepath.Dir(d); parent == d {
			return nil, fmt.Errorf("no existing parent directory")
		}
	}
	slices.Reverse(missing) // shallowest first
	for _, d := range missing {
		if err := os.Mkdir(d, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
			return nil, err
		}
	}
	return missing, nil
}

// writeAndSync writes content to an open temp file and flushes it, so
// that the rename that follows publishes bytes that are really there.
func writeAndSync(f *os.File, content []byte, mode fs.FileMode) error {
	err := func() error {
		if _, err := f.Write(content); err != nil {
			return err
		}
		if err := f.Chmod(mode); err != nil {
			return err
		}
		return f.Sync()
	}()
	// Close reports write errors that only surface at flush time, so
	// it is part of the result and not a deferred afterthought.
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// writeAtomic writes a file through a temp file and a rename. It is
// the rollback path: restoring a file has to be as atomic as
// overwriting it was.
func writeAtomic(path string, content []byte, mode fs.FileMode) error {
	if mode == 0 {
		mode = defaultFileMode
	}
	f, err := os.CreateTemp(filepath.Dir(path), tempPattern)
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := writeAndSync(f, content, mode); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// syncDir flushes a directory entry. Best effort: not every platform
// or filesystem allows it, and a failure costs durability, not
// correctness.
func syncDir(dir string) {
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	f.Sync()
	f.Close()
}
