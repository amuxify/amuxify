package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// Guarantee 1: an existing destination is never replaced.
func TestPlaceNoClobberRefusesExisting(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, ".amuxify-out.mkv.tmp")
	dest := filepath.Join(dir, "out.mkv")
	writeFile(t, tmp, "new bytes", 0o644)
	writeFile(t, dest, "old bytes", 0o644)

	err := PlaceNoClobber(tmp, dest)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("PlaceNoClobber over an existing file: got %v, want ErrExists", err)
	}
	if !exists(tmp) {
		t.Fatal("temp file was removed although the destination was refused")
	}
	if got := readFile(t, dest); got != "old bytes" {
		t.Fatalf("destination changed to %q", got)
	}
}

func TestPlaceNoClobberMoves(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, ".amuxify-out.mkv.tmp")
	dest := filepath.Join(dir, "out.mkv")
	writeFile(t, tmp, "payload", 0o644)

	if err := PlaceNoClobber(tmp, dest); err != nil {
		t.Fatal(err)
	}
	if exists(tmp) {
		t.Fatal("temp file still present after placement")
	}
	if got := readFile(t, dest); got != "payload" {
		t.Fatalf("destination holds %q", got)
	}
}

// A dangling symlink at the destination is still "something there": placing
// over it would write through to wherever the link points.
func TestPlaceNoClobberRefusesDanglingSymlinkDest(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, ".amuxify-out.mkv.tmp")
	dest := filepath.Join(dir, "out.mkv")
	outside := filepath.Join(t.TempDir(), "victim.mkv")
	writeFile(t, tmp, "payload", 0o644)
	if err := os.Symlink(outside, dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	err := PlaceNoClobber(tmp, dest)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("PlaceNoClobber over a dangling symlink: got %v, want ErrExists", err)
	}
	if exists(outside) {
		t.Fatal("placement followed the symlink and created the target outside the output directory")
	}
	if !exists(tmp) {
		t.Fatal("temp file lost")
	}
}

// A symlink pointing at a real file must not let the placement replace the
// link's target.
func TestPlaceNoClobberRefusesSymlinkToFileDest(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, ".amuxify-out.mkv.tmp")
	dest := filepath.Join(dir, "out.mkv")
	victim := filepath.Join(t.TempDir(), "victim.mkv")
	writeFile(t, tmp, "payload", 0o644)
	writeFile(t, victim, "precious", 0o644)
	if err := os.Symlink(victim, dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := PlaceNoClobber(tmp, dest); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	if got := readFile(t, victim); got != "precious" {
		t.Fatalf("symlink target overwritten: %q", got)
	}
}

