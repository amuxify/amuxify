package fsutil

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// forceCrossDevice makes the same-device move fail with EXDEV, the error
// link(2) and rename(2) return across filesystems, so the copy path runs.
func forceCrossDevice(t *testing.T) {
	t.Helper()
	orig := Place
	Place = func(src, dest string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dest, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { Place = orig })
}

// noTempFiles fails when anything other than the named files is left in dir.
func onlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("%s holds %v, want %v", dir, got, want)
	}
}

func TestMoveNoClobberSameDeviceIsPlace(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mkv")
	dest := filepath.Join(dir, "dest.mkv")
	writeFile(t, src, "payload", 0o644)
	if err := MoveNoClobber(src, dest); err != nil {
		t.Fatal(err)
	}
	if exists(src) || readFile(t, dest) != "payload" {
		t.Fatal("same-device move did not move the file")
	}
	// An existing destination is refused and both files stay.
	writeFile(t, src, "second", 0o644)
	if err := MoveNoClobber(src, dest); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	if readFile(t, src) != "second" || readFile(t, dest) != "payload" {
		t.Fatal("a refused move changed a file")
	}
}

// Across filesystems the bytes are copied, verified and the source removed.
func TestMoveNoClobberCrossDeviceCopies(t *testing.T) {
	forceCrossDevice(t)
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	body := strings.Repeat("payload bytes\n", 100000)
	writeFile(t, src, body, 0o755)
	if err := MoveNoClobber(src, dest); err != nil {
		t.Fatal(err)
	}
	if exists(src) {
		t.Error("source still present")
	}
	if readFile(t, dest) != body {
		t.Error("copy differs from the source")
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("copy mode %o, want 0600", fi.Mode().Perm())
	}
	onlyFiles(t, from)
	onlyFiles(t, to, "dest.mkv")
}

// Guarantee 1 across filesystems: a destination that exists is refused and
// both files are left intact.
func TestMoveNoClobberCrossDeviceRefusesExisting(t *testing.T) {
	forceCrossDevice(t)
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, src, "new bytes", 0o644)
	writeFile(t, dest, "old bytes", 0o644)
	if err := MoveNoClobber(src, dest); !errors.Is(err, ErrExists) {
		t.Fatalf("got %v, want ErrExists", err)
	}
	if readFile(t, src) != "new bytes" || readFile(t, dest) != "old bytes" {
		t.Fatal("a refused move changed a file")
	}
}

// A symlink at the destination, dangling or pointing at a victim, is never
// followed: O_EXCL refuses it, the victim keeps its bytes and the source
// stays where it was.
func TestMoveNoClobberCrossDeviceRefusesSymlinkDest(t *testing.T) {
	forceCrossDevice(t)
	from, to := t.TempDir(), t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.mkv")
	writeFile(t, victim, "precious", 0o644)
	for name, target := range map[string]string{"dangling": filepath.Join(to, "missing"), "victim": victim} {
		t.Run(name, func(t *testing.T) {
			src := filepath.Join(from, name+".mkv")
			dest := filepath.Join(to, name+".link")
			writeFile(t, src, "payload", 0o644)
			if err := os.Symlink(target, dest); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			err := MoveNoClobber(src, dest)
			if !errors.Is(err, ErrExists) {
				t.Fatalf("got %v, want ErrExists", err)
			}
			if readFile(t, src) != "payload" {
				t.Error("source changed")
			}
			if readFile(t, victim) != "precious" {
				t.Error("victim overwritten through the symlink")
			}
			if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Error("the symlink at dest was replaced or removed")
			}
			if exists(filepath.Join(to, "missing")) {
				t.Error("the dangling link's target was created")
			}
		})
	}
}

// A directory at the destination, or a missing parent, is an error that
// leaves the source alone.
func TestMoveNoClobberCrossDeviceBadDestinations(t *testing.T) {
	forceCrossDevice(t)
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	writeFile(t, src, "payload", 0o644)
	for _, dest := range []string{to, filepath.Join(to, "no", "such", "dir", "x.mkv")} {
		if err := MoveNoClobber(src, dest); err == nil {
			t.Errorf("%s: expected an error", dest)
		}
		if readFile(t, src) != "payload" {
			t.Fatalf("%s: source changed", dest)
		}
	}
	onlyFiles(t, to)
}

