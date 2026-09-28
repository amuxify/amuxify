//go:build unix

package ingest

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/remux"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
)

// Guarantee 3 for ingest: a named pipe under a media or sidecar name is
// refused by the scan step before anything opens it, with or without
// --force, and the run goes on to the other files. open(2) on a pipe
// blocks until a peer appears, and a planter never has to supply one, so
// the run must come back within seconds. Nothing in the tree changes and
// no tool runs.
func TestNamedPipeInputRefused(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	for _, name := range []string{"a.mkv", "b.nfo"} {
		if err := syscall.Mkfifo(filepath.Join(dir, name), 0o600); err != nil {
			t.Skipf("cannot create a named pipe: %v", err)
		}
	}
	write(t, filepath.Join(dir, "z.nfo"), "notes")
	before := snapshot(t, dir)
	for _, force := range []bool{false, true} {
		in, tr := newIngester(t, nil, mustProfile(t, "homelab"))
		in.Force = force
		in.apply(t)
		var res []report.FileResult
		done := make(chan struct{})
		go func() {
			defer close(done)
			var err error
			res, err = in.IngestPath(context.Background(), dir)
			if err != nil {
				t.Errorf("IngestPath: %v", err)
			}
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("IngestPath did not return within 5s: it is blocked on a named pipe")
		}
		got := byBase(res)
		for _, name := range []string{"a.mkv", "b.nfo"} {
			fr, ok := got[name]
			if !ok {
				t.Fatalf("force=%v: %s has no result", force, name)
			}
			if fr.Verdict != report.Fail || !fr.Has(scan.CodeUnreadable) || fr.Has(scan.CodeEmpty) {
				t.Errorf("force=%v: %s: %s %v", force, name, fr.Verdict, codes(fr))
			}
			if fr.Output != "" {
				t.Errorf("force=%v: %s: output %q", force, name, fr.Output)
			}
		}
		if fr := got["a.mkv"]; !fr.Has(remux.CodeRefused) {
			t.Errorf("force=%v: a.mkv was not refused: %v", force, codes(fr))
		}
		if _, ok := got["z.nfo"]; !ok {
			t.Errorf("force=%v: the run stopped at the pipes", force)
		}
		if len(tr.all()) != 0 {
			t.Errorf("force=%v: tools ran on a pipe: %v", force, tr.all())
		}
	}
	sameSnapshot(t, before, snapshot(t, dir))
	for _, name := range []string{"a.mkv", "b.nfo"} {
		if fi, err := os.Lstat(filepath.Join(dir, name)); err != nil || fi.Mode()&os.ModeNamedPipe == 0 {
			t.Errorf("%s: the pipe was removed or replaced: %v", name, err)
		}
	}
}
