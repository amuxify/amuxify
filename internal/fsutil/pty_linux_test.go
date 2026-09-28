//go:build linux

package fsutil

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"unsafe"
)

// openPTY allocates a pseudo-terminal pair through /dev/ptmx and returns
// the open master and the name of its slave device. The master stays open
// for the test, which is what lets the slave be opened at all. The two
// ioctls are unlockpt(3) and ptsname(3) in their kernel form.
func openPTY(t *testing.T) (*os.File, string) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	const (
		tiocSPTLCK = 0x40045431 // TIOCSPTLCK, int in
		tiocGPTN   = 0x80045430 // TIOCGPTN, unsigned int out
	)
	unlock := int32(0)
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), tiocSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		master.Close()
		t.Skipf("unlockpt: %v", e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), tiocGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		master.Close()
		t.Skipf("ptsname: %v", e)
	}
	t.Cleanup(func() { master.Close() })
	return master, fmt.Sprintf("/dev/pts/%d", n)
}
