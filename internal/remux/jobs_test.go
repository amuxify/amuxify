package remux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// The claim set hands one destination to exactly one of many concurrent
// claimants, folds case where the directory does, releases only the exact
// spelling it recorded, and keeps the plan and collision helpers the dry
// run tests use. Nothing here touches the disk.
func TestClaimExactlyOneWinner(t *testing.T) {
	fold := filepath.Join(string(filepath.Separator), "fold")
	exact := filepath.Join(string(filepath.Separator), "exact")
	rm := &Remuxer{caseFold: map[string]bool{fold: true, exact: false}}
	for round := 0; round < 20; round++ {
		var wins int32
		var wg sync.WaitGroup
		start := make(chan struct{})
		spellings := []string{"ep.mkv", "Ep.mkv", "EP.MKV"}
		for w := 0; w < 24; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				<-start
				if rm.claim(filepath.Join(fold, spellings[w%len(spellings)])) {
					atomic.AddInt32(&wins, 1)
				}
			}(w)
		}
		close(start)
		wg.Wait()
		if wins != 1 {
			t.Fatalf("round %d: %d winners in a folding directory", round, wins)
		}
		if !rm.plannedCollision(filepath.Join(fold, "eP.mKv")) {
			t.Fatal("the winner's claim is not visible as a collision")
		}
		// Only the spelling that was claimed can be released; releasing
		// another spelling is a no-op, so the claim stays.
		for _, s := range spellings {
			rm.release(filepath.Join(fold, s))
		}
		if rm.plannedCollision(filepath.Join(fold, "ep.mkv")) {
			t.Fatal("the claim survived its release")
		}
	}
	// An exact directory hands out the three spellings separately.
	for _, s := range []string{"ep.mkv", "Ep.mkv", "EP.MKV"} {
		if !rm.claim(filepath.Join(exact, s)) {
			t.Errorf("%s refused in an exact directory", s)
		}
	}
	if rm.claim(filepath.Join(exact, "Ep.mkv")) {
		t.Error("a taken name was claimed again")
	}
	rm.release(filepath.Join(exact, "Ep.mkv"))
	if !rm.claim(filepath.Join(exact, "Ep.mkv")) || rm.plannedCollision(filepath.Join(exact, "other.mkv")) {
		t.Error("release or collision check is not exact")
	}
	// plan records without checking, so the dry run helpers keep working.
	rm.plan(filepath.Join(exact, "Ep.mkv"))
	if !rm.plannedCollision(filepath.Join(exact, "Ep.mkv")) {
		t.Error("plan did not record")
	}
}

// Guarantee 1 under parallel jobs: several sources that rebuild to one
// destination, remuxed at the same time, leave exactly one output on disk,
// the others report OUTPUT_EXISTS, no temp file survives, and the run makes
// the same decisions as a sequential one. Repeated, because a race that
// shows only sometimes is still a race.
func TestParallelSameDestinationOnce(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, inPlace := range []bool{false, true} {
		mode := "output"
		if inPlace {
			mode = "in place"
		}
		t.Run(mode, func(t *testing.T) {
			for round := 0; round < 3; round++ {
				dir := t.TempDir()
				for name, as := range map[string]string{"sample.mov": "ep.mov", "sample.webm": "ep.webm", "sample.avi": "ep.avi", "sample.ts": "ep.ts"} {
					if err := os.Rename(testutil.Copy(t, name), filepath.Join(dir, as)); err != nil {
						t.Fatal(err)
					}
				}
				outRoot := filepath.Join(t.TempDir(), "out")
				rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
				rm.Jobs, rm.InPlace = 4, inPlace
				destDir := dir
				if !inPlace {
					rm.OutputRoot = outRoot
					destDir = outRoot
				}
				var streamed int32
				rm.Progress = func(report.FileResult) { atomic.AddInt32(&streamed, 1) }
				res, err := rm.RemuxPath(context.Background(), dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(res) != 4 || streamed != 4 {
					t.Fatalf("round %d: %d results, %d streamed", round, len(res), streamed)
				}
				placed, exists := 0, 0
				for _, fr := range res {
					switch {
					case fr.Has(CodePlaced) && fr.Output == filepath.Join(destDir, "ep.mkv"):
						placed++
					case fr.Has(CodeOutputExists) && fr.Verdict == report.Fail:
						exists++
					default:
						t.Errorf("round %d: %s: %v", round, filepath.Base(fr.Path), codes(fr))
					}
					if f, ok := finding(fr, CodeOutputExists); ok && f.Message != filepath.Join(destDir, "ep.mkv")+" already exists" {
						t.Errorf("round %d: %s: %q", round, filepath.Base(fr.Path), f.Message)
					}
				}
				if placed != 1 || exists != 3 {
					t.Errorf("round %d: %d placed, %d OUTPUT_EXISTS", round, placed, exists)
				}
				// The earliest name in walk order wins, as in a sequential
				// run, so the output is the rebuilt ep.avi.
				if !res[0].Has(CodePlaced) || filepath.Base(res[0].Path) != "ep.avi" {
					t.Errorf("round %d: first result %s %v", round, filepath.Base(res[0].Path), codes(res[0]))
				}
				entries, err := os.ReadDir(destDir)
				if err != nil {
					t.Fatal(err)
				}
				mkv := 0
				for _, e := range entries {
					if strings.HasPrefix(e.Name(), ".amuxify-") {
						t.Errorf("round %d: temp file %s left behind", round, e.Name())
					}
					if e.Name() == "ep.mkv" {
						mkv++
					}
				}
				if mkv != 1 {
					t.Errorf("round %d: %d ep.mkv in %s", round, mkv, destDir)
				}
				if inPlace {
					// The three losers keep their sources; the winner's
					// source is gone.
					for _, name := range []string{"ep.mov", "ep.webm", "ep.ts"} {
						if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
							t.Errorf("round %d: loser %s is gone", round, name)
						}
					}
					if _, err := os.Lstat(filepath.Join(dir, "ep.avi")); err == nil {
						t.Errorf("round %d: winner's source ep.avi still there", round)
					}
				}
			}
		})
	}
}

