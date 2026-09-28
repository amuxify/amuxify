package scan

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

// Guarantee 1 for the quarantine claim itself, with the disk taken out of
// the decision. The move primitive is held open for the first file to
// quarantine, so that nothing sits at the destination yet, and a second
// file that maps to the same place is scanned while that is so. It must be
// Two blocked files whose names differ only in Unicode normalisation, the
// composed and the decomposed spelling of an accented letter, quarantine to
// one directory entry on a filesystem such as APFS, while their quarantine
// keys compare as different byte strings. Each therefore also carries the
// key of its quarantine directory, so they are moved one after the other in
// walk order rather than at the same time; an ASCII name beside them shares
// neither key and keeps its parallelism. Nothing here touches the disk.
func TestSerialKeysNormalisationSharesDirectoryKey(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Quarantine = filepath.Join(t.TempDir(), "q")
	nfc := s.SerialKeys(filepath.Join(dir, "sub", "Caf\u00e9.url"), dir)
	nfd := s.SerialKeys(filepath.Join(dir, "sub", "Cafe\u0301.lnk"), dir)
	if len(nfc) != 2 || len(nfd) != 2 || nfc[0] == nfd[0] || nfc[1] != nfd[1] {
		t.Fatalf("keys %v and %v must share the directory key and nothing else", nfc, nfd)
	}
	if want := "quarantine-dir:" + strings.ToLower(filepath.Join(s.Quarantine, "sub")); nfc[1] != want {
		t.Errorf("directory key %q, want %q", nfc[1], want)
	}
	plain := s.SerialKeys(filepath.Join(dir, "sub", "Plain.url"), dir)
	if len(plain) != 1 || plain[0] != "quarantine:"+strings.ToLower(filepath.Join(s.Quarantine, "sub", "plain.url")) {
		t.Errorf("ASCII name keys %v", plain)
	}
}

// refused by the run-wide claim, with the words the disk would use, before
// it reaches the directory creation or the move; the primitive runs exactly
// once, the first file is moved once the hold is lifted, and the second
// stays where it was. Without the claim the second file would reach the
// move as well and the outcome would rest on the primitive's atomicity
// alone, which the cross-device copy path cannot offer for its temp file.
func TestQuarantineClaimRefusesBeforeDisk(t *testing.T) {
	noTools(t)
	q := filepath.Join(t.TempDir(), "quarantine")
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Quarantine = q
	a := write(t, filepath.Join(t.TempDir(), "a", "x.url"), "[InternetShortcut]\nURL=http://a\n", 0o644)
	b := write(t, filepath.Join(t.TempDir(), "b", "x.url"), "[InternetShortcut]\nURL=http://b\n", 0o644)
	dest := filepath.Join(q, "x.url")

	held := make(chan struct{})
	release := make(chan struct{})
	var places int32
	orig := fsutil.Place
	t.Cleanup(func() { fsutil.Place = orig })
	fsutil.Place = func(src, dst string) error {
		if atomic.AddInt32(&places, 1) == 1 {
			close(held)
			<-release
		}
		return orig(src, dst)
	}
	first := make(chan Result, 1)
	go func() { first <- s.ScanFile(context.Background(), a, filepath.Dir(a)) }()
	<-held
	// The first file holds the claim and is inside the move; the
	// destination does not exist. The second file is scanned now.
	if _, err := os.Lstat(dest); err == nil {
		t.Fatalf("%s exists before the move ran", dest)
	}
	second := s.ScanFile(context.Background(), b, filepath.Dir(b))
	expect(t, second.File, report.Block, CodeSidecarBlocked)
	refused := false
	for _, f := range second.File.Findings {
		if f.Code == CodeQuarantined && f.Severity == report.Warn && f.Message == "quarantine failed: "+fsutil.ErrExists.Error() {
			refused = true
		}
	}
	if !refused {
		t.Errorf("the second file was not refused by the claim: %v", second.File.Findings)
	}
	if n := atomic.LoadInt32(&places); n != 1 {
		t.Errorf("the move primitive ran %d times while the first file held the claim; the second reached the disk", n)
	}
	if _, err := os.Lstat(dest); err == nil {
		t.Errorf("%s exists after the refused file was scanned", dest)
	}
	if _, err := os.Lstat(b); err != nil {
		t.Errorf("the refused file is gone from %s: %v", b, err)
	}
	close(release)
	r := <-first
	expect(t, r.File, report.Block, CodeSidecarBlocked, CodeQuarantined)
	moved := false
	for _, f := range r.File.Findings {
		if f.Code == CodeQuarantined && f.Severity == report.Block && f.Message == "moved to "+dest {
			moved = true
		}
	}
	if !moved {
		t.Errorf("the first file was not moved: %v", r.File.Findings)
	}
	got, err := os.ReadFile(dest)
	if err != nil || !strings.Contains(string(got), "URL=http://a") {
		t.Errorf("quarantine holds %q, %v", got, err)
	}
	if _, err := os.Lstat(a); err == nil {
		t.Error("the moved file is still at its source")
	}
	// The claim was given back after the move, so a later file with the
	// same destination is answered by the disk, in the same words.
	c := write(t, filepath.Join(t.TempDir(), "c", "x.url"), "[InternetShortcut]\nURL=http://c\n", 0o644)
	third := s.ScanFile(context.Background(), c, filepath.Dir(c))
	refused = false
	for _, f := range third.File.Findings {
		if f.Code == CodeQuarantined && f.Severity == report.Warn && f.Message == "quarantine failed: "+fsutil.ErrExists.Error() {
			refused = true
		}
	}
	if !refused || atomic.LoadInt32(&places) != 2 {
		t.Errorf("a later file with the same destination: %v, %d moves", third.File.Findings, places)
	}
}

