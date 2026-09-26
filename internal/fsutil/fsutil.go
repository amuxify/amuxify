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

// CopyIdentity gives path the mode, ownership and modification time of the
// file at from, so an output written beside its source can carry the
// source's identity before it is placed. Ownership is best effort: chown
// fails for a non-root user changing the owner, which is fine.
func CopyIdentity(from, path string) error {
	fi, err := os.Stat(from)
	if err != nil {
		return err
	}
	if err := os.Chmod(path, fi.Mode().Perm()); err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		_ = os.Chown(path, int(st.Uid), int(st.Gid))
	}
	_ = os.Chtimes(path, time.Now(), fi.ModTime())
	return nil
}

// ReplaceInPlace renames tmp over dest, preserving dest's ownership, mode and
// modification time. Used only by --in-place after verification passed.
func ReplaceInPlace(tmp, dest string) error {
	if err := CopyIdentity(dest, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}

// Fsync flushes a written file to stable storage.
func Fsync(path string) error {
	f, err := os.Open(path)
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
