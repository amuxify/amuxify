// Package watch polls a directory and hands each regular file to ingest once
// it has stopped changing. It uses no filesystem notification interface, so
// it behaves the same on every platform and on network shares, where such
// interfaces do not deliver events.
//
// Each pass lists the tree with scan.Walk, so the quarantine exclusion and
// the symlink rules are the ones ingest applies. A file is a candidate when
// its size, modification time and identity (os.SameFile, which compares the
// device and inode) have been seen unchanged for at least the settle window,
// and each version of a file is ingested once: a file that changes after it
// was ingested is a new version and is ingested again once it settles. The
// set of files the watcher remembers is pruned on every pass to the files
// the walk listed, so it is bounded by the size of the tree.
package watch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
)

// Watcher polls one directory. Root must be the cleaned absolute path of a
// directory that is not a symbolic link; the caller checks that before the
// first pass and the watcher checks it again on every pass.
type Watcher struct {
	Root    string
	Settle  time.Duration
	Exclude []string // directories the walk does not enter; ingest passes its quarantine directory

	// Ingest processes one settled file and returns its result. It is called
	// only after the file has been checked again, immediately before the
	// call, to be the same regular file at the same place in the tree.
	Ingest func(ctx context.Context, path string) report.FileResult
	// Notice, when set, receives one line for each event that is not a file
	// result: an entry that is not a regular file (a symbolic link, a FIFO,
	// a socket or a device), which is skipped and never followed. Each such
	// entry is reported once, when it appears or when its identity changes,
	// not on every pass.
	Notice func(msg string)
	// Now returns the current time; nil means time.Now. Tests set it to
	// move the clock without waiting.
	Now func() time.Time

	seen map[string]*entry
	// lastErr is the text of the last walk or root error reported, so an
	// unreadable directory is reported once when it appears or changes and
	// not on every pass, and reported again after it was readable in
	// between.
	lastErr string
}

// version is what identifies the content of a path at one observation.
type version struct {
	fi os.FileInfo
}

// same reports whether the two observations show the same version of the
// same file: same identity, same size and same modification time. Either
// side may be missing (nil), which is never the same as anything.
func (v version) same(o version) bool {
	if v.fi == nil || o.fi == nil {
		return false
	}
	return os.SameFile(v.fi, o.fi) && v.fi.Size() == o.fi.Size() && v.fi.ModTime().Equal(o.fi.ModTime())
}

func (v version) regular() bool { return v.fi != nil && v.fi.Mode().IsRegular() }

// entry is what the watcher remembers about one path between passes.
type entry struct {
	v        version
	since    time.Time // when this version was first observed
	ingested bool      // this version has been handed to Ingest
	noticed  bool      // for an entry that is not a regular file: Notice was called
}

func (w *Watcher) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now()
}

func (w *Watcher) notice(format string, a ...interface{}) {
	if w.Notice != nil {
		w.Notice(fmt.Sprintf(format, a...))
	}
}

// Pending reports how many paths the watcher currently remembers. It is
// bounded by the number of entries the last walk listed.
func (w *Watcher) Pending() int { return len(w.seen) }

// checkRoot returns an error when Root is missing, is not a directory or is
// a symbolic link. A root that was swapped for a link would otherwise make
// the walk follow it, which ingest never does (guarantee 3).
func (w *Watcher) checkRoot() error {
	fi, err := os.Lstat(w.Root)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symbolic link; the watched directory must be the directory itself", w.Root)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a directory", w.Root)
	}
	return nil
}

