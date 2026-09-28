//go:build unix

package remux

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

// Guarantee 3 for the remuxer: a named pipe swapped onto the temp name, or
// given as the input, must be refused promptly and never waited on.
// open(2) on a pipe blocks until a peer appears, and a planter never has
// to supply one.

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

// A pipe under a media name is refused by scan before any tool opens it,
// and the remuxer never gets past that: FAIL UNREADABLE naming the pipe,
// nothing written, the pipe left where it was.
func TestNamedPipeInputRefused(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	fifo := filepath.Join(dir, "a.mkv")
	mkfifo(t, fifo)
	outRoot := filepath.Join(t.TempDir(), "out")
	for _, force := range []bool{false, true} {
		rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
		rm.OutputRoot = outRoot
		rm.Force = force
		var res []report.FileResult
		within(t, fifoWait, "RemuxPath", func() {
			var err error
			res, err = rm.RemuxPath(context.Background(), dir)
			if err != nil {
				t.Errorf("RemuxPath: %v", err)
			}
		})
		if len(res) != 1 {
			t.Fatalf("%d results", len(res))
		}
		fr := res[0]
		if fr.Verdict != report.Fail || !fr.Has(scan.CodeUnreadable) || fr.Has(CodePlaced) || fr.Output != "" {
			t.Fatalf("force=%v: %s %v output=%q", force, fr.Verdict, codes(fr), fr.Output)
		}
		if len(tr.all()) != 0 {
			t.Fatalf("force=%v: tools ran on a pipe: %v", force, tr.all())
		}
	}
	if fi, err := os.Lstat(fifo); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
		t.Fatalf("the pipe was removed or replaced: %v", err)
	}
	if l := leftovers(t, dir, outRoot); len(l) != 0 {
		t.Fatalf("files left: %v", l)
	}
}

// After mkvmerge has written the output, a pipe is swapped onto the temp
// name. Every step that follows (mkvpropedit, the flush, the identity
// copy, the placement) refers to the file by that name, so the swap must
// be caught as soon as mkvmerge returns: FAIL REMUX_FAIL within seconds,
// no tool run on the temp name after the swap, the source untouched and
// nothing placed or left behind.
func TestTempSwappedForPipeAfterMkvmerge(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, tc := range []struct {
		name    string
		inPlace bool
	}{
		{"output tree", false},
		{"in place", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := testutil.Copy(t, "clean.mkv")
			dir := filepath.Dir(src)
			srcBefore := fileSHA(t, src)
			outRoot := filepath.Join(t.TempDir(), "out")
			tmpDir := outRoot
			if tc.inPlace {
				tmpDir = dir
			}
			tmp := filepath.Join(tmpDir, ".amuxify-clean.mkv.tmp")
			mkvmergeWrapper(t, r, "", "rm -f "+shq(tmp)+" && mkfifo "+shq(tmp))
			rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
			rm.InPlace = tc.inPlace
			if !tc.inPlace {
				rm.OutputRoot = outRoot
			}
			var res []report.FileResult
			within(t, 30*time.Second, "RemuxPath", func() {
				var err error
				res, err = rm.RemuxPath(context.Background(), dir)
				if err != nil {
					t.Errorf("RemuxPath: %v", err)
				}
			})
			if len(res) != 1 {
				t.Fatalf("%d results", len(res))
			}
			fr := res[0]
			wrote, after := false, []string{}
			for _, l := range tr.all() {
				switch {
				case strings.Contains(l, " -o ") && strings.Contains(l, tmp):
					wrote = true
				case wrote && strings.Contains(l, tmp):
					after = append(after, l)
				}
			}
			if !wrote {
				t.Fatalf("mkvmerge never wrote the temp name: %v", tr.all())
			}
			if len(after) != 0 {
				t.Fatalf("a tool was run on the temp name after the swap: %v", after)
			}
			if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) || fr.Output != "" {
				t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
			}
			said := false
			for _, f := range fr.Findings {
				if f.Code == CodeRemuxFail && strings.Contains(f.Message, "not the file this run created") && strings.Contains(f.Message, "nothing was placed") {
					said = true
				}
			}
			if !said {
				t.Fatalf("the finding does not report the swap: %v", fr.Findings)
			}
			if fileSHA(t, src) != srcBefore {
				t.Fatal("source changed")
			}
			if _, err := os.Lstat(tmp); err == nil {
				t.Fatal("the planted pipe is still there")
			}
			if l := leftovers(t, dir, outRoot); len(l) != 0 {
				t.Fatalf("files left: %v", l)
			}
		})
	}
}

