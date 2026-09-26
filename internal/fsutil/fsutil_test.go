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

// identityOfPath returns the mode and modification time of the entry at path
// without following a symbolic link, so a test can prove that a victim behind
// a planted link kept both.
func identityOfPath(t *testing.T, path string) (os.FileMode, time.Time) {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Mode().Perm(), fi.ModTime()
}

// Guarantee 3: CopyIdentityTo writes through the descriptor it was handed.
// The file is opened, then its name is replaced by a symbolic link to a
// victim with a different mode and time. The copy must land on the file
// behind the descriptor and the victim behind the link must keep its mode
// and its modification time.
func TestCopyIdentityToNeverFollowsSymlinkAtFormerName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits and symlinks differ on windows")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "source.mp4")
	writeFile(t, src, "source", 0o666)
	if err := os.Chmod(src, 0o666); err != nil {
		t.Fatal(err)
	}
	srcStamp := time.Date(2010, 11, 12, 13, 14, 15, 0, time.UTC)
	if err := os.Chtimes(src, srcStamp, srcStamp); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim.mkv")
	writeFile(t, victim, "precious", 0o600)
	victimStamp := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(victim, victimStamp, victimStamp); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".amuxify-source.mkv.tmp")
	writeFile(t, tmp, "output", 0o640)
	f, err := os.OpenFile(tmp, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// The swap: the open file loses its name and a link to the victim takes
	// the name's place. A path-based copy would now reach the victim.
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, tmp); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := CopyIdentityTo(f, src); err != nil {
		t.Fatal(err)
	}
	if mode, mtime := identityOfPath(t, victim); mode != 0o600 || !mtime.Equal(victimStamp) {
		t.Fatalf("victim behind the planted link changed: mode %o mtime %v", mode, mtime)
	}
	if got := readFile(t, victim); got != "precious" {
		t.Fatalf("victim rewritten: %q", got)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o666 {
		t.Fatalf("the open file did not take the source's mode: %o", fi.Mode().Perm())
	}
	if got := fi.ModTime().Truncate(time.Second); !got.Equal(srcStamp) {
		t.Fatalf("the open file did not take the source's mtime: %v", got)
	}
	// The link itself is still the planted link, untouched.
	if target, err := os.Readlink(tmp); err != nil || target != victim {
		t.Fatalf("the planted link was replaced: %q %v", target, err)
	}
	// A missing source is an error and the open file is left alone.
	if err := CopyIdentityTo(f, filepath.Join(dir, "missing")); err == nil {
		t.Fatal("CopyIdentityTo accepted a missing source")
	}
	if fi2, _ := f.Stat(); fi2 == nil || fi2.Mode().Perm() != 0o666 {
		t.Fatal("the open file changed after a failed CopyIdentityTo")
	}
}

// OpenOwn refuses every entry an attacker could leave at a temp name: a
// symbolic link is not followed, a hard link to a victim is refused for its
// extra name, a different regular file is refused against the recorded
// identity, and a directory or a missing entry is an error. Its own file is
// returned open for writing.
func TestOpenOwnRefusesSwappedEntries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("O_NOFOLLOW and hard links differ on windows")
	}
	dir := t.TempDir()
	own := filepath.Join(dir, "own")
	writeFile(t, own, "own", 0o644)
	created, err := os.Lstat(own)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	writeFile(t, victim, "victim", 0o600)

	f, err := OpenOwn(own, created)
	if err != nil {
		t.Fatalf("own file refused: %v", err)
	}
	if _, err := f.Write([]byte("more")); err != nil {
		t.Fatalf("own file not open for writing: %v", err)
	}
	f.Close()
	if f, err := OpenOwn(own, nil); err != nil {
		t.Fatalf("own file refused without a recorded identity: %v", err)
	} else {
		f.Close()
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, rec := range []os.FileInfo{created, nil} {
		f, err := OpenOwn(link, rec)
		if err == nil {
			f.Close()
			t.Fatal("a symlink was opened")
		}
		if !strings.Contains(err.Error(), "symlink") {
			t.Fatalf("symlink: %v", err)
		}
	}

	hard := filepath.Join(dir, "hard")
	if err := os.Link(victim, hard); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []os.FileInfo{created, nil} {
		f, err := OpenOwn(hard, rec)
		if err == nil {
			f.Close()
			t.Fatal("a hard link to the victim was opened")
		}
		if !strings.Contains(err.Error(), "hard links") && !strings.Contains(err.Error(), "not the file this run created") {
			t.Fatalf("hard link: %v", err)
		}
	}

	f, err = OpenOwn(victim, created)
	if err == nil {
		f.Close()
		t.Fatal("a different regular file was accepted against the recorded identity")
	}
	if !strings.Contains(err.Error(), "not the file this run created") {
		t.Fatalf("foreign file: %v", err)
	}

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenOwn(sub, nil); err == nil {
		f.Close()
		t.Fatal("a directory was accepted")
	}
	if f, err := OpenOwn(filepath.Join(dir, "missing"), created); err == nil {
		f.Close()
		t.Fatal("a missing entry was accepted")
	}
	if got := readFile(t, victim); got != "victim" {
		t.Fatalf("victim rewritten: %q", got)
	}
}

