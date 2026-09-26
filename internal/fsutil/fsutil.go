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

// ReplaceInPlace renames tmp over dest, preserving dest's ownership, mode and
// modification time. Used only by --in-place after verification passed.
func ReplaceInPlace(tmp, dest string) error {
	fi, err := os.Stat(dest)
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp, fi.Mode().Perm()); err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		// Best effort: chown fails for non-root users changing owner, which is fine.
		_ = os.Chown(tmp, int(st.Uid), int(st.Gid))
	}
	_ = os.Chtimes(tmp, time.Now(), fi.ModTime())
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

// IsRoot reports whether the process runs as uid 0.
func IsRoot() bool { return os.Geteuid() == 0 }

// Abs returns a cleaned absolute path or an error.
func Abs(p string) (string, error) {
	a, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("%s: %w", p, err)
	}
	return filepath.Clean(a), nil
}
