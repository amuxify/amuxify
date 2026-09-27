//go:build linux

package fsutil

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// Setting a file's times through a descriptor on Linux.
//
// syscall.Futimes, which the other Unix systems use here, is implemented on
// Linux as utimes("/proc/self/fd/N") and so needs /proc to be mounted; a
// minimal or hardened container may not have it, and the in-place remux
// would then place its output with a fresh modification time (guarantee
// 6). The kernel's own descriptor form is utimensat(fd, NULL, times, 0),
// the Linux-specific call glibc's futimens(3) makes; it is what runs first
// here. Neither the standard library nor golang.org/x/sys/unix wraps that
// form (unix.UtimesNanoAt requires a path, and utimensat has not accepted
// an empty path with AT_EMPTY_PATH on the kernels in common use), so it is
// a raw system call. The /proc path is kept as the second attempt
// for a kernel or a seccomp profile that refuses the first. Only when both
// fail is the file's name used, and then only after the name has been
// checked to still lead to the file that was opened, with the call told
// never to follow a symbolic link.

const (
	atFDCWD           = -0x64 // AT_FDCWD, an int in the kernel's interface
	atSymlinkNoFollow = 0x100 // AT_SYMLINK_NOFOLLOW
)

// futimensFd and futimesProc are the two descriptor calls, replaceable in
// tests that simulate a missing /proc or a refused system call.
var (
	futimensFd  = utimensatFd
	futimesProc = syscall.Futimes
)

// utimensatFd is utimensat(fd, NULL, ts, 0): the times are set on the file
// the descriptor refers to, and no name is involved.
func utimensatFd(fd int, ts *[2]syscall.Timespec) error {
	_, _, e := syscall.Syscall6(syscall.SYS_UTIMENSAT, uintptr(fd), 0, uintptr(unsafe.Pointer(ts)), 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// utimensatNoFollow is utimensat(AT_FDCWD, path, ts, AT_SYMLINK_NOFOLLOW):
// a symbolic link at path has its own times set and its target is never
// touched.
func utimensatNoFollow(path string, ts *[2]syscall.Timespec) error {
	p, err := syscall.BytePtrFromString(path)
	if err != nil {
		return err
	}
	dirfd := atFDCWD
	_, _, e := syscall.Syscall6(syscall.SYS_UTIMENSAT, uintptr(dirfd), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(ts)), uintptr(atSymlinkNoFollow), 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

// futimes sets the access and modification times of the open file f. It
// goes through the descriptor, and falls back to the file's name only when
// the descriptor calls are unavailable and the name still leads to f.
func futimes(f *os.File, atime, mtime time.Time) error {
	ts := [2]syscall.Timespec{
		syscall.NsecToTimespec(atime.UnixNano()),
		syscall.NsecToTimespec(mtime.UnixNano()),
	}
	fd := int(f.Fd())
	fdErr := futimensFd(fd, &ts)
	if fdErr == nil {
		return nil
	}
	tv := []syscall.Timeval{
		syscall.NsecToTimeval(atime.UnixNano()),
		syscall.NsecToTimeval(mtime.UnixNano()),
	}
	procErr := futimesProc(fd, tv)
	if procErr == nil {
		return nil
	}
	if err := utimesOwnName(f, &ts); err != nil {
		return fmt.Errorf("utimensat on the descriptor: %v; through /proc: %v; by name: %v", fdErr, procErr, err)
	}
	return nil
}

// utimesOwnName sets the times of f by its name, after checking that the
// name still leads to the very file f is open on: not a symbolic link, a
// regular file, and the same device and inode as the descriptor. The call
// itself never follows a link, so the window that remains between the
// check and the call can only be filled by a hard link to another file
// renamed onto the name; the name is looked at once more afterwards so a
// swap in that window is returned as an error rather than passed over.
// The error reaches CopyIdentityTo, which returns it, and the in-place
// callers then stop before the rename with the source still in place.
func utimesOwnName(f *os.File, ts *[2]syscall.Timespec) error {
	path := f.Name()
	ffi, err := f.Stat()
	if err != nil {
		return err
	}
	if !ffi.Mode().IsRegular() {
		return fmt.Errorf("%s is not open on a regular file", path)
	}
	if err := sameAsOpen(path, ffi); err != nil {
		return err
	}
	if err := utimensatNoFollow(path, ts); err != nil {
		return &os.PathError{Op: "utimensat", Path: path, Err: err}
	}
	return sameAsOpen(path, ffi)
}

// sameAsOpen reports an error when path does not name the regular file
// described by ffi.
func sameAsOpen(path string, ffi os.FileInfo) error {
	lfi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if lfi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is now a symlink; refusing to follow it", path)
	}
	if !lfi.Mode().IsRegular() || !os.SameFile(ffi, lfi) {
		return fmt.Errorf("%s no longer names the file that was opened", path)
	}
	return nil
}