// A destination whose name is a directory is refused as well.
func TestPlaceNoClobberRefusesDirectoryDest(t *testing.T) {
	dir := t.TempDir()
	tmp := filepath.Join(dir, ".amuxify-out.mkv.tmp")
	dest := filepath.Join(dir, "out.mkv")
	writeFile(t, tmp, "payload", 0o644)
	if err := os.Mkdir(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := PlaceNoClobber(tmp, dest); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	if !exists(tmp) {
		t.Fatal("temp file lost")
	}
}

// Two placements racing for the same name: exactly one wins and the loser
// keeps its temp file.
func TestPlaceNoClobberOnlyOneWinner(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.mkv")
	a := filepath.Join(dir, ".amuxify-a.tmp")
	b := filepath.Join(dir, ".amuxify-b.tmp")
	writeFile(t, a, "A", 0o644)
	writeFile(t, b, "B", 0o644)
	errA := PlaceNoClobber(a, dest)
	errB := PlaceNoClobber(b, dest)
	if errA != nil {
		t.Fatalf("first placement failed: %v", errA)
	}
	if !errors.Is(errB, ErrExists) {
		t.Fatalf("second placement: got %v, want ErrExists", errB)
	}
	if got := readFile(t, dest); got != "A" {
		t.Fatalf("destination holds %q, want the first writer's bytes", got)
	}
	if !exists(b) {
		t.Fatal("loser's temp file was removed")
	}
}

// Guarantee 2: output is written to a hidden sibling first.
func TestTempNameIsHiddenSibling(t *testing.T) {
	got := TempName(filepath.FromSlash("/a/b/c.mkv"))
	want := filepath.FromSlash("/a/b/.amuxify-c.mkv.tmp")
	if got != want {
		t.Fatalf("TempName: got %q want %q", got, want)
	}
	if !strings.HasPrefix(filepath.Base(got), ".amuxify-") || !strings.HasSuffix(got, ".tmp") {
		t.Fatalf("temp name %q is not hidden or not suffixed .tmp", got)
	}
}

// The temp name always stays in the destination's own directory, whatever the
// destination path looks like.
func TestTempNameStaysInDestinationDir(t *testing.T) {
	cases := []string{
		filepath.FromSlash("/a/b/../c.mkv"),
		filepath.FromSlash("/a/b/./c.mkv"),
		filepath.FromSlash("/a/b/c d.mkv"),
		filepath.FromSlash("/a/b/-rf.mkv"),
		filepath.FromSlash("/a/b/..mkv"),
		filepath.FromSlash("/a/b/$(id).mkv"),
		filepath.FromSlash("/a/b/‮mkv.exe"),
		"rel.mkv",
	}
	for _, dest := range cases {
		tmp := TempName(dest)
		wantDir := filepath.Dir(filepath.Clean(dest))
		if filepath.Dir(tmp) != wantDir {
			t.Errorf("TempName(%q) = %q, escapes %q", dest, tmp, wantDir)
		}
		if !strings.HasPrefix(filepath.Base(tmp), ".amuxify-") {
			t.Errorf("TempName(%q) = %q is not hidden", dest, tmp)
		}
		if tmp == dest {
			t.Errorf("TempName(%q) equals the destination", dest)
		}
	}
}

// Guarantee 6: in-place replacement keeps mode and mtime.
func TestReplaceInPlacePreservesModeAndMtime(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.mkv")
	tmp := TempName(dest)
	writeFile(t, dest, "original", 0o644)
	writeFile(t, tmp, "replacement", 0o600)
	if err := os.Chmod(dest, 0o640); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chtimes(dest, stamp, stamp); err != nil {
		t.Fatal(err)
	}

	if err := ReplaceInPlace(tmp, dest); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode: got %o want 640", fi.Mode().Perm())
	}
	if got := fi.ModTime().Truncate(time.Second); !got.Equal(stamp) {
		t.Fatalf("mtime: got %v want %v", got, stamp)
	}
	if got := readFile(t, dest); got != "replacement" {
		t.Fatalf("content: %q", got)
	}
	if exists(tmp) {
		t.Fatal("temp file still present")
	}
}

// ReplaceInPlace is only for files that exist; a missing destination must not
// turn into a plain create, because the caller never verified against it.
func TestReplaceInPlaceRefusesMissingDest(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.mkv")
	tmp := TempName(dest)
	writeFile(t, tmp, "replacement", 0o600)
	if err := ReplaceInPlace(tmp, dest); err == nil {
		t.Fatal("ReplaceInPlace created a file that did not exist")
	}
	if exists(dest) {
		t.Fatal("destination appeared")
	}
	if !exists(tmp) {
		t.Fatal("temp file lost")
	}
}

// When the destination name is a symlink, the rename replaces the link, not
// the file the link points at.
func TestReplaceInPlaceNeverWritesThroughSymlink(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.mkv")
	writeFile(t, victim, "precious", 0o644)
	dest := filepath.Join(dir, "movie.mkv")
	if err := os.Symlink(victim, dest); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	tmp := TempName(dest)
	writeFile(t, tmp, "replacement", 0o600)
	_ = ReplaceInPlace(tmp, dest)
	if got := readFile(t, victim); got != "precious" {
		t.Fatalf("symlink target overwritten: %q", got)
	}
}

