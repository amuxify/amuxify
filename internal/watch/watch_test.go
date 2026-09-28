package watch

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/report"
)

// clock is a settable time source so a test moves through settle windows
// without waiting.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func (c *clock) String() string          { return c.t.Format(time.RFC3339) }

// harness wires a Watcher with a fake ingest that records the paths it was
// handed and returns a PASS result, and a notice sink.
type harness struct {
	w        *Watcher
	clock    *clock
	ingested []string
	notices  []string
	// onIngest, when set, runs inside the fake ingest with the path.
	onIngest func(p string) report.FileResult
}

func newHarness(t *testing.T, root string, settle time.Duration, exclude ...string) *harness {
	t.Helper()
	h := &harness{clock: &clock{t: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}}
	h.w = &Watcher{Root: root, Settle: settle, Exclude: exclude, Now: h.clock.now,
		Notice: func(msg string) { h.notices = append(h.notices, msg) }}
	h.w.Ingest = func(ctx context.Context, p string) report.FileResult {
		h.ingested = append(h.ingested, p)
		if h.onIngest != nil {
			return h.onIngest(p)
		}
		return report.FileResult{Path: p}
	}
	return h
}

func (h *harness) pass(t *testing.T) []report.FileResult {
	t.Helper()
	res, err := h.w.Pass(context.Background())
	if err != nil {
		t.Fatalf("pass at %s: %v", h.clock, err)
	}
	return res
}

func (h *harness) count(p string) int {
	n := 0
	for _, q := range h.ingested {
		if q == p {
			n++
		}
	}
	return n
}

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func appendTo(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	// The change must be visible as a new modification time even on a
	// filesystem with coarse timestamps.
	touch(t, path, time.Now().Add(2*time.Second))
}

