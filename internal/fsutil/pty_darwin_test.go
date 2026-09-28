//go:build darwin

package fsutil

import (
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY allocates a pseudo-terminal pair through /dev/ptmx and returns
// the open master and the name of its slave device. The master stays open
// for the test. The three ioctls are grantpt(3), unlockpt(3) and
// ptsname(3) in their kernel form.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	const (
		tiocPTYGRANT = 0x20007454 // TIOCPTYGRANT
		tiocPTYUNLK  = 0x20007452 // TIOCPTYUNLK
		tiocPTYGNAME = 0x40807453 // TIOCPTYGNAME, 128-byte name out
	)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), tiocPTYGRANT, 0); e != 0 {
		master.Close()
		t.Skipf("grantpt: %v", e)
	}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), tiocPTYUNLK, 0); e != 0 {
		master.Close()
		t.Skipf("unlockpt: %v", e)
	}
	var name [128]byte
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), tiocPTYGNAME, uintptr(unsafe.Pointer(&name[0]))); e != 0 {
		master.Close()
		t.Skipf("ptsname: %v", e)
	}
	t.Cleanup(func() { master.Close() })
	n := 0
	for n < len(name) && name[n] != 0 {
		n++
	}
	return master, string(name[:n])
}