// In place, the identity copy opens the temp name for writing after
// mkvmerge and mkvpropedit are done with it. A pipe swapped onto the name
// in that window is refused by kind at once, the placement is never
// reached, and the source keeps its bytes and identity.
func TestTakeIdentityRefusesPipeAtTemp(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	src := testutil.Copy(t, "clean.mkv")
	dir := filepath.Dir(src)
	srcBefore := fileSHA(t, src)
	if err := os.Chmod(src, 0o666); err != nil {
		t.Fatal(err)
	}
	srcStamp := time.Date(2010, 11, 12, 13, 14, 15, 0, time.UTC)
	if err := os.Chtimes(src, srcStamp, srcStamp); err != nil {
		t.Fatal(err)
	}
	swapped := false
	t.Cleanup(func() { beforeIdentity, beforePlace = nil, nil })
	beforeIdentity = func(tmp, _ string) {
		if err := os.Remove(tmp); err != nil {
			t.Fatalf("swap: %v", err)
		}
		if err := syscall.Mkfifo(tmp, 0o600); err != nil {
			t.Fatalf("swap: %v", err)
		}
		swapped = true
	}
	beforePlace = func(tmp, _ string) { t.Errorf("placement reached after the swap at %s", tmp) }
	rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.InPlace = true
	var res []report.FileResult
	within(t, 30*time.Second, "RemuxPath", func() {
		var err error
		res, err = rm.RemuxPath(context.Background(), dir)
		if err != nil {
			t.Errorf("RemuxPath: %v", err)
		}
	})
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	fr := res[0]
	if len(tr.writes()) == 0 {
		t.Fatalf("mkvmerge never ran: %v", codes(fr))
	}
	if !swapped {
		t.Fatalf("the seam never ran: %v", codes(fr))
	}
	if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) || fr.Output != "" {
		t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
	}
	said := false
	for _, f := range fr.Findings {
		if f.Code == CodeRemuxFail && strings.Contains(f.Message, "named pipe") && strings.Contains(f.Message, "nothing was placed") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the finding does not name the pipe: %v", fr.Findings)
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
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("files left: %v", l)
	}
}

// A pipe swapped onto the temp name before mkvmerge opens it parks the
// tool in open(2), and amuxify cannot see inside the tool. What bounds the
// wait is the tool timeout: the run must come back as FAIL REMUX_FAIL
// "timed out" soon after it, with the source untouched. The wrapper execs
// the real mkvmerge so the kill reaches the blocked process itself.
func TestMkvmergeBlockedOnPipeIsKilled(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	real, err := r.Path(exec.MKVMerge)
	if err != nil {
		t.Fatal(err)
	}
	src := testutil.Copy(t, "clean.mkv")
	dir := filepath.Dir(src)
	srcBefore := fileSHA(t, src)
	outRoot := filepath.Join(t.TempDir(), "out")
	tmp := filepath.Join(outRoot, ".amuxify-clean.mkv.tmp")
	script := filepath.Join(t.TempDir(), "mkvmerge")
	body := "#!/bin/sh\nfor a in \"$@\"; do\n" +
		"if [ \"$a\" = " + shq(tmp) + " ]; then rm -f " + shq(tmp) + " && mkfifo " + shq(tmp) + "; fi\ndone\n" +
		"exec " + shq(real) + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_MKVMERGE", script)
	rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	rm.Timeout = 3 * time.Second
	var res []report.FileResult
	start := time.Now()
	within(t, 30*time.Second, "RemuxPath", func() {
		var err error
		res, err = rm.RemuxPath(context.Background(), dir)
		if err != nil {
			t.Errorf("RemuxPath: %v", err)
		}
	})
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	fr := res[0]
	if len(tr.writes()) == 0 {
		t.Fatalf("mkvmerge never ran: %v", codes(fr))
	}
	if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) || fr.Output != "" {
		t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
	}
	said := false
	for _, f := range fr.Findings {
		if f.Code == CodeRemuxFail && strings.Contains(f.Message, "timed out") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the finding does not report the timeout after %s: %v", time.Since(start), fr.Findings)
	}
	if fileSHA(t, src) != srcBefore {
		t.Fatal("source changed")
	}
	if l := leftovers(t, dir, outRoot); len(l) != 0 {
		t.Fatalf("files left: %v", l)
	}
}
