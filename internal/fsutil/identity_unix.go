//go:build unix

package fsutil

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// openNoFollow opens path with the access flag given (O_RDONLY or O_WRONLY)
// and returns it only when the entry at the name is a regular file. The
// open itself never follows a symbolic link at the final component and
// never blocks: a named pipe planted at the name would otherwise park the
// process in open(2) until a peer shows up, which a planter can withhold
// for as long as it likes. O_NONBLOCK makes open(2) return at once (a
// write-only open of a pipe with no reader fails with ENXIO, a read-only
// one succeeds), fstat then tells what was opened, and anything that is
// not a regular file is closed again and refused with an error that names
// its kind. O_NONBLOCK is cleared before the file is returned, so ordinary
// reads and writes on it behave as they always have. O_NOCTTY covers the
// one effect an open can have before fstat refuses the entry: a process
// that leads its session and has no controlling terminal, which a run under
// a container init, systemd or setsid is, would otherwise acquire a
// terminal device planted at the name as its controlling terminal for the
// rest of the run, and a hangup on that terminal would then reach it as
// SIGHUP. The flag has no effect on a regular file or a pipe.
func openNoFollow(path string, flag int) (*os.File, error) {
	fd, err := openRetry(path, flag|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOCTTY)
	if err != nil {
		if errors.Is(err, syscall.ENXIO) {
			return nil, fmt.Errorf("%s is a named pipe or a socket; refusing to open it", path)
		}
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		_ = syscall.Close(fd)
		return nil, &os.PathError{Op: "fstat", Path: path, Err: err}
	}
	if st.Mode&syscall.S_IFMT != syscall.S_IFREG {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("%s is %s; refusing to open it", path, kindOf(uint32(st.Mode)))
	}
	if err := syscall.SetNonblock(fd, false); err != nil {
		_ = syscall.Close(fd)
		return nil, &os.PathError{Op: "fcntl", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openRetry is open(2) repeated on EINTR, which the Go runtime's own
// signals can cause.
func openRetry(path string, flag int) (int, error) {
	for {
		fd, err := syscall.Open(path, flag, 0)
		if err == syscall.EINTR {
			continue
		}
		return fd, err
	}
}

// kindOf names the kind of entry a stat mode describes, for an error
// message about something that should have been a regular file.
func kindOf(mode uint32) string {
	switch mode & syscall.S_IFMT {
	case syscall.S_IFIFO:
		return "a named pipe"
	case syscall.S_IFSOCK:
		return "a socket"
	case syscall.S_IFCHR, syscall.S_IFBLK:
		return "a device"
	case syscall.S_IFDIR:
		return "a directory"
	case syscall.S_IFLNK:
		return "a symlink"
	}
	return "not a regular file"
}

// refusedSymlink reports whether err is the error openNoFollow returns when
// the name was a symbolic link: ELOOP on Linux and macOS, EMLINK on some
// BSDs.
func refusedSymlink(err error) bool {
	return errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK)
}