// Guarantee 3 for ReplaceInPlace: when a symbolic link to a victim sits at
// the temp name, nothing is written to the victim, its mode and time stay,
// the destination is not replaced and the link is left where it was found.
// With a recorded identity, a regular file swapped onto the temp name is
// refused as well.
func TestReplaceInPlaceRefusesSwappedTemp(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits and symlinks differ on windows")
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.mkv")
	writeFile(t, dest, "original", 0o666)
	if err := os.Chmod(dest, 0o666); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(t.TempDir(), "victim.mkv")
	writeFile(t, victim, "precious", 0o600)
	victimStamp := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(victim, victimStamp, victimStamp); err != nil {
		t.Fatal(err)
	}
	tmp := TempName(dest)
	if err := os.Symlink(victim, tmp); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := ReplaceInPlace(tmp, dest); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("ReplaceInPlace with a symlink at the temp name: %v", err)
	}
	if mode, mtime := identityOfPath(t, victim); mode != 0o600 || !mtime.Equal(victimStamp) {
		t.Fatalf("victim behind the planted link changed: mode %o mtime %v", mode, mtime)
	}
	if got := readFile(t, victim); got != "precious" {
		t.Fatalf("victim rewritten: %q", got)
	}
	if got := readFile(t, dest); got != "original" {
		t.Fatalf("destination replaced: %q", got)
	}
	if fi, err := os.Lstat(dest); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("destination is no longer a regular file: %v", err)
	}
	if target, err := os.Readlink(tmp); err != nil || target != victim {
		t.Fatalf("the planted link was removed or replaced: %q %v", target, err)
	}

	// A regular file swapped onto the temp name is refused against the
	// identity recorded at creation and the destination stays.
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	writeFile(t, tmp, "mine", 0o644)
	created, err := os.Lstat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	writeFile(t, tmp, "foreign", 0o644)
	err = ReplaceInPlaceOwn(tmp, dest, created)
	if err == nil || !strings.Contains(err.Error(), "not the file this run created") {
		t.Fatalf("ReplaceInPlaceOwn with a foreign file at the temp name: %v", err)
	}
	if got := readFile(t, dest); got != "original" {
		t.Fatalf("destination replaced by the foreign file: %q", got)
	}
	if got := readFile(t, tmp); got != "foreign" {
		t.Fatalf("the foreign file was removed or changed: %q", got)
	}

	// The run's own file passes and carries the destination's identity.
	if err := os.Remove(tmp); err != nil {
		t.Fatal(err)
	}
	writeFile(t, tmp, "replacement", 0o600)
	created, err = os.Lstat(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReplaceInPlaceOwn(tmp, dest, created); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, dest); got != "replacement" {
		t.Fatalf("destination: %q", got)
	}
	if mode, _ := identityOfPath(t, dest); mode != 0o666 {
		t.Fatalf("the replacement did not take the destination's mode: %o", mode)
	}
}

// The rename in ReplaceInPlaceOwn uses the temp name once more after the
// descriptor is closed. A symlink swapped onto that name in that instant is
// renamed over the destination; the outcome is reported as an error that
// says the file is gone, the planted entry is left in place rather than
// removed, and the link's target is never followed or changed.
func TestReplaceInPlaceOwnReportsSwapBeforeRename(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits and symlinks differ on windows")
	}
	dir := t.TempDir()
	dest := filepath.Join(dir, "movie.mkv")
	writeFile(t, dest, "original", 0o666)
	victim := filepath.Join(t.TempDir(), "victim.mkv")
	writeFile(t, victim, "precious", 0o600)
	victimStamp := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(victim, victimStamp, victimStamp); err != nil {
		t.Fatal(err)
	}
	tmp := TempName(dest)
	created, err := CreateTemp(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tmp, []byte("rebuilt"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		plant func()
		check func()
	}{
		{"symlink", func() {
			if err := os.Symlink(victim, tmp); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
		}, func() {
			if target, err := os.Readlink(dest); err != nil || target != victim {
				t.Fatalf("the planted link was not left at the destination: %q %v", target, err)
			}
		}},
		{"foreign file", func() { writeFile(t, tmp, "foreign", 0o644) }, func() {
			if got := readFile(t, dest); got != "foreign" {
				t.Fatalf("the foreign file was not left at the destination: %q", got)
			}
			if fi, err := os.Lstat(dest); err != nil || Nlink(fi) != 1 {
				t.Fatalf("foreign file removed or linked: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writeFile(t, dest, "original", 0o666)
			fired := false
			beforeRename = func(gotTmp, gotDest string) {
				fired = true
				if gotTmp != tmp || gotDest != dest {
					t.Fatalf("seam saw %q %q", gotTmp, gotDest)
				}
				if err := os.Remove(tmp); err != nil {
					t.Fatal(err)
				}
				tc.plant()
			}
			t.Cleanup(func() { beforeRename = nil })
			err := ReplaceInPlaceOwn(tmp, dest, created)
			if !fired {
				t.Fatal("the seam never ran")
			}
			if err == nil || !strings.Contains(err.Error(), "not the file this run created") || !strings.Contains(err.Error(), "left in place") {
				t.Fatalf("ReplaceInPlaceOwn after a swap before the rename: %v", err)
			}
			if mode, mtime := identityOfPath(t, victim); mode != 0o600 || !mtime.Equal(victimStamp) {
				t.Fatalf("victim changed: mode %o mtime %v", mode, mtime)
			}
			if got := readFile(t, victim); got != "precious" {
				t.Fatalf("victim rewritten: %q", got)
			}
			if fi, err := os.Lstat(victim); err != nil || Nlink(fi) != 1 {
				t.Fatalf("victim gained a name: %v", err)
			}
			tc.check()
			if _, err := os.Lstat(tmp); err == nil {
				t.Fatal("the temp name still exists")
			}
			// The file this run built is gone with the swap; recreate it
			// for the next row.
			if _, err := os.Lstat(dest); err == nil {
				_ = os.Remove(dest)
			}
			var cerr error
			created, cerr = CreateTemp(tmp)
			if cerr != nil {
				t.Fatal(cerr)
			}
			if err := os.WriteFile(tmp, []byte("rebuilt"), 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}