func touch(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

const settle = 30 * time.Second

// A file whose size or modification time keeps changing is never handed to
// ingest; once it stops changing it is ingested after one settle window
// counted from the last change.
func TestGrowingFileIsNotIngested(t *testing.T) {
	root := t.TempDir()
	p := write(t, filepath.Join(root, "a.nfo"), "1")
	h := newHarness(t, root, settle)
	h.pass(t)
	for i := 0; i < 3; i++ {
		h.clock.advance(settle)
		appendTo(t, p, "x")
		h.pass(t)
		if len(h.ingested) != 0 {
			t.Fatalf("growing file ingested at %s", h.clock)
		}
	}
	// It stopped growing: one settle window after the pass that saw the last
	// version it is ingested, not before.
	h.clock.advance(settle / 2)
	h.pass(t)
	if len(h.ingested) != 0 {
		t.Fatalf("ingested before the settle window passed")
	}
	h.clock.advance(settle / 2)
	h.pass(t)
	if h.count(p) != 1 {
		t.Fatalf("ingested %d times, want 1", h.count(p))
	}
}

// A settled file is ingested once and never again while it stays the same,
// however many passes follow.
func TestSettledFileIsIngestedExactlyOnce(t *testing.T) {
	root := t.TempDir()
	p := write(t, filepath.Join(root, "sub", "a.nfo"), "nfo")
	h := newHarness(t, root, settle)
	if res := h.pass(t); len(res) != 0 {
		t.Fatalf("first pass ingested %v", res)
	}
	h.clock.advance(settle)
	if res := h.pass(t); len(res) != 1 || res[0].Path != p {
		t.Fatalf("second pass results %v", res)
	}
	for i := 0; i < 5; i++ {
		h.clock.advance(settle)
		if res := h.pass(t); len(res) != 0 {
			t.Fatalf("pass %d ingested again: %v", i+3, res)
		}
	}
	if h.count(p) != 1 || h.w.Pending() != 1 {
		t.Errorf("count %d pending %d", h.count(p), h.w.Pending())
	}
}

// A file that changes after it was ingested is a new version: it is
// ingested again, after its own settle window.
func TestChangedFileIsIngestedAgain(t *testing.T) {
	root := t.TempDir()
	p := write(t, filepath.Join(root, "a.nfo"), "nfo")
	h := newHarness(t, root, settle)
	h.pass(t)
	h.clock.advance(settle)
	h.pass(t)
	appendTo(t, p, " more")
	h.clock.advance(settle)
	h.pass(t)
	if h.count(p) != 1 {
		t.Fatalf("re-ingested in the pass that first saw the change")
	}
	h.clock.advance(settle)
	h.pass(t)
	if h.count(p) != 2 {
		t.Fatalf("ingested %d times, want 2", h.count(p))
	}
	// Same size and time but a different file at the name (swapped for a
	// hard link of another file, or restored from a copy that preserves
	// times): the identity differs, so it is a new version too.
	other := write(t, filepath.Join(t.TempDir(), "other.nfo"), "nfo more")
	fi, _ := os.Stat(p)
	touch(t, other, fi.ModTime())
	if err := os.Rename(other, p); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(settle)
	h.pass(t)
	if h.count(p) != 2 {
		t.Fatalf("swapped file ingested before it settled")
	}
	h.clock.advance(settle)
	h.pass(t)
	if h.count(p) != 3 {
		t.Fatalf("ingested %d times, want 3 after the swap", h.count(p))
	}
}

// Guarantee 3: a symbolic link that appears in the tree is never followed
// and never ingested, whatever it points at, and is reported once rather
// than on every pass. A link to a directory is not entered either.
func TestSymlinkIsSkippedAndNoticedOnce(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	target := write(t, filepath.Join(outside, "target.nfo"), "nfo")
	write(t, filepath.Join(outside, "dir", "inner.nfo"), "nfo")
	h := newHarness(t, root, 0)
	h.pass(t)
	link := filepath.Join(root, "link.nfo")
	symlink(t, target, link)
	symlink(t, filepath.Join(outside, "dir"), filepath.Join(root, "dirlink"))
	for i := 0; i < 3; i++ {
		h.pass(t)
	}
	if len(h.ingested) != 0 {
		t.Fatalf("ingested through a link: %v", h.ingested)
	}
	if len(h.notices) != 2 {
		t.Fatalf("notices: %q", h.notices)
	}
	for _, n := range h.notices {
		if !strings.Contains(n, "symbolic link") {
			t.Errorf("notice %q does not name the link", n)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("target touched: %v", err)
	}
}

// A regular file that is renamed away and replaced by another file with the
// same name between two passes is a new file: its settle window starts
// again and the earlier ingest does not count for it.
func TestSwappedFileIsNew(t *testing.T) {
	root := t.TempDir()
	p := write(t, filepath.Join(root, "a.nfo"), "first")
	h := newHarness(t, root, settle)
	h.pass(t)
	h.clock.advance(settle)
	h.pass(t)
	if h.count(p) != 1 {
		t.Fatalf("first version not ingested")
	}
	second := write(t, filepath.Join(root, "b.new"), "second")
	if err := os.Rename(p, filepath.Join(t.TempDir(), "moved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(second, p); err != nil {
		t.Fatal(err)
	}
	h.pass(t)
	if h.count(p) != 1 {
		t.Fatalf("swapped file ingested before it settled")
	}
	h.clock.advance(settle)
	h.pass(t)
	if h.count(p) != 2 {
		t.Fatalf("swapped file ingested %d times, want 2", h.count(p))
	}
}

// Guarantee 3: a directory that is replaced by a symbolic link between two
// passes is not followed: the files below the link are not listed, and the
// entries the watcher remembered below the directory are dropped.
func TestDirectoryReplacedBySymlinkIsNotFollowed(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	p := write(t, filepath.Join(sub, "a.nfo"), "nfo")
	elsewhere := t.TempDir()
	write(t, filepath.Join(elsewhere, "a.nfo"), "nfo")
	h := newHarness(t, root, settle)
	h.pass(t)
	if err := os.RemoveAll(sub); err != nil {
		t.Fatal(err)
	}
	symlink(t, elsewhere, sub)
	h.clock.advance(settle)
	h.pass(t)
	if len(h.ingested) != 0 {
		t.Fatalf("followed the link: %v", h.ingested)
	}
	if h.w.Pending() != 1 {
		t.Errorf("pending %d, want only the link itself", h.w.Pending())
	}
	if _, ok := h.w.seen[p]; ok {
		t.Errorf("%s still remembered below a link", p)
	}
}

// recheck is the last look before the ingest. Each row changes the tree
// after the observation the settle decision was made on and expects the
// check to refuse, so the ingest never runs on a file other than the one
// that settled (guarantees 1 and 3).
func TestRecheckRefusesEveryChange(t *testing.T) {
	cases := []struct {
		name   string
		change func(t *testing.T, root, p string)
	}{
		{"removed", func(t *testing.T, root, p string) { os.Remove(p) }},
		{"appended", func(t *testing.T, root, p string) { appendTo(t, p, "x") }},
		{"swapped for another file", func(t *testing.T, root, p string) {
			other := write(t, filepath.Join(t.TempDir(), "o"), "nfo")
			os.Remove(p)
			if err := os.Rename(other, p); err != nil {
				t.Fatal(err)
			}
		}},
		{"replaced by a symlink", func(t *testing.T, root, p string) {
			target := write(t, filepath.Join(t.TempDir(), "t"), "nfo")
			os.Remove(p)
			symlink(t, target, p)
		}},
		{"replaced by a directory", func(t *testing.T, root, p string) {
			os.Remove(p)
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
		}},
		{"parent replaced by a symlink", func(t *testing.T, root, p string) {
			dir := filepath.Dir(p)
			moved := filepath.Join(t.TempDir(), "moved")
			if err := os.Rename(dir, moved); err != nil {
				t.Fatal(err)
			}
			symlink(t, moved, dir)
		}},
		{"root replaced by a symlink", func(t *testing.T, root, p string) {
			moved := filepath.Join(t.TempDir(), "moved")
			if err := os.Rename(root, moved); err != nil {
				t.Fatal(err)
			}
			symlink(t, moved, root)
		}},
		{"renamed out and a hard link put back", func(t *testing.T, root, p string) {
			// The same inode reached through a different parent: the
			// parent directory is a link now, which is refused before
			// the identity is even compared.
			dir := filepath.Dir(p)
			moved := filepath.Join(t.TempDir(), "moved")
			if err := os.Rename(dir, moved); err != nil {
				t.Fatal(err)
			}
			symlink(t, moved, dir)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			p := write(t, filepath.Join(root, "sub", "a.nfo"), "nfo")
			fi, err := os.Lstat(p)
			if err != nil {
				t.Fatal(err)
			}
			w := &Watcher{Root: root}
			if err := w.recheck(p, version{fi: fi}); err != nil {
				t.Fatalf("recheck refused an unchanged file: %v", err)
			}
			tc.change(t, root, p)
			if err := w.recheck(p, version{fi: fi}); err == nil {
				t.Fatalf("recheck accepted the changed tree")
			}
		})
	}
	t.Run("outside the root", func(t *testing.T) {
		root := t.TempDir()
		p := write(t, filepath.Join(t.TempDir(), "a.nfo"), "nfo")
		fi, _ := os.Lstat(p)
		w := &Watcher{Root: root}
		if err := w.recheck(p, version{fi: fi}); err == nil {
			t.Fatal("recheck accepted a path outside the root")
		}
		if err := w.recheck(root, version{fi: fi}); err == nil {
			t.Fatal("recheck accepted the root itself")
		}
	})
}

// A file that vanishes is dropped from the pending set without a result,
// and a file that vanishes between the settle decision and the ingest is
// dropped silently too; the memory never grows past the tree.
func TestVanishedFilesAreDropped(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, settle)
	var paths []string
	for i := 0; i < 20; i++ {
		paths = append(paths, write(t, filepath.Join(root, "d", "f"+string(rune('a'+i))+".nfo"), "nfo"))
	}
	h.pass(t)
	if h.w.Pending() != 20 {
		t.Fatalf("pending %d", h.w.Pending())
	}
	for _, p := range paths[:10] {
		os.Remove(p)
	}
	h.clock.advance(settle)
	// The rest settle now; the first of them vanishes while an earlier one
	// is being ingested, so it is gone at its recheck.
	h.onIngest = func(p string) report.FileResult {
		os.Remove(paths[11])
		return report.FileResult{Path: p}
	}
	res := h.pass(t)
	if len(res) != 9 {
		t.Fatalf("results %d, want 9: %v", len(res), h.ingested)
	}
	for _, r := range res {
		if r.Path == paths[11] {
			t.Errorf("vanished file %s was ingested", r.Path)
		}
	}
	if h.w.Pending() != 9 {
		t.Errorf("pending %d after pruning, want 9", h.w.Pending())
	}
	os.RemoveAll(filepath.Join(root, "d"))
	h.pass(t)
	if h.w.Pending() != 0 {
		t.Errorf("pending %d after the tree emptied", h.w.Pending())
	}
}

// An unreadable directory is returned once when it appears, not on every
// pass, again when the error changes, and again after it was readable in
// between. The readable files are still ingested.
func TestUnreadableDirectoryReportedOncePerChange(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every directory")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	write(t, filepath.Join(locked, "hidden.nfo"), "nfo")
	open := write(t, filepath.Join(root, "open.nfo"), "nfo")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o755) })
	h := newHarness(t, root, 0)
	_, err := h.w.Pass(context.Background())
	if err == nil || !strings.Contains(err.Error(), "cannot read "+locked) {
		t.Fatalf("first pass error %v", err)
	}
	if h.count(open) != 1 {
		t.Errorf("readable file not ingested alongside the error")
	}
	for i := 0; i < 3; i++ {
		if _, err := h.w.Pass(context.Background()); err != nil {
			t.Fatalf("pass %d repeated the error: %v", i+2, err)
		}
	}
	other := filepath.Join(root, "other")
	write(t, filepath.Join(other, "x.nfo"), "nfo")
	if err := os.Chmod(other, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(other, 0o755) })
	if _, err := h.w.Pass(context.Background()); err == nil || !strings.Contains(err.Error(), other) {
		t.Fatalf("a second unreadable directory was not reported: %v", err)
	}
	os.Chmod(locked, 0o755)
	os.Chmod(other, 0o755)
	if _, err := h.w.Pass(context.Background()); err != nil {
		t.Fatalf("readable again but still an error: %v", err)
	}
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := h.w.Pass(context.Background()); err == nil {
		t.Fatal("the directory became unreadable again and was not reported")
	}
}

