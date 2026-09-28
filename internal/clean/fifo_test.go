//go:build unix

package clean

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
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/testutil"
)

// Guarantee 3 for the cleaner: a named pipe at a name the run opens must
// be refused promptly, never waited on. open(2) on a pipe blocks until a
// peer appears, and a planter never has to supply one.

const fifoWait = 5 * time.Second

func mkfifo(t *testing.T, path string) {
	t.Helper()
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a named pipe: %v", err)
	}
}

// within runs fn on its own goroutine and fails the test when it has not
// returned after d.
func within(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not return within %s: it is blocked on the named pipe", what, d)
	}
}

// A pipe as the input is refused by its kind before any tool or reader
// opens it: FAIL CLEAN_FAIL, no ffprobe run, the pipe left where it was.
// Both entry points check, as ingest hands the cleaner a result scan
// produced earlier and the entry may have been swapped since.
func TestNamedPipeInputRefused(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.mkv", "a.nfo"} {
		mkfifo(t, filepath.Join(dir, name))
	}
	c, tr := newCleaner(t, nil, mustProfile(t, "homelab"))
	for _, name := range []string{"a.mkv", "a.nfo"} {
		path := filepath.Join(dir, name)
		var direct, scanned report.FileResult
		within(t, fifoWait, "CleanFile", func() { direct = c.CleanFile(context.Background(), path) })
		within(t, fifoWait, "CleanScanned", func() {
			scanned = c.CleanScanned(context.Background(), scan.Result{File: report.FileResult{Path: path}})
		})
		for _, fr := range []report.FileResult{direct, scanned} {
			if fr.Verdict != report.Fail || !fr.Has(CodeCleanFail) {
				t.Errorf("%s: %s %v", name, fr.Verdict, codes(fr))
			}
			for _, f := range fr.Findings {
				if f.Code == CodeCleanFail && !strings.Contains(f.Message, "not a regular file") {
					t.Errorf("%s: the finding does not say what was refused: %q", name, f.Message)
				}
			}
		}
		if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("%s: the pipe was removed or replaced: %v", name, err)
		}
	}
	if len(tr.all()) != 0 {
		t.Fatalf("tools ran on a pipe: %v", tr.all())
	}
}

// Guarantee 3 and 1 for the MP4 rewrite: ffmpeg writes the temp file, and
// before the cleaner reads it back a pipe is swapped onto the temp name.
// Every read that follows (the probe, the stream hashes, the atom parse,
// the flush before placement) goes by that name, so the swap must be
// caught right after ffmpeg returns, before any of them opens it. The
// result is FAIL CLEAN_FAIL naming the pipe within seconds, ffprobe never
// runs on the temp name, the source keeps its bytes and identity, and no
// temp file is left behind.
func TestMp4RewriteRefusesPipeAtTemp(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge)
	src := testutil.Copy(t, "purchased.mp4")
	dir := filepath.Dir(src)
	srcBefore := fileSHA(t, src)
	if err := os.Chmod(src, 0o666); err != nil {
		t.Fatal(err)
	}
	srcStamp := time.Date(2010, 11, 12, 13, 14, 15, 0, time.UTC)
	if err := os.Chtimes(src, srcStamp, srcStamp); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".amuxify-purchased.mp4.tmp")
	ffmpegWrapper(t, r, "if [ -f "+shq(tmp)+" ]; then rm -f "+shq(tmp)+" && mkfifo "+shq(tmp)+"; fi")

	c, tr := newCleaner(t, nil, mustProfile(t, "homelab"))
	c.Timeout = 30 * time.Second
	var fr report.FileResult
	within(t, 30*time.Second, "CleanFile", func() { fr = c.CleanFile(context.Background(), src) })
	wroteTmp, readTmp := false, false
	for _, l := range tr.all() {
		if strings.Contains(l, "ffmpeg") && strings.Contains(l, "-y ") && strings.HasSuffix(l, tmp) {
			wroteTmp = true
		} else if strings.Contains(l, tmp) {
			readTmp = true
		}
	}
	if !wroteTmp {
		t.Fatalf("ffmpeg never wrote the temp name: %v", tr.all())
	}
	if readTmp {
		t.Fatalf("a tool was run on the temp name after the swap: %v", tr.all())
	}
	if fr.Verdict != report.Fail || !fr.Has(CodeCleanFail) || fr.Has(CodeMetadata) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	said := false
	for _, f := range fr.Findings {
		if f.Code == CodeCleanFail && strings.Contains(f.Message, "not the file this run created") && strings.Contains(f.Message, "original untouched") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the finding does not report the swap: %v", fr.Findings)
	}
	fi, err := os.Lstat(src)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("source is no longer a regular file: %v", err)
	}
	if fi.Mode().Perm() != 0o666 || !fi.ModTime().Equal(srcStamp) {
		t.Fatalf("source identity changed: mode %o mtime %v", fi.Mode().Perm(), fi.ModTime())
	}
	if fileSHA(t, src) != srcBefore {
		t.Fatal("source changed")
	}
	if _, err := os.Lstat(tmp); err == nil {
		t.Fatal("the planted pipe is still there")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}
