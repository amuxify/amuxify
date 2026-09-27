// Package fsutil holds the filesystem safety primitives: symlink checks,
// hardlink counts, no-clobber placement, temp-file naming, stat preservation.
package fsutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// TempName returns a hidden sibling path for writing output before rename.
func TempName(dest string) string {
	dir, base := filepath.Split(dest)
	return filepath.Join(dir, ".amuxify-"+base+".tmp")
}

// IsSymlink reports whether path itself is a symbolic link.
func IsSymlink(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return false, err
	}
	return fi.Mode()&os.ModeSymlink != 0, nil
}

// Nlink returns the hard-link count of a path (1 when unknown).
func Nlink(fi os.FileInfo) uint64 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink)
	}
	return 1
}

// ErrExists is returned by PlaceNoClobber when the destination exists.
var ErrExists = errors.New("destination already exists")

// PlaceNoClobber moves tmp to dest without ever replacing an existing file.
// It tries a hard link first (atomic no-clobber on POSIX), then falls back to
// a stat check plus rename for filesystems without link support.
func PlaceNoClobber(tmp, dest string) error {
	if err := os.Link(tmp, dest); err == nil {
		return os.Remove(tmp)
	} else if os.IsExist(err) || errors.Is(err, os.ErrExist) {
		return ErrExists
	}
	if _, err := os.Lstat(dest); err == nil {
		return ErrExists
	}
	return os.Rename(tmp, dest)
}

// Temp is a temp file created by CreateTemp and still held open.
type Temp struct {
	f  *os.File
	fi os.FileInfo
}

// Info returns the identity recorded when the file was created, for
// comparison with os.SameFile.
func (t *Temp) Info() os.FileInfo { return t.fi }

// Close releases the descriptor, after which the inode may be freed and its
// number given to another file. Closing a closed or nil handle is harmless.
func (t *Temp) Close() error {
	if t == nil || t.f == nil {
		return nil
	}
	err := t.f.Close()
	t.f = nil
	return err
}

// Sync flushes the file to stable storage through the descriptor held
// since its creation. fsync acts on the file, not on the descriptor, so the
// data an external tool wrote into that file through its name is flushed
// as well, and the name itself is never opened again for it: an entry
// swapped onto the name cannot be reached this way, and a named pipe
// planted there cannot stall the flush.
func (t *Temp) Sync() error {
	if t == nil || t.f == nil {
		return errors.New("temp file is not open")
	}
	return t.f.Sync()
}

// CreateTemp removes a leftover entry at tmp, creates tmp empty and
// exclusively, and returns a handle to the file it made, so that a name
// about to be handed to an external tool belongs to the caller before the
// tool starts and can be recognised again afterwards with os.SameFile. The
// handle keeps the file open, and an open descriptor keeps the inode
// allocated, so its number cannot be freed and given to another file
// created at the same name while the handle is open. On ext4 a freed inode
// number is reused at once, and a file swapped onto the name that way would
// be indistinguishable from the caller's own by os.SameFile; the caller
// therefore closes the handle only after its last identity check. A
// leftover that cannot be removed is an error, as is anything that appears
// at the name between the removal and the creation. The file gets the mode
// the tool would give a file it created itself, 0666 under the umask.
//
// The creation is exclusive, so a named pipe or any other entry planted at
// the name before the call is removed as a leftover, never opened, and one
// planted between the removal and the creation makes the creation fail; a
// pipe cannot make this call block.
func CreateTemp(tmp string) (*Temp, error) {
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return nil, err
	}
	return &Temp{f: f, fi: fi}, nil
}

