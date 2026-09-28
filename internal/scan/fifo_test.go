//go:build unix

package scan

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// Guarantee 3 covers every entry swapped onto or planted at a name the run
// opens, and a named pipe is the one that can stall the process for good:
// open(2) on it waits for a peer the planter never has to supply. These
// tests plant pipes where the scanner reads and require a prompt refusal.

const fifoWait = 5 * time.Second

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a named pipe: %v", err)
	}
}

// within runs fn and fails when it has not returned after d. fn reports
// through its return value, never through t, as it runs on its own
// goroutine.
func within(t *testing.T, d time.Duration, what string, fn func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: it is blocked on the named pipe", what, d)
		return nil
	}
}

// A pipe as the input, under a media name, a sidecar name or given as the
// path itself, is refused before anything opens it: FAIL UNREADABLE with
// the kind named, never EMPTY_FILE for its zero size, and no tool runs.
func TestNamedPipeInputRefused(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	media := filepath.Join(dir, "a.mkv")
	sidecar := filepath.Join(dir, "b.nfo")
	mkfifo(t, media)
	mkfifo(t, sidecar)
	write(t, filepath.Join(dir, "c.srt"), "1\n00:00:01,000 --> 00:00:02,000\nHello\n", 0o644)
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	var res []Result
	err := within(t, fifoWait, "ScanPath", func() (err error) {
		res, err = s.ScanPath(context.Background(), dir)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 3 {
		t.Fatalf("%d results, want 3: the run must go on past the pipes", len(res))
	}
	for _, r := range res {
		fr := r.File
		switch filepath.Base(fr.Path) {
		case "a.mkv", "b.nfo":
			expect(t, fr, report.Fail, CodeUnreadable)
			if fr.Has(CodeEmpty) || fr.Has(CodeSymlink) {
				t.Errorf("%s: %v", fr.Path, codes(fr))
			}
			if f, ok := findingOK(fr, CodeUnreadable); !ok || !strings.Contains(f.Message, "named pipe") {
				t.Errorf("%s: the finding does not name the pipe: %+v", fr.Path, f)
			}
			if fi, err := os.Lstat(fr.Path); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
				t.Errorf("%s: the pipe was removed or replaced: %v", fr.Path, err)
			}
		case "c.srt":
			expect(t, fr, report.Pass)
		}
	}
	// The pipe as the path itself.
	err = within(t, fifoWait, "ScanPath on the pipe", func() (err error) {
		res, err = s.ScanPath(context.Background(), media)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	expect(t, res[0].File, report.Fail, CodeUnreadable)
}

func findingOK(fr report.FileResult, code string) (report.Finding, bool) {
	for _, f := range fr.Findings {
		if f.Code == code {
			return f, true
		}
	}
	return report.Finding{}, false
}

// The readers the scanner calls by name after its own stat of the input,
// the polyglot check and the NFO reader, refuse a pipe swapped onto the
// path in the meantime rather than wait on it.
func TestScanReadersRefuseNamedPipe(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "a.mkv")
	mkfifo(t, fifo)
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	err := within(t, fifoWait, "polyglot", func() error {
		if got := s.polyglot(fifo, 4096); got != "" {
			t.Errorf("polyglot reported %q for a pipe", got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = within(t, fifoWait, "readNfo", func() error {
		_, err := readNfo(fifo)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("readNfo: got %v, want a refusal", err)
	}
}

// A pipe planted at the quarantine slot of a BLOCK file is never opened
// and never replaced: the move is refused, the file stays where it is, and
// the run reports the failed quarantine and returns promptly.
func TestQuarantineRefusesNamedPipeAtDestination(t *testing.T) {
	noTools(t)
	root := t.TempDir()
	q := filepath.Join(t.TempDir(), "quarantine")
	src := write(t, filepath.Join(root, "sub", "x.url"), "[InternetShortcut]\nURL=http://x\n", 0o644)
	dest := filepath.Join(q, "sub", "x.url")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	mkfifo(t, dest)
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Quarantine = q
	var res []Result
	err := within(t, fifoWait, "ScanPath with quarantine", func() (err error) {
		res, err = s.ScanPath(context.Background(), root)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	fr := res[0].File
	expect(t, fr, report.Block, CodeSidecarBlocked)
	requireQuarantineFinding(t, fr, report.Warn, "quarantine failed")
	if b, err := os.ReadFile(src); err != nil || !strings.Contains(string(b), "InternetShortcut") {
		t.Fatalf("source moved or changed: %v", err)
	}
	if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the pipe at the quarantine slot was removed or replaced: %v", err)
	}
}

// The attachment extraction hands mkvextract a path under a directory this
// run created; the file is read back with a reader that refuses a pipe. A
// tool that leaves a pipe at that path (this wrapper does, after the real
// mkvextract has run) cannot stall the scan: the attachment goes unsniffed
// and the scan of the file finishes.
func TestAttachmentSwappedForPipeDoesNotBlock(t *testing.T) {
	r := testutil.Need(t, exec.FFprobe, exec.MKVMerge, exec.MKVExtract)
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	real, err := r.Path(exec.MKVExtract)
	if err != nil {
		t.Fatal(err)
	}
	// The third argument is "<id>:<path>"; the part after the first colon
	// is the extraction target.
	script := filepath.Join(t.TempDir(), "mkvextract")
	marker := filepath.Join(t.TempDir(), "planted")
	body := "#!/bin/sh\n" + shq(real) + " \"$@\"\nrc=$?\nout=\"${3#*:}\"\n" +
		"rm -f \"$out\" && mkfifo \"$out\" && : > " + shq(marker) + "\nexit $rc\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_MKVEXTRACT", script)
	// A fresh runner: r resolved and cached the real mkvextract.
	var calls []string
	run := &exec.Runner{Timeout: 2 * time.Minute, Trace: func(s string) { calls = append(calls, s) }}
	s := newScanner(t, mustProfile(t, "homelab"), run)
	src := testutil.Copy(t, "fake_font.mkv")
	var fr report.FileResult
	err = within(t, 30*time.Second, "ScanFile", func() error {
		fr = s.ScanFile(context.Background(), src, filepath.Dir(src)).File
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	extracted := false
	for _, c := range calls {
		if strings.Contains(c, "mkvextract") && strings.Contains(c, "attachments") {
			extracted = true
		}
	}
	if !extracted {
		t.Fatalf("mkvextract never ran; the pipe was not planted: %v", codes(fr))
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the wrapper did not plant the pipe: %v", err)
	}
	if fr.Has(CodeAttachExec) {
		t.Fatalf("ATTACH_EXEC reported although the extracted attachment was a pipe and could not be read: %v", codes(fr))
	}
}

// shq quotes s for /bin/sh.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