// nlink returns the hard-link count of path.
func nlink(t *testing.T, path string) uint64 {
	t.Helper()
	fi, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return Nlink(fi)
}

// A source that is a symlink is never followed, whichever way the move
// runs: on one filesystem, where link(2) would follow it on macOS and turn
// dest into a hard link to the victim; across filesystems; and in the copy
// step on its own, which has its own guard.
func TestMoveNoClobberRefusesSymlinkSource(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T)
		move  func(src, dest string) error
	}{
		{"same device", func(*testing.T) {}, MoveNoClobber},
		{"cross device", forceCrossDevice, MoveNoClobber},
		{"copy step alone", func(*testing.T) {}, copyThenRemove},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.setup(t)
			from, to := t.TempDir(), t.TempDir()
			victim := filepath.Join(t.TempDir(), "victim.mkv")
			writeFile(t, victim, "precious", 0o644)
			src := filepath.Join(from, "src.mkv")
			if err := os.Symlink(victim, src); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			dest := filepath.Join(to, "dest.mkv")
			err := tc.move(src, dest)
			if err == nil || !strings.Contains(err.Error(), "not a regular file") {
				t.Fatalf("a symlinked source was moved: %v", err)
			}
			if exists(dest) {
				t.Error("destination created from a symlinked source")
			}
			if fi, err := os.Lstat(src); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Error("the source symlink was removed")
			}
			if readFile(t, victim) != "precious" || nlink(t, victim) != 1 {
				t.Error("victim changed or gained a link")
			}
		})
	}
}

// Sources that are not files at all are refused on the same-device path
// before anything is linked: a directory, and a dangling symlink.
func TestMoveNoClobberRefusesNonRegularSource(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	dir := filepath.Join(from, "dir.mkv")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	dangling := filepath.Join(from, "dangling.mkv")
	if err := os.Symlink(filepath.Join(from, "missing"), dangling); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, src := range []string{dir, dangling} {
		dest := filepath.Join(to, filepath.Base(src))
		err := MoveNoClobber(src, dest)
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("%s: got %v", src, err)
		}
		if exists(dest) {
			t.Errorf("%s: destination created", src)
		}
		if _, err := os.Lstat(src); err != nil {
			t.Errorf("%s: source removed", src)
		}
	}
	onlyFiles(t, to)
}

// The race the Lstat cannot close on its own: between the check and the
// link, src is replaced by a symlink to a victim file. On macOS link(2)
// follows the symlink, so dest becomes a second name for the victim and the
// symlink is unlinked; on Linux dest becomes a hard link to the symlink. In
// both cases the identity check after the move sees that dest is not the
// file src was, removes it and reports the change. The victim keeps its
// bytes and its link count, and nothing is left at dest.
func TestMoveNoClobberSourceSwappedForSymlinkBeforeLink(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim.mkv")
	writeFile(t, victim, "precious", 0o644)
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, src, "payload", 0o644)
	orig := Place
	Place = func(s, d string) error {
		if err := os.Remove(s); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, s); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return PlaceNoClobber(s, d)
	}
	t.Cleanup(func() { Place = orig })
	err := MoveNoClobber(src, dest)
	if err == nil || !strings.Contains(err.Error(), "changed during the move") {
		t.Fatalf("got %v, want a refusal", err)
	}
	if _, err := os.Lstat(dest); err == nil {
		t.Error("an entry was left at dest after the swap")
	}
	if readFile(t, victim) != "precious" {
		t.Error("victim bytes changed")
	}
	if n := nlink(t, victim); n != 1 {
		t.Errorf("victim has %d links, want 1", n)
	}
	onlyFiles(t, to)
}