// A root that is swapped for a symbolic link, or that is removed, stops the
// walk: nothing is listed, nothing is ingested and the error is returned
// once per change.
func TestRootSwappedForSymlinkIsRefused(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "watched")
	write(t, filepath.Join(root, "a.nfo"), "nfo")
	h := newHarness(t, root, 0)
	moved := filepath.Join(base, "moved")
	if err := os.Rename(root, moved); err != nil {
		t.Fatal(err)
	}
	symlink(t, moved, root)
	_, err := h.w.Pass(context.Background())
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("pass over a link root: %v", err)
	}
	if _, err := h.w.Pass(context.Background()); err != nil {
		t.Fatalf("repeated: %v", err)
	}
	if len(h.ingested) != 0 {
		t.Fatalf("ingested through the link root: %v", h.ingested)
	}
	os.Remove(root)
	if _, err := h.w.Pass(context.Background()); err == nil {
		t.Fatal("a missing root was not reported")
	}
}

// The directories in Exclude are not entered, so a quarantine that sits
// inside the watched tree is not ingested a second time, as in ingest.
func TestExcludedDirectoryIsNotEntered(t *testing.T) {
	root := t.TempDir()
	q := filepath.Join(root, "quarantine")
	write(t, filepath.Join(q, "blocked.exe"), "MZ")
	a := write(t, filepath.Join(root, "a.nfo"), "nfo")
	h := newHarness(t, root, 0, q)
	h.pass(t)
	if len(h.ingested) != 1 || h.ingested[0] != a {
		t.Fatalf("ingested %v", h.ingested)
	}
}