// CopyIdentityTo gives the open file f the mode, ownership and modification
// time of the file at src. Every write goes through the descriptor (fchmod,
// fchown and futimes), never through a name, so a symbolic link swapped onto
// the file's former name after f was opened cannot redirect any of them to
// another file. Ownership is best effort: chown fails for a non-root user
// changing the owner, which is fine. The mode and the time are not: a mode
// or a time that cannot be set is returned as an error, so that the callers
// (ReplaceInPlaceOwn and the remuxer's in-place placement) stop before the
// rename and the source keeps its name, rather than placing an output whose
// identity differs from the source without a finding saying so. On Linux
// that error also carries the refusal of the name-based fallback when the
// temp name was swapped while the descriptor calls were unavailable. Only
// src is read by name, and it is the file whose identity the caller wants
// copied.
func CopyIdentityTo(f *os.File, src string) error {
	fi, err := os.Stat(src)
	if err != nil {
		return err
	}
	if err := f.Chmod(fi.Mode().Perm()); err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = f.Chown(int(st.Uid), int(st.Gid))
	}
	if err := setTimes(f, time.Now(), fi.ModTime()); err != nil {
		return fmt.Errorf("set the modification time of %s: %v", f.Name(), err)
	}
	return nil
}

// setTimes is the descriptor-based time call CopyIdentityTo uses. Tests
// replace it to make the call fail on every platform, since the real
// futimes cannot be made to fail on macOS or the BSDs from a test.
var setTimes = futimes

