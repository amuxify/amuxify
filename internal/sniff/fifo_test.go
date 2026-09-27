//go:build unix

package sniff

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Guarantee 3: File never follows a symlink and never waits on a named
// pipe. A pipe planted at a path the scanner is about to sniff would
// otherwise block the read until the planter chose to write, and a link
// would have its target classified in place of the entry itself.
func TestFileRefusesNamedPipeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "a.mkv")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a named pipe: %v", err)
	}
	target := filepath.Join(dir, "target.mkv")
	if err := os.WriteFile(target, []byte("MZ\x90\x00payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.mkv")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, tc := range []struct{ name, path, want string }{
		{"named pipe", fifo, "named pipe"},
		{"symlink", link, "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			type out struct {
				res Result
				err error
			}
			done := make(chan out, 1)
			go func() {
				res, err := File(tc.path)
				done <- out{res, err}
			}()
			select {
			case got := <-done:
				if got.err == nil || !strings.Contains(got.err.Error(), tc.want) {
					t.Fatalf("got %v %v, want a refusal naming the %s", got.res, got.err, tc.want)
				}
				if got.res.Kind != Unknown {
					t.Fatalf("a refused path was classified as %s", got.res.Kind)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("File(%s) did not return within 5s: it is blocked on the entry", tc.path)
			}
		})
	}
	// The same bytes under their own name are still classified.
	res, err := File(target)
	if err != nil || res.Kind != Executable {
		t.Fatalf("regular file: %v %v", res, err)
	}
}