// A file that changes while it is being ingested, whoever changed it, is a
// new version: it is forgotten and ingested again once it settles. That
// holds for amuxify's own rewrite (the result names the path as its output
// or records a cleaner edit) as for a foreign change, so a foreign change
// that lands during an own write is never missed; the second ingest of an
// own rewrite finds nothing to do and settles the file. An output at
// another path (an in-place remux that changed the extension) is a new
// file and is ingested once it settles.
func TestChangedDuringIngestIsIngestedAgain(t *testing.T) {
	root := t.TempDir()
	h := newHarness(t, root, settle)
	own := write(t, filepath.Join(root, "own.mkv"), "x")
	cleaned := write(t, filepath.Join(root, "cleaned.mkv"), "x")
	foreign := write(t, filepath.Join(root, "foreign.mkv"), "x")
	same := write(t, filepath.Join(root, "same.mkv"), "x")
	renamed := write(t, filepath.Join(root, "renamed.mp4"), "x")
	output := filepath.Join(root, "renamed.mkv")
	h.onIngest = func(p string) report.FileResult {
		fr := report.FileResult{Path: p}
		switch p {
		case own:
			appendTo(t, p, "rebuilt")
			fr.Output = p
		case cleaned:
			appendTo(t, p, "edited")
			fr.Addf("METADATA", report.Pass, "removed title")
		case foreign:
			appendTo(t, p, "someone else")
		case renamed:
			if err := os.Rename(p, output); err != nil {
				t.Fatal(err)
			}
			fr.Output = output
		}
		return fr
	}
	h.pass(t)
	h.clock.advance(settle)
	h.pass(t)
	if len(h.ingested) != 5 {
		t.Fatalf("first round ingested %v", h.ingested)
	}
	if _, ok := h.w.seen[renamed]; ok {
		t.Error("the old name of a renamed file is still remembered")
	}
	h.onIngest = nil
	h.pass(t)
	if len(h.ingested) != 5 {
		t.Fatalf("a changed file was ingested before it settled again: %v", h.ingested)
	}
	for i := 0; i < 3; i++ {
		h.clock.advance(settle)
		h.pass(t)
	}
	for _, p := range []string{own, cleaned, foreign} {
		if h.count(p) != 2 {
			t.Errorf("%s ingested %d times, want 2", filepath.Base(p), h.count(p))
		}
	}
	if h.count(same) != 1 {
		t.Errorf("unchanged file ingested %d times, want 1", h.count(same))
	}
	if h.count(output) != 1 || h.count(renamed) != 1 {
		t.Errorf("renamed output ingested %d times, source %d, want 1 and 1", h.count(output), h.count(renamed))
	}
}

