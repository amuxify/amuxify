//go:build unix

package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/testutil"
)

// stepWriter runs one action each time a marker line first appears in the
// output, in order; an action that sends the process SIGINT waits so the
// signal is delivered before the watcher looks at its context again.
type stepWriter struct {
	mu    sync.Mutex
	buf   bytes.Buffer
	steps []step
	next  int
	err   error
}

type step struct {
	marker string
	action func() error
}

func (w *stepWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	for w.next < len(w.steps) && strings.Count(w.buf.String(), w.steps[w.next].marker) > w.next {
		if err := w.steps[w.next].action(); err != nil && w.err == nil {
			w.err = err
		}
		w.next++
	}
	return len(p), nil
}

func interrupt() error {
	err := syscall.Kill(os.Getpid(), syscall.SIGINT)
	time.Sleep(500 * time.Millisecond)
	return err
}

// A watch without --once runs until interrupted: the file in progress is
// finished and reported, no further file is started, the stop is announced
// and the exit code is 0 whatever the verdicts were. No temp file is left
// behind (guarantee 2).
func TestWatchInterruptedExitsClean(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "0.url"), "[InternetShortcut]\nURL=http://x\n")
	a := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	b := write(t, filepath.Join(dir, "b.nfo"), "nfo\n")
	w := &stepWriter{steps: []step{{"PASS  " + a + "\n", interrupt}}}
	var errb bytes.Buffer
	code := Main([]string{"watch", "--interval", "10ms", "--settle", "0s", dir}, w, &errb)
	out := w.buf.String()
	if w.next != 1 {
		t.Fatalf("marker line never streamed; output:\n%s", out)
	}
	if w.err != nil {
		t.Fatalf("kill: %v", w.err)
	}
	if code != 0 {
		t.Fatalf("exit %d, want 0 after a clean interrupt\n%s%s", code, out, errb.String())
	}
	if strings.Contains(out, "PASS  "+b+"\n") {
		t.Errorf("a further file was started after the interrupt:\n%s", out)
	}
	if !strings.Contains(out, "BLOCK "+filepath.Join(dir, "0.url")+"\n") {
		t.Errorf("the file before the interrupt is missing:\n%s", out)
	}
	if !strings.HasSuffix(out, "amuxify watch: interrupted, stopped\n") {
		t.Errorf("stop line missing at the end:\n%s", out)
	}
	if !strings.Contains(out, "\nBLOCK: 2 file(s) BLOCK=1 PASS=1\n") {
		t.Errorf("the interrupted pass did not end with its count line:\n%s", out)
	}
	noTemp(t, dir)
}

// Across passes of one run a settled file is ingested once, and a file that
// changes after its ingest is ingested again; the run is then interrupted.
func TestWatchIngestsChangedFileAgain(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	marker := "PASS  " + a + "\n"
	w := &stepWriter{steps: []step{
		{marker, func() error {
			// Give the watcher a few passes to prove it does not ingest
			// the unchanged file again, then change it.
			time.Sleep(100 * time.Millisecond)
			f, err := os.OpenFile(a, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return err
			}
			f.WriteString("more\n")
			f.Close()
			later := time.Now().Add(5 * time.Second)
			return os.Chtimes(a, later, later)
		}},
		{marker, interrupt},
	}}
	var errb bytes.Buffer
	code := Main([]string{"watch", "--interval", "10ms", "--settle", "0s", dir}, w, &errb)
	out := w.buf.String()
	if w.err != nil {
		t.Fatalf("step: %v", w.err)
	}
	if w.next != 2 {
		t.Fatalf("the changed file was not ingested again; output:\n%s", out)
	}
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errb.String())
	}
	if n := strings.Count(out, marker); n != 2 {
		t.Errorf("file reported %d times, want 2:\n%s", n, out)
	}
}