// OpenOwn opens path for writing without following a symbolic link and
// returns the file only when what was opened is a regular file with a single
// name that, when created is not nil, is the file created describes. It is
// the check a caller performs on a temp file it made itself before it writes
// metadata to it or places it, so that a symbolic link, a hard link or a
// different file swapped onto the name is refused rather than followed. The
// open never blocks, so a named pipe swapped onto the name is refused too
// rather than left to stall the run (see OpenRegular). The caller closes
// the file.
func OpenOwn(path string, created os.FileInfo) (*os.File, error) {
	f, err := openNoFollow(path, os.O_WRONLY)
	if err != nil {
		if refusedSymlink(err) {
			return nil, fmt.Errorf("%s is now a symlink; refusing to follow it", path)
		}
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !fi.Mode().IsRegular() || (created != nil && !os.SameFile(created, fi)) {
		_ = f.Close()
		return nil, fmt.Errorf("%s is not the file this run created", path)
	}
	if n := Nlink(fi); n != 1 {
		_ = f.Close()
		return nil, fmt.Errorf("%s has %d hard links; expected 1", path, n)
	}
	return f, nil
}

// OpenRegular opens path for reading and returns the file only when the
// entry at the name is a regular file. It is the open every reader of an
// input file or of a temp file uses instead of os.Open: a symbolic link at
// the name is refused rather than followed, and the open never blocks, so
// a named pipe planted at the name (which open(2) would otherwise wait on
// until a writer appears, possibly forever) is refused as well, as is a
// socket, a device or a directory. The check is made on the descriptor
// after the open, so what is refused is what was really opened, not what
// an earlier stat of the name saw. The returned file is in ordinary
// blocking mode. The caller closes it.
func OpenRegular(path string) (*os.File, error) {
	f, err := openNoFollow(path, os.O_RDONLY)
	if err != nil {
		if refusedSymlink(err) {
			return nil, fmt.Errorf("%s is a symlink; refusing to follow it", path)
		}
		return nil, err
	}
	return f, nil
}

// ReplaceInPlace renames tmp over dest, giving the new file dest's
// ownership, mode and modification time first. The identity is copied
// through a descriptor of tmp obtained with OpenOwn, so a symbolic link
// swapped onto the temp name is refused and never has its target's metadata
// rewritten; the rename itself replaces whatever sits at dest without
// following it. Callers that recorded the temp file's identity when they
// created it use ReplaceInPlaceOwn, which also requires that identity.
func ReplaceInPlace(tmp, dest string) error {
	return ReplaceInPlaceOwn(tmp, dest, nil)
}

// ReplaceInPlaceOwn is ReplaceInPlace for a caller that created tmp itself
// and kept the os.FileInfo from that creation: the file opened at tmp must
// be that file, or nothing is written and nothing is renamed.
func ReplaceInPlaceOwn(tmp, dest string, created os.FileInfo) error {
	f, err := OpenOwn(tmp, created)
	if err != nil {
		return err
	}
	if err := CopyIdentityTo(f, dest); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if beforeRename != nil {
		beforeRename(tmp, dest)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	if created == nil {
		return nil
	}
	// The rename used the temp name once more after the descriptor was
	// closed, so an entry swapped onto that name in between is what now
	// sits at dest. The window needs write access to the directory and is
	// not closable without an exchange rename, which is not portable, so
	// the outcome is checked afterwards instead: the entry at dest must be
	// the file this run created. A foreign entry is reported and left where
	// it is, because the file it replaced is already gone and removing the
	// entry could delete the only name of some other file.
	now, err := os.Lstat(dest)
	if err != nil {
		return fmt.Errorf("%s after the rename: %v", dest, err)
	}
	if !now.Mode().IsRegular() || !os.SameFile(created, now) {
		return fmt.Errorf("the entry now at %s is not the file this run created; the file it replaced is gone and the entry was left in place", dest)
	}
	return nil
}

// beforeRename, when set by a test, runs after the descriptor of tmp is
// closed and before the rename, which is the one window in which the temp
// name is used again by ReplaceInPlaceOwn. It is nil in production.
var beforeRename func(tmp, dest string)

// Fsync flushes a written file to stable storage. The name is opened with
// OpenRegular, so a symbolic link or a named pipe swapped onto it is
// refused. A caller that still holds the handle from CreateTemp uses
// Temp.Sync instead and never opens the name again.
func Fsync(path string) error {
	f, err := OpenRegular(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// SameDevice reports whether two paths are on the same filesystem.
func SameDevice(a, b string) (bool, error) {
	fa, err := os.Stat(a)
	if err != nil {
		return false, err
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false, err
	}
	sa, oka := fa.Sys().(*syscall.Stat_t)
	sb, okb := fb.Sys().(*syscall.Stat_t)
	if !oka || !okb {
		return true, nil
	}
	return sa.Dev == sb.Dev, nil
}

// Ext returns the lower-case extension without the dot.
func Ext(path string) string {
	return strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
}

// Geteuid is os.Geteuid, replaceable in tests that exercise the root refusal.
var Geteuid = os.Geteuid

// IsRoot reports whether the process runs as uid 0.
func IsRoot() bool { return Geteuid() == 0 }

// Abs returns a cleaned absolute path or an error.
func Abs(p string) (string, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	return filepath.Clean(a), nil
}

// MkdirAllUnder creates dir, which must be root itself or lie below root,
// one component at a time without following symlinks. Every component that
// already exists between root and dir must be a real directory: a symlink or
// a regular file in that position returns an error and nothing is created,
// so a planted symlink can never redirect output or quarantine placement
// outside the tree the user named. root itself is taken as the user's choice:
// it may be a symlink, and when it does not exist yet it is created with
// os.MkdirAll; only the components below it are checked. The returned error
// for a dir outside root, or for a bad component, wraps no sentinel and is
// meant to be reported verbatim.
func MkdirAllUnder(root, dir string) error {
	root = filepath.Clean(root)
	dir = filepath.Clean(dir)
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return fmt.Errorf("mkdir %s: not under %s: %v", dir, root, err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("mkdir %s: outside %s", dir, root)
	}
	fi, err := os.Stat(root)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	case err != nil:
		return fmt.Errorf("mkdir %s: %w", dir, err)
	case !fi.IsDir():
		return fmt.Errorf("mkdir %s: %s is not a directory", dir, root)
	}
	if rel == "." {
		return nil
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		fi, err := os.Lstat(cur)
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("mkdir %s: %s is a symlink; refusing to follow it", dir, cur)
		case err == nil && !fi.IsDir():
			return fmt.Errorf("mkdir %s: %s exists and is not a directory", dir, cur)
		case err == nil:
			continue
		case errors.Is(err, os.ErrNotExist):
			if err := os.Mkdir(cur, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
			// Re-check what now sits there: a racing symlink plant between
			// the Lstat and the Mkdir must still be refused.
			fi, err := os.Lstat(cur)
			if err != nil {
				return fmt.Errorf("mkdir %s: %w", dir, err)
			}
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				return fmt.Errorf("mkdir %s: %s is not a directory", dir, cur)
			}
		default:
			return fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	return nil
}