// clamscan loads its whole signature database on every start, so a
// parallel run must not start one per worker. Six media files are scanned
// with four jobs and a clamscan stub that records any overlap with another
// instance of itself; the stub is never found running twice at once, and
// every file still gets its clamscan. The stub reads and writes only paths
// baked into its body, because the runner starts tools with a clean
// environment.
func TestClamScanRunsOneAtATime(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	var paths []string
	for _, n := range []string{"a.mkv", "b.mkv", "c.mkv", "d.mkv", "e.mkv", "f.mkv"} {
		paths = append(paths, write(t, filepath.Join(dir, n), ebml+strings.Repeat("\x00", 512), 0o644))
	}
	state := t.TempDir()
	lock := filepath.Join(state, "running")
	overlap := filepath.Join(state, "overlap")
	count := filepath.Join(state, "count")
	stub := filepath.Join(state, "clamscan")
	body := "#!/bin/sh\n" +
		"if mkdir " + shq(lock) + " 2>/dev/null; then\n" +
		"  sleep 0.2\n" +
		"  rmdir " + shq(lock) + "\n" +
		"else\n" +
		"  : > " + shq(overlap) + "\n" +
		"fi\n" +
		"echo \"$@\" >> " + shq(count) + "\n" +
		"exit 0\n"
	if err := os.WriteFile(stub, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_CLAMSCAN", stub)
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.ClamAV = true
	s.Jobs = 4
	res, err := s.ScanPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(paths) {
		t.Fatalf("%d results, want %d", len(res), len(paths))
	}
	for _, r := range res {
		if r.File.Has(CodeClamError) || r.File.Has(CodeClamMissing) {
			t.Errorf("%s: %v", filepath.Base(r.File.Path), codes(r.File))
		}
	}
	if _, err := os.Lstat(overlap); err == nil {
		t.Fatal("two clamscan processes ran at the same time")
	}
	ran, err := os.ReadFile(count)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(ran), "\n"); n != len(paths) {
		t.Errorf("clamscan ran %d times, want %d:\n%s", n, len(paths), ran)
	}
	// A cancelled run does not wait for a slot.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	clamSlots <- struct{}{}
	defer func() { <-clamSlots }()
	fr := s.ScanFile(ctx, paths[0], dir).File
	if !fr.Has(CodeClamError) {
		t.Errorf("a cancelled run did not report the wait it gave up: %v", codes(fr))
	}
}

// shq quotes s for a POSIX shell.
