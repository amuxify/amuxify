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

// CopyIdentity carries mode and mtime from one file to another without
// moving either, so a verified output can take its source's identity before
// it is placed under a new name (guarantee 6).
func TestCopyIdentityCopiesModeAndMtime(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "movie.mp4")
	out := filepath.Join(dir, "movie.mkv")
	writeFile(t, src, "source", 0o644)
	writeFile(t, out, "output", 0o600)
	if err := os.Chmod(src, 0o640); err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2019, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(src, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if err := CopyIdentity(src, out); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(out)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode: got %o want 640", fi.Mode().Perm())
	}
	if got := fi.ModTime().Truncate(time.Second); !got.Equal(stamp) {
		t.Fatalf("mtime: got %v want %v", got, stamp)
	}
	// Neither file moved or changed content.
	if readFile(t, src) != "source" || readFile(t, out) != "output" {
		t.Fatal("CopyIdentity touched file content")
	}
	// A missing source is an error and leaves the target alone.
	if err := CopyIdentity(filepath.Join(dir, "missing"), out); err == nil {
		t.Fatal("CopyIdentity accepted a missing source")
	}
	if fi2, _ := os.Stat(out); fi2 == nil || fi2.Mode().Perm() != fi.Mode().Perm() {
		t.Fatal("target changed after a failed CopyIdentity")
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

// Guarantee 4: output and quarantine placement never leave the tree the
// user named. MkdirAllUnder creates the requested path component by
// component and refuses any planted symlink on the way.
func TestMkdirAllUnderCreatesDeepPaths(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "a", "b", "c", "d")
	if err := MkdirAllUnder(root, dir); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(dir)
	if err != nil || !fi.IsDir() {
		t.Fatalf("dir not created: %v", err)
	}
	// Calling it again on an existing path is a no-op.
	if err := MkdirAllUnder(root, dir); err != nil {
		t.Fatal(err)
	}
	// root itself is allowed and creates nothing.
	if err := MkdirAllUnder(root, root); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAllUnder(root, root+string(filepath.Separator)); err != nil {
		t.Fatal(err)
	}
}

func TestMkdirAllUnderRefusesSymlinkedComponent(t *testing.T) {
	root := t.TempDir()
	elsewhere := t.TempDir()
	if err := os.Symlink(elsewhere, filepath.Join(root, "sub")); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	err := MkdirAllUnder(root, filepath.Join(root, "sub", "deeper"))
	if err == nil {
		t.Fatal("expected refusal for a symlinked intermediate directory")
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error does not name the symlink: %v", err)
	}
	if exists(filepath.Join(elsewhere, "deeper")) {
		t.Fatal("directory was created through the symlink, outside root")
	}
	// The symlink itself as the target is refused too.
	if err := MkdirAllUnder(root, filepath.Join(root, "sub")); err == nil {
		t.Fatal("expected refusal when the target is a symlink")
	}
	// A symlink deeper in an otherwise real chain is refused as well, and
	// the real components before it are left as they are.
	if err := os.MkdirAll(filepath.Join(root, "real", "chain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(root, "real", "chain", "link")); err != nil {
		t.Fatal(err)
	}
	if err := MkdirAllUnder(root, filepath.Join(root, "real", "chain", "link", "x", "y")); err == nil {
		t.Fatal("expected refusal for a symlink deep in the chain")
	}
	if exists(filepath.Join(elsewhere, "x")) {
		t.Fatal("directory was created through the deep symlink")
	}
}

func TestMkdirAllUnderRefusesFileComponent(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "sub"), "not a dir", 0o644)
	err := MkdirAllUnder(root, filepath.Join(root, "sub", "deeper"))
	if err == nil {
		t.Fatal("expected refusal when a file sits where a directory is needed")
	}
	if readFile(t, filepath.Join(root, "sub")) != "not a dir" {
		t.Fatal("the file in the way was modified")
	}
	if err := MkdirAllUnder(root, filepath.Join(root, "sub")); err == nil {
		t.Fatal("expected refusal when the target itself is a file")
	}
}

func TestMkdirAllUnderRefusesEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(root)
	bad := []string{
		filepath.Join(root, "..", "escaped"),
		filepath.Join(root, "a", "..", "..", "escaped"),
		filepath.Join(parent, "escaped"),
		filepath.Join(root + "sibling"),
		filepath.Dir(parent),
		string(filepath.Separator),
	}
	for _, dir := range bad {
		if err := MkdirAllUnder(root, dir); err == nil {
			t.Errorf("%s: expected refusal", dir)
		}
	}
	if exists(filepath.Join(parent, "escaped")) || exists(root+"sibling") {
		t.Fatal("a directory was created outside root")
	}
	// A missing root is the user's chosen tree and is created for them.
	if err := MkdirAllUnder(filepath.Join(parent, "missing"), filepath.Join(parent, "missing", "x")); err != nil {
		t.Fatalf("missing root: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(parent, "missing", "x")); err != nil || !fi.IsDir() {
		t.Fatal("missing root was not created")
	}
	// A root that is a regular file is an error.
	writeFile(t, filepath.Join(parent, "file"), "x", 0o644)
	if err := MkdirAllUnder(filepath.Join(parent, "file"), filepath.Join(parent, "file", "x")); err == nil {
		t.Fatal("expected error for a file root")
	}
}

// The root itself may be a symlink: the user chose it, and the checks
// protect the components below it. This documents the choice.
func TestMkdirAllUnderAllowsSymlinkedRoot(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("symlinks not supported:", err)
	}
	if err := MkdirAllUnder(link, filepath.Join(link, "a", "b")); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Lstat(filepath.Join(real, "a", "b")); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created under the real root: %v", err)
	}
}

func TestMkdirAllUnderHostileNames(t *testing.T) {
	root := t.TempDir()
	names := []string{
		"..hidden", "a..b", " leading", "trailing ", "with;semicolon",
		"$(echo pwned)", "`id`", "-flag", "‮exe.mkv", "zero​width",
		"日本語", "émoji🎬", "quote'and\"double", "star*and?glob", "pipe|and&amp",
	}
	for _, n := range names {
		dir := filepath.Join(root, n, "inner")
		if err := MkdirAllUnder(root, dir); err != nil {
			t.Errorf("%q: %v", n, err)
			continue
		}
		if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
			t.Errorf("%q: not created", n)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != len(names) {
		t.Errorf("expected %d entries under root, found %d", len(names), len(entries))
	}
}
