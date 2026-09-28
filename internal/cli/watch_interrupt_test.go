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

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// stepWriter runs one action each time a marker line first appears in the
// output, in order; an action that sends the process SIGINT waits so the
// signal is delivered before the watcher looks at its context again. Two
// steps may share a marker, in which case the second waits for the marker's
// second appearance.
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
	for w.next < len(w.steps) {
		s := w.steps[w.next]
		need := 1
		for _, earlier := range w.steps[:w.next] {
			if earlier.marker == s.marker {
				need++
			}
		}
		if strings.Count(w.buf.String(), s.marker) < need {
			break
		}
		if err := s.action(); err != nil && w.err == nil {
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
// and the exit code is 0 whatever the verdicts were. The tree here holds
// sidecars only, so this covers the stop path; the interrupt while a tool
// is running is TestWatchInterruptFinishesFileInProgress.
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
}

// Guarantee 2 and the documented stop behaviour with a real tool running:
// an interrupt that lands while mkvmerge is rebuilding a file does not kill
// the tool. The file is finished with its real verdict, its output is placed
// and its temp file is gone, no further file is started, and the command
// exits 0. The mkvmerge wrapper announces the write and then waits for the
// test, which sends SIGINT while the temp file exists and only then lets
// the tool go on.
func TestWatchInterruptFinishesFileInProgress(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	real, err := r.Path(exec.MKVMerge)
	if err != nil {
		t.Fatal(err)
	}
	src := testutil.Copy(t, "sample.avi")
	dir := filepath.Dir(src)
	dest := filepath.Join(dir, "sample.mkv")
	tmp := filepath.Join(dir, ".amuxify-sample.mkv.tmp")
	// Sorted after sample.avi, so it is the next candidate of the same pass.
	z := write(t, filepath.Join(dir, "z.nfo"), "nfo\n")
	started := filepath.Join(t.TempDir(), "started")
	resume := filepath.Join(t.TempDir(), "resume")
	script := filepath.Join(t.TempDir(), "mkvmerge")
	// The Runner strips the environment, so the paths are baked in. The
	// wait is capped at 20 seconds so a broken test cannot hang.
	body := "#!/bin/sh\nwrite=0\nfor a in \"$@\"; do [ \"$a\" = -o ] && write=1; done\n" +
		"if [ \"$write\" = 1 ]; then\n: > " + shellQuote(started) + "\ni=0\n" +
		"while [ ! -e " + shellQuote(resume) + " ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done\nfi\n" +
		"exec " + shellQuote(real) + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_MKVMERGE", script)
	tempSeen := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for {
			if _, err := os.Lstat(started); err == nil {
				break
			}
			if time.Now().After(deadline) {
				tempSeen <- false
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		_, err := os.Lstat(tmp)
		syscall.Kill(os.Getpid(), syscall.SIGINT)
		// The signal is delivered and the context cancelled while the
		// tool is still waiting; only then is it allowed to finish.
		time.Sleep(300 * time.Millisecond)
		tempSeen <- err == nil
		if err := os.WriteFile(resume, nil, 0o644); err != nil {
			t.Error(err)
		}
	}()
	code, out, errb := run(t, "watch", "--interval", "10ms", "--settle", "0s", "--state-dir", t.TempDir(), dir)
	if !<-tempSeen {
		t.Fatalf("the temp file did not exist while mkvmerge ran, so the interrupt landed elsewhere:\n%s%s", out, errb)
	}
	if code != 0 {
		t.Fatalf("exit %d, want 0 after a clean interrupt\n%s%s", code, out, errb)
	}
	if countLines(out, "PASS  "+src) != 1 {
		t.Errorf("the file in progress was not finished with its verdict:\n%s%s", out, errb)
	}
	if strings.Contains(out+errb, "context canceled") || strings.Contains(out, "FAIL") {
		t.Errorf("the interrupt reached the tool:\n%s%s", out, errb)
	}
	if strings.Contains(out, z) {
		t.Errorf("a further file was started after the interrupt:\n%s", out)
	}
	if !strings.HasSuffix(out, "amuxify watch: interrupted, stopped\n") {
		t.Errorf("stop line missing at the end:\n%s", out)
	}
	if _, err := os.Lstat(dest); err != nil {
		t.Errorf("the rebuilt file was not placed: %v", err)
	}
	if _, err := os.Lstat(src); err == nil {
		t.Error("the source of an in-place rebuild is still there")
	}
	if _, err := os.Lstat(tmp); err == nil {
		t.Error("the temp file is still there")
	}
	noTemp(t, dir)
}

// shellQuote quotes s for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
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

// Under --json a continuous run writes one complete document per pass that
// ingested something, each on its own line: the first pass reports the file
// that was there, a file written after that document appeared is reported
// by a later pass in a document of its own, and the run is then
// interrupted. Stdout carries exactly those two lines and nothing else.
func TestWatchJSONWritesOneDocumentPerPass(t *testing.T) {
	testutil.Stubs(t)
	dir := t.TempDir()
	first := write(t, filepath.Join(dir, "first.nfo"), "nfo\n")
	second := filepath.Join(dir, "second.nfo")
	w := &stepWriter{steps: []step{
		{`"path":"` + first + `"`, func() error {
			return os.WriteFile(second, []byte("nfo\n"), 0o644)
		}},
		{`"path":"` + second + `"`, interrupt},
	}}
	var errb bytes.Buffer
	code := Main([]string{"--json", "watch", "--interval", "10ms", "--settle", "0s", dir}, w, &errb)
	out := w.buf.String()
	if w.err != nil {
		t.Fatalf("step: %v", w.err)
	}
	if w.next != 2 {
		t.Fatalf("the second file was never reported; output:\n%s%s", out, errb.String())
	}
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, out, errb.String())
	}
	ls := lines(out)
	if len(ls) != 2 {
		t.Fatalf("want exactly two lines on stdout, got %d:\n%s", len(ls), out)
	}
	for i, want := range []string{first, second} {
		doc := decodeReport(t, []byte(ls[i]))
		if doc["schema"] != report.SchemaID || doc["command"] != "ingest" {
			t.Errorf("line %d: schema %v command %v", i+1, doc["schema"], doc["command"])
		}
		files, _ := doc["files"].([]interface{})
		if len(files) != 1 {
			t.Fatalf("line %d: %d files, want 1:\n%s", i+1, len(files), ls[i])
		}
		if p := files[0].(map[string]interface{})["path"]; p != want {
			t.Errorf("line %d: path %v, want %s", i+1, p, want)
		}
	}
	if !strings.HasSuffix(errb.String(), "amuxify watch: interrupted, stopped\n") {
		t.Errorf("stop line not on stderr under --json:\n%s", errb.String())
	}
}