// The same race with a different regular file renamed onto src, as someone
// who can already rename a victim onto the path might do: the entry placed
// at dest is not the file that was checked, so the move is reported as
// failed rather than quietly accepted. After the link and the unlink of
// src, dest is that file's only name, so it is kept rather than removed:
// the discard must never turn a failed move into a delete.
func TestMoveNoClobberSourceSwappedForOtherFileBeforeLink(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, src, "payload", 0o644)
	orig := Place
	Place = func(s, d string) error {
		victim := filepath.Join(from, "victim.mkv")
		writeFile(t, victim, "renamed onto src", 0o644)
		if err := os.Rename(victim, s); err != nil {
			t.Fatal(err)
		}
		return PlaceNoClobber(s, d)
	}
	t.Cleanup(func() { Place = orig })
	err := MoveNoClobber(src, dest)
	if err == nil || !strings.Contains(err.Error(), "changed during the move") || !strings.Contains(err.Error(), "left in place") {
		t.Fatalf("got %v, want a refusal that keeps the entry", err)
	}
	if !exists(dest) || readFile(t, dest) != "renamed onto src" {
		t.Error("the only name of the renamed file was removed")
	}
	if n := nlink(t, dest); n != 1 {
		t.Errorf("dest has %d links, want 1", n)
	}
	onlyFiles(t, to, "dest.mkv")
}

// PlaceNoClobber falls back to rename where link is refused, and on a
// filesystem whose inode numbers are not stable across a rename (a union or
// network mount that derives them from the path) the file then sits at dest
// under a new identity. The move is reported as failed, but the file, whose
// only name is now dest, must be left there: amuxify does not delete media.
// The Place seam models such a filesystem by giving the file a new identity
// on its way to dest.
func TestMoveNoClobberKeepsSoleNameWhenIdentityChanges(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, src, "payload", 0o644)
	orig := Place
	Place = func(s, d string) error {
		writeFile(t, d, readFile(t, s), 0o644)
		return os.Remove(s)
	}
	t.Cleanup(func() { Place = orig })
	err := MoveNoClobber(src, dest)
	if err == nil || !strings.Contains(err.Error(), "changed during the move") || !strings.Contains(err.Error(), "left in place") {
		t.Fatalf("got %v, want a refusal that keeps the entry", err)
	}
	if !exists(dest) || readFile(t, dest) != "payload" {
		t.Fatal("the moved file was deleted")
	}
	if exists(src) {
		t.Error("source still present")
	}
	onlyFiles(t, to, "dest.mkv")
}

// A Place that reports success but leaves nothing at dest, or leaves a
// directory there, is reported as a changed source, and a directory is not
// removed.
func TestMoveNoClobberPlaceLeavesWrongEntry(t *testing.T) {
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, src, "payload", 0o644)
	orig := Place
	t.Cleanup(func() { Place = orig })
	Place = func(s, d string) error { return nil }
	if err := MoveNoClobber(src, dest); err == nil || !strings.Contains(err.Error(), "changed during the move") {
		t.Fatalf("nothing at dest: got %v", err)
	}
	Place = func(s, d string) error { return os.Mkdir(d, 0o755) }
	if err := MoveNoClobber(src, dest); err == nil || !strings.Contains(err.Error(), "changed during the move") {
		t.Fatalf("directory at dest: got %v", err)
	}
	if fi, err := os.Lstat(dest); err != nil || !fi.IsDir() {
		t.Error("the directory at dest was removed")
	}
	if readFile(t, src) != "payload" {
		t.Error("source changed")
	}
}

// When the copy itself fails the partial destination is removed and the
// source is untouched.
func TestMoveNoClobberCrossDeviceCopyFailureRemovesPartial(t *testing.T) {
	forceCrossDevice(t)
	orig := CopyData
	CopyData = func(dst io.Writer, src io.Reader) (int64, error) {
		n, _ := io.CopyN(dst, src, 3)
		return n, errors.New("disk full")
	}
	t.Cleanup(func() { CopyData = orig })
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, src, "payload", 0o644)
	err := MoveNoClobber(src, dest)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("got %v", err)
	}
	if exists(dest) {
		t.Error("partial copy left behind")
	}
	if readFile(t, src) != "payload" {
		t.Error("source changed")
	}
}

// A copy that writes the right number of bytes but not the right bytes is
// caught by the hash comparison: the seam reads all of src, so the hasher
// sees the true content, and writes a same-length corruption to dst.
func TestMoveNoClobberCrossDeviceCorruptCopyIsRefused(t *testing.T) {
	forceCrossDevice(t)
	orig := CopyData
	CopyData = func(dst io.Writer, src io.Reader) (int64, error) {
		b, err := io.ReadAll(src)
		if err != nil {
			return 0, err
		}
		bad := []byte(strings.Repeat("x", len(b)))
		n, err := dst.Write(bad)
		return int64(n), err
	}
	t.Cleanup(func() { CopyData = orig })
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, src, "payload", 0o644)
	err := MoveNoClobber(src, dest)
	if err == nil || !strings.Contains(err.Error(), "does not hash") {
		t.Fatalf("a corrupt copy was accepted: %v", err)
	}
	if exists(dest) || readFile(t, src) != "payload" {
		t.Error("corrupt copy left behind or source changed")
	}
}