// Guarantee 3: hard links are counted so callers can refuse to break them.
func TestNlinkCountsHardLinks(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mkv")
	b := filepath.Join(dir, "b.mkv")
	writeFile(t, a, "x", 0o644)
	fi, err := os.Lstat(a)
	if err != nil {
		t.Fatal(err)
	}
	if n := Nlink(fi); n != 1 {
		t.Fatalf("fresh file: nlink %d", n)
	}
	if err := os.Link(a, b); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	fi, err = os.Lstat(a)
	if err != nil {
		t.Fatal(err)
	}
	if n := Nlink(fi); n != 2 {
		t.Fatalf("after link: nlink %d want 2", n)
	}
}

type fakeInfo struct{ os.FileInfo }

func (fakeInfo) Sys() interface{} { return nil }

func TestNlinkWithoutStatIsOne(t *testing.T) {
	if n := Nlink(fakeInfo{}); n != 1 {
		t.Fatalf("Nlink without Stat_t: %d", n)
	}
}

func TestIsSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.mkv")
	link := filepath.Join(dir, "link.mkv")
	dangling := filepath.Join(dir, "dangling.mkv")
	writeFile(t, target, "x", 0o644)
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), dangling); err != nil {
		t.Fatal(err)
	}
	if ok, err := IsSymlink(target); err != nil || ok {
		t.Fatalf("regular file: %v %v", ok, err)
	}
	if ok, err := IsSymlink(link); err != nil || !ok {
		t.Fatalf("symlink: %v %v", ok, err)
	}
	if ok, err := IsSymlink(dangling); err != nil || !ok {
		t.Fatalf("dangling symlink must still count as a symlink: %v %v", ok, err)
	}
	if _, err := IsSymlink(filepath.Join(dir, "absent")); err == nil {
		t.Fatal("missing path reported no error")
	}
}

// Guarantee 10: the root check goes through the replaceable seam.
func TestIsRootUsesGeteuid(t *testing.T) {
	orig := Geteuid
	t.Cleanup(func() { Geteuid = orig })
	Geteuid = func() int { return 0 }
	if !IsRoot() {
		t.Fatal("euid 0 not reported as root")
	}
	Geteuid = func() int { return 1000 }
	if IsRoot() {
		t.Fatal("euid 1000 reported as root")
	}
	Geteuid = func() int { return -1 }
	if IsRoot() {
		t.Fatal("unknown euid reported as root")
	}
}

func TestExtLowercasesAndStripsDot(t *testing.T) {
	cases := map[string]string{
		"a.MKV":         "mkv",
		"a.tar.gz":      "gz",
		"noext":         "",
		"dir.d/file":    "",
		".hidden":       "hidden",
		"trailingdot.":  "",
		"x.mkv.exe":     "exe",
		"x.‮exe":        "‮exe",
		"spaces .M p 4": "m p 4",
	}
	for in, want := range cases {
		if got := Ext(filepath.FromSlash(in)); got != want {
			t.Errorf("Ext(%q) = %q want %q", in, got, want)
		}
	}
}

func TestSameDevice(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a")
	b := filepath.Join(dir, "b")
	writeFile(t, a, "x", 0o644)
	writeFile(t, b, "y", 0o644)
	same, err := SameDevice(a, b)
	if err != nil || !same {
		t.Fatalf("siblings: same=%v err=%v", same, err)
	}
	if _, err := SameDevice(a, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing path reported no error")
	}
}

func TestFsync(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	writeFile(t, p, "x", 0o644)
	if err := Fsync(p); err != nil {
		t.Fatal(err)
	}
	if err := Fsync(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing path reported no error")
	}
}

func TestAbsCleansTraversal(t *testing.T) {
	got, err := Abs(filepath.FromSlash("a/../b/./c"))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) || strings.Contains(got, "..") {
		t.Fatalf("Abs did not clean: %q", got)
	}
}