// Guarantee 1 across rounds of one run: a claim is given back when the
// remux placed nothing, so a later file with the same destination is not
// refused for a file that never wrote. Here the first file fails before
// mkvmerge runs because its source vanishes, and the second then rebuilds
// to the freed name.
func TestClaimReleasedWhenNothingPlaced(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	dir := collisionPair(t, "ep.mov", "ep.webm")
	outRoot := filepath.Join(t.TempDir(), "out")
	rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	results, err := rm.Scanner.ScanPath(context.Background(), dir)
	if err != nil || len(results) != 2 {
		t.Fatalf("scan: %v, %d results", err, len(results))
	}
	inputRoot, out := rm.Roots(dir, true)
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	// ep.mov is removed after its scan and before its remux, which fails
	// at the identity check and must free ep.mkv.
	if err := os.Remove(filepath.Join(dir, "ep.mov")); err != nil {
		t.Fatal(err)
	}
	mov := rm.RemuxScanned(context.Background(), results[0], inputRoot, out)
	if !mov.Has(CodeRemuxFail) || mov.Has(CodeOutputExists) {
		t.Fatalf("ep.mov: %v", codes(mov))
	}
	if rm.plannedCollision(filepath.Join(out, "ep.mkv")) {
		t.Fatal("ep.mkv stayed claimed by a remux that placed nothing")
	}
	webm := rm.RemuxScanned(context.Background(), results[1], inputRoot, out)
	if !webm.Has(CodePlaced) || webm.Output != filepath.Join(out, "ep.mkv") {
		t.Fatalf("ep.webm: %v %q", codes(webm), webm.Output)
	}
	if !rm.plannedCollision(filepath.Join(out, "ep.mkv")) {
		t.Error("a placed output is not claimed for the rest of the run")
	}
}

// Two names of one inode are never rebuilt at the same time: the pool
// serialises them by inode, so with hardlinks=break in place the second
// name finds the link already broken by the first and is handled as a plain
// file, exactly as a sequential run does. Had the two run at once, the
// second would have seen two links as well and reported HARDLINKED too.
func TestParallelHardLinksSerialised(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.mov")
	if err := os.Rename(testutil.Copy(t, "sample.mov"), a); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(dir, "b.mov")
	if err := os.Link(a, b); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.Jobs, rm.InPlace, rm.Hardlinks = 4, true, "break"
	keys := rm.SerialKeys(a, dir, dir)
	keysB := rm.SerialKeys(b, dir, dir)
	if len(keys) == 0 || !strings.HasPrefix(keys[0], "inode:") || keys[0] != keysB[0] {
		t.Fatalf("inode keys %v %v", keys, keysB)
	}
	res, err := rm.RemuxPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("%d results", len(res))
	}
	first, second := res[0], res[1]
	if filepath.Base(first.Path) != "a.mov" || !first.Has(CodePlaced) || !first.Has(CodeHardlinked) {
		t.Errorf("a.mov: %v", codes(first))
	}
	if filepath.Base(second.Path) != "b.mov" || !second.Has(CodePlaced) || second.Has(CodeHardlinked) {
		t.Errorf("b.mov ran beside a.mov or was not rebuilt: %v", codes(second))
	}
	for _, name := range []string{"a.mkv", "b.mkv"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing", name)
		}
	}
}