// A cancelled context ends the pass between two files: the file in progress
// is finished under a context the cancellation does not reach, so the tool
// working on it is not killed mid-write (guarantee 2), the rest stay pending
// and are ingested by the next pass. A pass that starts with a cancelled
// context ingests nothing.
func TestCancelStopsBetweenFiles(t *testing.T) {
	root := t.TempDir()
	a := write(t, filepath.Join(root, "a.nfo"), "nfo")
	b := write(t, filepath.Join(root, "b.nfo"), "nfo")
	h := newHarness(t, root, 0)
	type key string
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), key("k"), "v"))
	var cancelledDuring []string
	checking := true
	h.w.Ingest = func(ictx context.Context, p string) report.FileResult {
		h.ingested = append(h.ingested, p)
		if !checking {
			return report.FileResult{Path: p}
		}
		cancel()
		// The caller's context is cancelled now; the ingest's own must
		// not be, and it must still carry the caller's values.
		if ictx.Err() != nil {
			cancelledDuring = append(cancelledDuring, p)
		}
		if ictx.Value(key("k")) != "v" {
			t.Errorf("the ingest context lost the caller's values")
		}
		if ctx.Err() == nil {
			t.Errorf("the caller's context is not cancelled")
		}
		return report.FileResult{Path: p}
	}
	res, err := h.w.Pass(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Path != a {
		t.Fatalf("results %v", res)
	}
	if len(cancelledDuring) != 0 {
		t.Fatalf("the file in progress was handed a cancelled context: %v", cancelledDuring)
	}
	res, err = h.w.Pass(ctx)
	if err != nil || len(res) != 0 {
		t.Fatalf("a pass under a cancelled context ingested %v (%v)", res, err)
	}
	if len(h.ingested) != 1 {
		t.Fatalf("ingested %v, want only %s", h.ingested, a)
	}
	checking = false
	res = h.pass(t)
	if len(res) != 1 || res[0].Path != b {
		t.Fatalf("next pass results %v, want %s", res, b)
	}
}
