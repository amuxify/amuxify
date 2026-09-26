//go:build unix

package fsutil

import (
	"errors"
	"os"
	"syscall"
	"time"
)

// openNoFollow opens path for writing without following a symbolic link at
// the final component, so the descriptor it returns can only lead to the
// entry that sits at the name itself.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_WRONLY|syscall.O_NOFOLLOW, 0)
}

// refusedSymlink reports whether err is the error openNoFollow returns when
// the name was a symbolic link: ELOOP on Linux and macOS, EMLINK on some
// BSDs.
func refusedSymlink(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK)
}

// futimes sets the access and modification times of the open file f through
// its descriptor, never through a name.
func futimes(f *os.File, atime, mtime time.Time) error {
	tv := []syscall.Timeval{
		syscall.NsecToTimeval(atime.UnixNano()),
		syscall.NsecToTimeval(mtime.UnixNano()),
	}
	return syscall.Futimes(int(f.Fd()), tv)
}
