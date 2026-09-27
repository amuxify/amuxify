//go:build unix && !linux

package fsutil

import (
	"os"
	"syscall"
	"time"
)

// futimes sets the access and modification times of the open file f through
// its descriptor, never through a name. On macOS and the BSDs futimes(2) is
// a real descriptor call; Linux has its own file.
func futimes(f *os.File, atime, mtime time.Time) error {
	tv := []syscall.Timeval{
		syscall.NsecToTimeval(atime.UnixNano()),
		syscall.NsecToTimeval(mtime.UnixNano()),
	}
	return syscall.Futimes(int(f.Fd()), tv)
}
