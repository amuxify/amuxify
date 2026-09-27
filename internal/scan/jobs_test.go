package scan

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/report"
)

// Guarantee 1 under parallel jobs: two blocked files from different roots
// that quarantine to one destination, moved at the same time, give exactly
// one quarantined file and one "quarantine failed", the loser stays where it
// was, and the winner's bytes are what sits in quarantine. Repeated, because
// a race that shows only sometimes is still a race.
func TestQuarantineSameNameFromTwoRootsOnce(t *testing.T) {
	noTools(t)
	q := filepath.Join(t.TempDir(), "quarantine")
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Quarantine = q
	for round := 0; round < 25; round++ {
		a := write(t, filepath.Join(t.TempDir(), "a", "x.url"), "[InternetShortcut]\nURL=http://a\n", 0o644)
		b := write(t, filepath.Join(t.TempDir(), "b", "x.url"), "[InternetShortcut]\nURL=http://b\n", 0o644)
		dest := filepath.Join(q, "x.url")
		var wg sync.WaitGroup
		results := make([]Result, 2)
		start := make(chan struct{})
		for i, p := range []string{a, b} {
			wg.Add(1)
			go func(i int, p string) {
				defer wg.Done()
				<-start
				results[i] = s.ScanFile(context.Background(), p, filepath.Dir(p))
			}(i, p)
		}
		close(start)
		wg.Wait()
		moved, failed := 0, 0
		var winner string
		for i, r := range results {
			expect(t, r.File, report.Block, CodeSidecarBlocked)
			for _, f := range r.File.Findings {
				if f.Code != CodeQuarantined {
					continue
				}
				switch {
				case f.Severity == report.Block && f.Message == "moved to "+dest:
					moved++
					winner = []string{a, b}[i]
				case f.Severity == report.Warn && f.Message == "quarantine failed: "+fsutil.ErrExists.Error():
					failed++
				default:
					t.Errorf("round %d: %+v", round, f)
				}
			}
		}
		if moved != 1 || failed != 1 {
			t.Fatalf("round %d: %d moved, %d failed", round, moved, failed)
		}
		got, err := os.ReadFile(dest)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), "URL=http://"+filepath.Base(filepath.Dir(winner))) {
			t.Errorf("round %d: quarantine holds %q, winner was %s", round, got, winner)
		}
		if _, err := os.Lstat(winner); err == nil {
			t.Errorf("round %d: winner's source still there", round)
		}
		loser := a
		if winner == a {
			loser = b
		}
		if _, err := os.Lstat(loser); err != nil {
			t.Errorf("round %d: loser's source is gone: %v", round, err)
		}
		entries, err := os.ReadDir(q)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 1 {
			t.Errorf("round %d: quarantine holds %d entries", round, len(entries))
		}
		if err := os.Remove(dest); err != nil {
			t.Fatal(err)
		}
	}
}

// ScanPath with several jobs returns the results in walk order, streams one
// result per file, keys two names of one inode to the same serial key, and
// on cancellation returns only the files it finished with the context's
// error, so the caller reports the run as interrupted.
func TestScanPathParallelOrderAndCancel(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	var want []string
	for _, n := range []string{"a.nfo", "b.nfo", "c.url", "d.nfo", "e.nfo", "f.nfo", "g.nfo", "h.nfo"} {
		want = append(want, write(t, filepath.Join(dir, n), "x\n", 0o644))
	}
	if err := os.Link(filepath.Join(dir, "a.nfo"), filepath.Join(dir, "a2.nfo")); err == nil {
		want = append(want, filepath.Join(dir, "a2.nfo"))
		s := newScanner(t, mustProfile(t, "homelab"), nil)
		ka := s.SerialKeys(filepath.Join(dir, "a.nfo"), dir)
		kb := s.SerialKeys(filepath.Join(dir, "a2.nfo"), dir)
		if len(ka) != 1 || !strings.HasPrefix(ka[0], "inode:") || ka[0] != kb[0] {
			t.Errorf("inode keys %v %v", ka, kb)
		}
		if k := s.SerialKeys(filepath.Join(dir, "b.nfo"), dir); len(k) != 0 {
			t.Errorf("single-link file has keys %v", k)
		}
		s.Quarantine = filepath.Join(t.TempDir(), "q")
		if k := s.SerialKeys(filepath.Join(dir, "C.URL"), dir); len(k) != 1 || k[0] != "quarantine:"+strings.ToLower(filepath.Join(s.Quarantine, "c.url")) {
			t.Errorf("quarantine key %v", k)
		}
	}
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Jobs = 4
	var mu sync.Mutex
	var streamed []string
	s.Progress = func(r Result) {
		mu.Lock()
		streamed = append(streamed, r.File.Path)
		mu.Unlock()
	}
	res, err := s.ScanPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(want) || len(streamed) != len(want) {
		t.Fatalf("%d results, %d streamed, want %d", len(res), len(streamed), len(want))
	}
	sorted := append([]string(nil), want...)
	sort.Strings(sorted)
	for i, r := range res {
		if r.File.Path != sorted[i] {
			t.Errorf("result %d is %s, want %s", i, r.File.Path, sorted[i])
		}
	}

	// Cancel from inside the first result that streams; the run starts no
	// more files, waits for the ones running, and returns their results.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var after int32
	var once sync.Once
	s.Progress = func(Result) {
		once.Do(cancel)
		mu.Lock()
		after++
		mu.Unlock()
	}
	res, err = s.ScanPath(ctx, dir)
	if err != context.Canceled {
		t.Fatalf("err %v, want context.Canceled", err)
	}
	if len(res) == 0 || len(res) == len(want) {
		t.Errorf("%d results after cancellation, want some but not all %d", len(res), len(want))
	}
	if int(after) != len(res) {
		t.Errorf("%d streamed but %d returned", after, len(res))
	}
	for i := 1; i < len(res); i++ {
		if res[i].File.Path <= res[i-1].File.Path {
			t.Errorf("results out of walk order: %s after %s", res[i].File.Path, res[i-1].File.Path)
		}
	}
}