// A short copy that reports no error is still caught by the size check.
func TestMoveNoClobberCrossDeviceShortCopyIsRefused(t *testing.T) {
	forceCrossDevice(t)
	orig := CopyData
	CopyData = func(dst io.Writer, src io.Reader) (int64, error) { return io.CopyN(dst, src, 3) }
	t.Cleanup(func() { CopyData = orig })
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, src, "payload", 0o644)
	if err := MoveNoClobber(src, dest); err == nil {
		t.Fatal("a short copy was accepted")
	}
	if exists(dest) || readFile(t, src) != "payload" {
		t.Error("partial copy left behind or source changed")
	}
}

// The source vanishes or is swapped while the copy runs: the bytes copied
// came from the file that was opened, so the copy is discarded rather than
// removing whatever now sits at the source path.
func TestMoveNoClobberCrossDeviceSourceChangedMidCopy(t *testing.T) {
	for _, tc := range []struct{ name, action string }{{"vanishes", "remove"}, {"swapped", "swap"}} {
		t.Run(tc.name, func(t *testing.T) {
			forceCrossDevice(t)
			from, to := t.TempDir(), t.TempDir()
			src := filepath.Join(from, "src.mkv")
			dest := filepath.Join(to, "dest.mkv")
			writeFile(t, src, "payload", 0o644)
			orig := CopyData
			CopyData = func(dst io.Writer, r io.Reader) (int64, error) {
				n, err := io.Copy(dst, r)
				switch tc.action {
				case "remove":
					_ = os.Remove(src)
				case "swap":
					other := filepath.Join(from, "other.mkv")
					writeFile(t, other, "swapped in", 0o644)
					if err := os.Rename(other, src); err != nil {
						t.Fatal(err)
					}
				}
				return n, err
			}
			t.Cleanup(func() { CopyData = orig })
			err := MoveNoClobber(src, dest)
			if err == nil || !strings.Contains(err.Error(), "changed during the move") {
				t.Fatalf("got %v, want a refusal", err)
			}
			if exists(dest) {
				t.Error("copy kept although the source changed")
			}
			if tc.action == "swap" && readFile(t, src) != "swapped in" {
				t.Error("the file swapped into the source path was removed")
			}
		})
	}
}

// Two moves racing for one destination across filesystems: exactly one
// wins, the loser keeps its source.
func TestMoveNoClobberCrossDeviceOnlyOneWinner(t *testing.T) {
	forceCrossDevice(t)
	from, to := t.TempDir(), t.TempDir()
	a := filepath.Join(from, "a.mkv")
	b := filepath.Join(from, "b.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, a, "A", 0o644)
	writeFile(t, b, "B", 0o644)
	errA := MoveNoClobber(a, dest)
	errB := MoveNoClobber(b, dest)
	if errA != nil {
		t.Fatalf("first move failed: %v", errA)
	}
	if !errors.Is(errB, ErrExists) {
		t.Fatalf("second move: got %v, want ErrExists", errB)
	}
	if readFile(t, dest) != "A" || !exists(b) || exists(a) {
		t.Fatal("wrong winner or the loser's source was removed")
	}
}

// Any error other than EXDEV from the same-device move is returned as is,
// without falling back to a copy.
func TestMoveNoClobberOnlyCopiesOnEXDEV(t *testing.T) {
	orig := Place
	Place = func(src, dest string) error { return errors.New("permission denied") }
	t.Cleanup(func() { Place = orig })
	from, to := t.TempDir(), t.TempDir()
	src := filepath.Join(from, "src.mkv")
	dest := filepath.Join(to, "dest.mkv")
	writeFile(t, src, "payload", 0o644)
	if err := MoveNoClobber(src, dest); err == nil || err.Error() != "permission denied" {
		t.Fatalf("got %v", err)
	}
	if exists(dest) || readFile(t, src) != "payload" {
		t.Fatal("copy attempted on a non-EXDEV error")
	}
}
