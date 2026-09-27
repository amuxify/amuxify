package fsutil

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// Place is the same-device move MoveNoClobber tries first. Tests replace it
// to force the cross-device copy path, which a unit test cannot otherwise
// reach without a second filesystem.
var Place = PlaceNoClobber

// CopyData copies the bytes of one file into another during a cross-device
// move. Tests replace it to fail the copy or to change the source while it
// runs.
var CopyData = func(dst io.Writer, src io.Reader) (int64, error) { return io.Copy(dst, src) }

// MoveNoClobber moves src to dest without ever replacing an existing file,
// on the same filesystem or across filesystems. src must be a regular file:
// it is checked with Lstat first, so a symlink is never followed, whichever
// way the move runs (guarantee 3). On one filesystem it is PlaceNoClobber,
// followed by a check that the entry now at dest is the very file src was
// before the move; link(2) follows a symlink source on some systems, so a
// symlink swapped into src between the check and the link would otherwise
// leave a hard link to the link's target at dest. When the check fails an
// error is returned, and the entry at dest is removed only when removing it
// cannot delete the last name of any file: when it is a symlink, or a
// regular file that still has another name, which is what a hard link to a
// symlink's target is. A regular file whose only name is dest is left in
// place, because on a filesystem whose inode numbers are not stable across
// a rename (some FUSE, CIFS and union mounts) the rename fallback of
// PlaceNoClobber lands the original file at dest under a new number, and
// removing it would delete the only copy of the media. Across filesystems,
// where link and rename fail with EXDEV, it creates dest with
// O_CREATE|O_EXCL (so a file, a symlink or a named pipe already at dest is
// refused and never followed or opened), copies the bytes, fsyncs, reads
// dest back through OpenRegular and compares
// its SHA-256 with the hash of the bytes copied, checks that src is still
// the file it opened, and only then removes src. On any failure the partial
// copy is removed and src is left untouched. The copy is created with mode
// 0600. It is used by quarantine, whose directory usually sits on another
// filesystem than the media tree; output placement keeps using
// PlaceNoClobber and stays on one filesystem (guarantee 2).
func MoveNoClobber(src, dest string) error {
	lfi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !lfi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; not moved", src)
	}
	err = Place(src, dest)
	if err != nil {
		if errors.Is(err, syscall.EXDEV) {
			return copyThenRemove(src, dest)
		}
		return err
	}
	now, err := os.Lstat(dest)
	if err != nil {
		return fmt.Errorf("%s changed during the move: %v", src, err)
	}
	if now.Mode().IsRegular() && os.SameFile(lfi, now) {
		return nil
	}
	// The entry at dest is not the file that was checked. A symlink is
	// removed (only the link itself goes), and so is a regular file that
	// still has another name, since dest then holds a hard link to a file
	// that keeps its own name. A regular file with a single name and
	// anything else, such as a directory, is left where it is.
	if now.Mode()&os.ModeSymlink != 0 || (now.Mode().IsRegular() && Nlink(now) >= 2) {
		_ = os.Remove(dest)
		return fmt.Errorf("%s changed during the move; the entry placed at %s was discarded", src, dest)
	}
	return fmt.Errorf("%s changed during the move; the entry placed at %s was left in place", src, dest)
}

// identity is the device and inode pair that names a file independently of
// its path; it is what proves a path still leads to the file that was opened.
type identity struct {
	dev uint64
	ino uint64
	ok  bool
}

func identityOf(fi os.FileInfo) identity {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return identity{dev: uint64(st.Dev), ino: uint64(st.Ino), ok: true}
	}
	return identity{}
}

func copyThenRemove(src, dest string) (err error) {
	lfi, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !lfi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file; refusing to copy it", src)
	}
	// The path was a regular file at Lstat; the open refuses anything else
	// without following a link or blocking on a pipe swapped in since, and
	// the file opened must still be that same file.
	in, err := OpenRegular(src)
	if err != nil {
		return err
	}
	defer in.Close()
	sfi, err := in.Stat()
	if err != nil {
		return err
	}
	if !sfi.Mode().IsRegular() || identityOf(sfi) != identityOf(lfi) {
		return fmt.Errorf("%s changed while it was being opened; refusing to copy it", src)
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return ErrExists
		}
		return err
	}
	// From here on a failure removes the copy, and only the copy: the file
	// removed is checked to be the one created here, so a file swapped into
	// dest by someone else is left alone.
	ofi, statErr := out.Stat()
	created := identityOf(ofi)
	remove := func() {
		_ = out.Close()
		if statErr == nil {
			if now, err := os.Lstat(dest); err != nil || !now.Mode().IsRegular() || identityOf(now) != created {
				return
			}
		}
		_ = os.Remove(dest)
	}
	if statErr != nil {
		remove()
		return statErr
	}

	h := sha256.New()
	n, err := CopyData(out, io.TeeReader(in, h))
	if err != nil {
		remove()
		return err
	}
	if n != sfi.Size() {
		remove()
		return fmt.Errorf("copied %d bytes of %s, expected %d", n, src, sfi.Size())
	}
	if err := out.Sync(); err != nil {
		remove()
		return err
	}
	if err := out.Close(); err != nil {
		remove()
		return err
	}
	if err := verifyCopy(dest, created, n, h.Sum(nil)); err != nil {
		remove()
		return err
	}
	// The source must still be the file that was copied before it is
	// removed; a file renamed onto its path meanwhile stays where it is.
	if now, err := os.Lstat(src); err != nil || !now.Mode().IsRegular() || identityOf(now) != identityOf(lfi) || now.Size() != sfi.Size() {
		remove()
		if err != nil {
			return fmt.Errorf("%s changed during the move: %v", src, err)
		}
		return fmt.Errorf("%s changed during the move; the copy was discarded", src)
	}
	if err := os.Remove(src); err != nil {
		remove()
		return err
	}
	return nil
}

// verifyCopy reads dest back and checks that it is the file created here,
// has the expected size and hashes to sum. The name is opened with
// OpenRegular, so a link or a named pipe swapped onto it while the copy ran
// is refused rather than followed or waited on.
func verifyCopy(dest string, created identity, size int64, sum []byte) error {
	f, err := OpenRegular(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || identityOf(fi) != created {
		return fmt.Errorf("%s is not the file that was written", dest)
	}
	if fi.Size() != size {
		return fmt.Errorf("%s holds %d bytes after the copy, expected %d", dest, fi.Size(), size)
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if !bytes.Equal(h.Sum(nil), sum) {
		return fmt.Errorf("%s does not hash to the bytes that were copied", dest)
	}
	return nil
}