// Pass walks the tree once, updates what the watcher remembers and ingests
// every file that has settled. It returns the results of this pass in the
// order the files finished, and an error when the root could not be used or
// a directory could not be read. That error is returned only when it is new
// or different from the one returned by the previous pass, so a caller that
// reports it does so once per change; the files that could be listed are
// still processed. A cancelled context ends the pass between two files.
func (w *Watcher) Pass(ctx context.Context) ([]report.FileResult, error) {
	if w.seen == nil {
		w.seen = map[string]*entry{}
	}
	now := w.now()
	if err := w.checkRoot(); err != nil {
		return nil, w.report(err)
	}
	paths, walkErr := scan.Walk(w.Root, w.Exclude...)
	present := make(map[string]bool, len(paths))
	var candidates []string
	for _, p := range paths {
		present[p] = true
		fi, err := os.Lstat(p)
		if err != nil {
			// Listed a moment ago and gone already: nothing to remember.
			delete(w.seen, p)
			continue
		}
		v := version{fi: fi}
		e, ok := w.seen[p]
		if !ok || !e.v.same(v) {
			// A new path, or a new version of a known one: a different
			// file at the name (swapped), or the same file with a new
			// size or modification time (still being written, or changed
			// after it was ingested). Either way the settle clock starts
			// again and the version has not been ingested.
			e = &entry{v: v, since: now}
			w.seen[p] = e
		}
		if !v.regular() {
			if !e.noticed {
				e.noticed = true
				w.notice("skipping %s: %s", p, describe(fi))
			}
			continue
		}
		if !e.ingested && now.Sub(e.since) >= w.Settle {
			candidates = append(candidates, p)
		}
	}
	// Files that vanished are dropped, so the memory is bounded by the tree.
	for p := range w.seen {
		if !present[p] {
			delete(w.seen, p)
		}
	}
	var out []report.FileResult
	for _, p := range candidates {
		if ctx.Err() != nil {
			break
		}
		e := w.seen[p]
		if err := w.recheck(p, e.v); err != nil {
			// The file is not what the settle decision was made on: gone,
			// changed, replaced, or no longer reached through real
			// directories. It is forgotten and observed afresh on the next
			// pass, so a swapped file waits for its own settle window.
			delete(w.seen, p)
			continue
		}
		fr := w.Ingest(ctx, p)
		out = append(out, fr)
		w.record(p, e)
	}
	if walkErr != nil {
		return out, w.report(walkErr)
	}
	w.lastErr = ""
	return out, nil
}

// report returns err when its text differs from the last error reported and
// nil when it is the same one again.
func (w *Watcher) report(err error) error {
	if err.Error() == w.lastErr {
		return nil
	}
	w.lastErr = err.Error()
	return err
}

// record notes the outcome of an ingest. When the path still shows the
// version that was ingested, that version is marked ingested. Any other
// state is forgotten, so the path is observed afresh on the next pass and
// ingested again once it settles. That covers a file that someone changed
// while it was being processed, and also a file amuxify itself rewrote: the
// rewritten file is a new version, and its second ingest finds nothing left
// to do and marks it ingested. The watcher does not try to tell its own
// writes from foreign ones, because a foreign change that lands during an
// own write would then be missed.
func (w *Watcher) record(p string, e *entry) {
	fi, err := os.Lstat(p)
	if err == nil && e.v.same(version{fi: fi}) {
		e.ingested = true
		return
	}
	delete(w.seen, p)
}

// recheck confirms, immediately before the ingest, that p still leads to the
// version the settle decision was made on: every directory between Root and
// the file is a real directory (not a symbolic link, so a directory replaced
// by a link on the way is not followed), and the entry at the end is a
// regular file with the same identity, size and modification time. A file
// renamed away and another put in its place, or a file that vanished, fails
// the check.
func (w *Watcher) recheck(p string, v version) error {
	rel, err := filepath.Rel(w.Root, p)
	if err != nil {
		return err
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%s is not inside %s", p, w.Root)
	}
	if err := w.checkRoot(); err != nil {
		return err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	dir := w.Root
	for _, c := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, c)
		fi, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symbolic link", dir)
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
	}
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	now := version{fi: fi}
	if !now.regular() {
		return fmt.Errorf("%s is %s", p, describe(fi))
	}
	if !v.same(now) {
		return errors.New(p + " changed")
	}
	return nil
}

// describe names the kind of a non-regular entry for a notice line.
func describe(fi os.FileInfo) string {
	m := fi.Mode()
	switch {
	case m&os.ModeSymlink != 0:
		return "a symbolic link"
	case m.IsDir():
		return "a directory"
	case m&os.ModeNamedPipe != 0:
		return "a named pipe"
	case m&os.ModeSocket != 0:
		return "a socket"
	case m&os.ModeDevice != 0:
		return "a device"
	}
	return "not a regular file"
}
