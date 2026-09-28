//go:build !unix

package fsutil

import (
	"errors"
	"os"
	"time"
)

// errNoDescriptorIdentity is returned on platforms where amuxify cannot open
// a name without following a symbolic link or blocking on a named pipe, or
// set file times through a descriptor. The callers fail rather than fall
// back to a path-based call, which would reintroduce the window this
// package exists to close.
var errNoDescriptorIdentity = errors.New("descriptor-based file access is not supported on this platform")

func openNoFollow(path string, flag int) (*os.File, error) { return nil, errNoDescriptorIdentity }

func refusedSymlink(err error) bool { return false }

func futimes(f *os.File, atime, mtime time.Time) error { return errNoDescriptorIdentity }
