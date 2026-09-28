//go:build linux || darwin

package fsutil

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

// Guarantee 3 covers what an open can do before the descriptor check
// refuses the entry, and a terminal device is the one kind of entry whose
// open has a side effect of its own: a process that leads its session and
// has no controlling terminal, which amuxify under a container init,
// systemd or setsid is, acquires the terminal it opens as its controlling
// terminal, and a hangup on it then reaches the process as SIGHUP. The
// test allocates a pseudo-terminal, plants its slave device where a media
// file would be, and runs the test binary again as a new session leader
// (Setsid) that opens the device through OpenRegular. The device must be
// refused and the child must still have no controlling terminal, which
// /dev/tty tells: it opens only for a process that has one. The child then
// opens the device with a plain open, which does acquire it, and confirms
// through /dev/tty that it did; this proves the environment was one in
// which the flag mattered, so a lost O_NOCTTY would have failed the first
// step rather than passed it by accident.
func TestOpenRegularDoesNotAcquireControllingTerminal(t *testing.T) {
	const env = "AMUXIFY_TEST_PTY_SLAVE"
	if slave := os.Getenv(env); slave != "" {
		os.Exit(nocttyHelper(slave))
	}
	_, slave := openPTY(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestOpenRegularDoesNotAcquireControllingTerminal$", "-test.v=false")
	cmd.Env = append(os.Environ(), env+"="+slave)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	out, err := cmd.CombinedOutput()
	msg := strings.TrimSpace(string(out))
	if strings.HasPrefix(msg, "premise: ") {
		t.Skipf("the child could not acquire a controlling terminal at all, so the flag cannot be observed: %s", msg)
	}
	if err != nil {
		t.Fatalf("%v: %s", err, msg)
	}
}

// nocttyHelper is the body of the session-leader child; it prints what
// went wrong and returns the exit status.
func nocttyHelper(slave string) int {
	if f, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		f.Close()
		fmt.Println("premise: the child already has a controlling terminal")
		return 1
	}
	f, err := OpenRegular(slave)
	if err == nil {
		f.Close()
		fmt.Printf("OpenRegular opened the terminal device %s as a regular file\n", slave)
		return 1
	}
	if !strings.Contains(err.Error(), "device") {
		fmt.Printf("the refusal does not name the device: %v\n", err)
		return 1
	}
	if tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0); err == nil {
		tty.Close()
		fmt.Printf("OpenRegular of %s made it the controlling terminal\n", slave)
		return 1
	}
	// The plain open is the control: it acquires the terminal, and /dev/tty
	// then opens.
	plain, err := os.OpenFile(slave, os.O_RDWR, 0)
	if err != nil {
		fmt.Printf("premise: plain open of %s: %v\n", slave, err)
		return 1
	}
	defer plain.Close()
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		fmt.Printf("premise: a plain open of %s did not become the controlling terminal: %v\n", slave, err)
		return 1
	}
	tty.Close()
	return 0
}
