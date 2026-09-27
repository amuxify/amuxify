//go:build unix

package mp4

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Guarantee 3: Parse never follows a symlink and never waits on a named
// pipe. The scanner and the cleaner call it by name on an input file and
// on a temp file, and a pipe swapped onto either would otherwise block the
// run until the planter chose to write.
func TestParseRefusesNamedPipeAndSymlink(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "a.mp4")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("cannot create a named pipe: %v", err)
	}
	target := filepath.Join(dir, "target.mp4")
	if err := os.WriteFile(target, []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom\x00\x00\x00\x08moov"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.mp4")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	for _, tc := range []struct{ name, path, want string }{
		{"named pipe", fifo, "named pipe"},
		{"symlink", link, "symlink"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			done := make(chan error, 1)
			go func() {
				in, err := Parse(tc.path)
				if in != nil && err == nil {
					done <- nil
					return
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("got %v, want a refusal naming the %s", err, tc.want)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("Parse(%s) did not return within 5s: it is blocked on the entry", tc.path)
			}
		})
	}
	in, err := Parse(target)
	if err != nil || !in.HasMoov {
		t.Fatalf("regular file: %+v %v", in, err)
	}
}
