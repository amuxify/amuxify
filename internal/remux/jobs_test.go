package remux

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
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

// Two names of one inode are never rebuilt at the same time, and the report
// of a parallel run is the report of a sequential one. Every file is
// scanned before any is rebuilt, so with hardlinks=break in place both
// names carry the scan's "file has 2 hard links" and nlink of 2, exactly as
// a run with one job reports them; the pool then serialises the two names
// by inode, so the second is rebuilt after the first has broken the link
// and, like the sequential run, gets no "breaks the link" warning of its
// own. Had the two been rebuilt at once, each would have found the other's
// link intact and both would have carried the warning. The parallel report
// is compared finding by finding with the sequential one.
func TestParallelHardLinksSerialised(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	run := func(t *testing.T, jobs int) []report.FileResult {
		t.Helper()
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
		rm.Jobs, rm.InPlace, rm.Hardlinks = jobs, true, "break"
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
		for _, name := range []string{"a.mkv", "b.mkv"} {
			if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
				t.Errorf("jobs %d: %s missing", jobs, name)
			}
		}
		return res
	}
	seq := run(t, 1)
	par := run(t, 4)
	for i, want := range seq {
		got := par[i]
		if filepath.Base(got.Path) != filepath.Base(want.Path) || !reflect.DeepEqual(codes(got), codes(want)) || !reflect.DeepEqual(got.Info, want.Info) {
			t.Errorf("result %d differs: parallel %s %v %v, sequential %s %v %v", i, filepath.Base(got.Path), codes(got), got.Info, filepath.Base(want.Path), codes(want), want.Info)
		}
		for j := range want.Findings {
			if j < len(got.Findings) && got.Findings[j].Message != want.Findings[j].Message {
				t.Errorf("result %d finding %d: parallel %q, sequential %q", i, j, got.Findings[j].Message, want.Findings[j].Message)
			}
		}
	}
	// What both runs report, spelled out so a change in either would show:
	// the first name is scanned and rebuilt as hard-linked, the second is
	// scanned as hard-linked and rebuilt as a plain file.
	for _, res := range [][]report.FileResult{seq, par} {
		first, second := res[0], res[1]
		if filepath.Base(first.Path) != "a.mov" || !first.Has(CodePlaced) || first.Info["nlink"] != "2" {
			t.Errorf("a.mov: %v %v", codes(first), first.Info)
		}
		if !hasFinding(first, CodeHardlinked, report.Pass, "file has 2 hard links") || !hasFinding(first, CodeHardlinked, report.Warn, "hard-linked; replacing in place breaks the link") {
			t.Errorf("a.mov findings: %v", first.Findings)
		}
		if filepath.Base(second.Path) != "b.mov" || !second.Has(CodePlaced) || second.Info["nlink"] != "2" {
			t.Errorf("b.mov: %v %v", codes(second), second.Info)
		}
		if !hasFinding(second, CodeHardlinked, report.Pass, "file has 2 hard links") || hasFinding(second, CodeHardlinked, report.Warn, "hard-linked; replacing in place breaks the link") {
			t.Errorf("b.mov ran beside a.mov, or was scanned after a.mov broke the link: %v", second.Findings)
		}
	}
}

// foldsNormalisation reports whether the filesystem under dir treats the
// composed and the decomposed spelling of an accented name as one entry, as
// APFS does: a file is written under the composed spelling and looked up
// under the decomposed one.
func foldsNormalisation(t *testing.T, dir string) bool {
	t.Helper()
	nfc := filepath.Join(dir, ".probe-é")
	if err := os.WriteFile(nfc, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(nfc)
	a, err := os.Lstat(nfc)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Lstat(filepath.Join(dir, ".probe-é"))
	return err == nil && os.SameFile(a, b)
}

// Guarantee 1 and the promise that a parallel run decides as a sequential
// one, for two sources whose names differ only in Unicode normalisation.
// Café.mov is spelled with a composed é, as a Linux tool writes it, and
// Café.webm with a decomposed one, as Finder writes it; on APFS both
// rebuild to one directory entry Café.mkv and both temp names are one entry
// too. The two keys compare as different byte strings, so without the
// directory key both files run at once, both pass the destination check,
// and the loser fails at its temp file with REMUX_FAIL "file exists"
// instead of the OUTPUT_EXISTS a sequential run reports, with the winner
// decided by timing. With the key they run in walk order: the first is
// placed, the second meets it on disk. Both modes, in place and to an
// output tree, and repeated because a race that shows only sometimes is
// still a race. The keys themselves are checked on every filesystem; the
// rebuild only where the two spellings collide.
func TestParallelNormalisationCollisionSerialised(t *testing.T) {
	nfc, nfd := "Café.mov", "Café.webm"
	rm := &Remuxer{InPlace: true}
	dir, out := t.TempDir(), filepath.Join(t.TempDir(), "out")
	kc := rm.SerialKeys(filepath.Join(dir, nfc), dir, out)
	kd := rm.SerialKeys(filepath.Join(dir, nfd), dir, out)
	if len(kc) != 4 || len(kd) != 4 || kc[1] != kd[1] || kc[3] != kd[3] || kc[0] == kd[0] || kc[2] == kd[2] {
		t.Fatalf("keys %v and %v must share the two directory keys and nothing else", kc, kd)
	}
	if kc[1] != "dest-dir:"+strings.ToLower(out) || kc[3] != "dest-dir:"+strings.ToLower(dir) {
		t.Errorf("directory keys %q %q", kc[1], kc[3])
	}
	if !foldsNormalisation(t, dir) {
		t.Skip("this filesystem keeps the two spellings as two entries")
	}
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, inPlace := range []bool{false, true} {
		mode := "output"
		if inPlace {
			mode = "in place"
		}
		t.Run(mode, func(t *testing.T) {
			for round := 0; round < 3; round++ {
				dir := collisionPair(t, nfc, nfd)
				rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
				rm.Jobs, rm.InPlace = 4, inPlace
				destDir := dir
				if !inPlace {
					destDir = filepath.Join(t.TempDir(), "out")
					rm.OutputRoot = destDir
				}
				res, err := rm.RemuxPath(context.Background(), dir)
				if err != nil {
					t.Fatal(err)
				}
				if len(res) != 2 {
					t.Fatalf("round %d: %d results", round, len(res))
				}
				first, second := res[0], res[1]
				if !first.Has(CodePlaced) || first.Verdict >= report.Fail {
					t.Errorf("round %d: the first file in walk order, %s, was not placed: %s %v", round, filepath.Base(first.Path), first.Verdict, codes(first))
				}
				f, ok := finding(second, CodeOutputExists)
				if !ok || second.Verdict != report.Fail || second.Has(CodePlaced) || second.Has(CodeRemuxFail) || second.Output != "" {
					t.Errorf("round %d: the second file, %s, must report OUTPUT_EXISTS as a sequential run does: %s %v", round, filepath.Base(second.Path), second.Verdict, codes(second))
				} else if !strings.HasSuffix(f.Message, ".mkv already exists") {
					t.Errorf("round %d: %q", round, f.Message)
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
					if strings.HasSuffix(e.Name(), ".mkv") {
						mkv++
					}
				}
				if mkv != 1 {
					t.Errorf("round %d: %d .mkv outputs in %s, want 1", round, mkv, destDir)
				}
				if inPlace {
					if _, err := os.Lstat(second.Path); err != nil {
						t.Errorf("round %d: the refused source %s is gone", round, filepath.Base(second.Path))
					}
					if _, err := os.Lstat(first.Path); err == nil {
						t.Errorf("round %d: the winner's source %s is still there", round, filepath.Base(first.Path))
					}
				}
			}
		})
	}
}

// hasFinding reports whether fr carries a finding with exactly this code,
// severity and message.
func hasFinding(fr report.FileResult, code string, sev report.Severity, msg string) bool {
	for _, f := range fr.Findings {
		if f.Code == code && f.Severity == sev && f.Message == msg {
			return true
		}
	}
	return false
}

// Only a media file carries destination keys. A sidecar that shares its
// stem with a media file, Movie.nfo beside Movie.mkv, is skipped by
// RemuxScanned before any destination is computed, so serialising it behind
// the rebuild of Movie.mkv would only make it wait. The inode key stays for
// every regular file with more than one name, sidecar or not.
func TestSerialKeysOnlyMediaGetDestKeys(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	for _, name := range []string{"Movie.mkv", "Movie.nfo", "Movie.srt", "Movie.jpg", "Movie.avi"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	rm := &Remuxer{InPlace: true}
	want := "dest:" + strings.ToLower(filepath.Join(out, "Movie.mkv"))
	wantInPlace := "dest:" + strings.ToLower(filepath.Join(dir, "Movie.mkv"))
	for _, name := range []string{"Movie.mkv", "Movie.avi"} {
		keys := rm.SerialKeys(filepath.Join(dir, name), dir, out)
		if !reflect.DeepEqual(keys, []string{want, wantInPlace}) {
			t.Errorf("%s: keys %v", name, keys)
		}
	}
	for _, name := range []string{"Movie.nfo", "Movie.srt", "Movie.jpg"} {
		if keys := rm.SerialKeys(filepath.Join(dir, name), dir, out); len(keys) != 0 {
			t.Errorf("%s: keys %v, want none", name, keys)
		}
	}
	// A hard-linked sidecar keeps its inode key and gets nothing else.
	if err := os.Link(filepath.Join(dir, "Movie.nfo"), filepath.Join(dir, "Movie2.nfo")); err != nil {
		t.Skipf("hard links unsupported: %v", err)
	}
	keys := rm.SerialKeys(filepath.Join(dir, "Movie.nfo"), dir, out)
	if len(keys) != 1 || !strings.HasPrefix(keys[0], "inode:") {
		t.Errorf("hard-linked sidecar keys %v", keys)
	}
}

// Guarantee 1 across files of one run: a claim follows what is on disk. In
// place, ep.mov becomes ep.mkv under a new name and its source is checked
// once more after the output is placed; here the source is replaced in
// that window, so the placed output is removed again and the claim on
// ep.mkv must go with it. The next file with the same destination, ep.webm,
// then rebuilds to the free name instead of being refused with an
// OUTPUT_EXISTS that nothing on disk would justify.
func TestClaimReleasedWhenPlacedOutputRemovedAgain(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	dir := collisionPair(t, "ep.mov", "ep.webm")
	mov := filepath.Join(dir, "ep.mov")
	dest := filepath.Join(dir, "ep.mkv")
	swap := testutil.Copy(t, "multi.mkv")
	swapSum := fileSHA(t, swap)
	var swapped int32
	t.Cleanup(func() { beforePlace = nil })
	beforePlace = func(tmp, d string) {
		// Only the first placement, ep.mov's, sees its source replaced;
		// the rename onto the source changes its inode, which the last
		// source check notices once the output sits at dest.
		if d != dest || !atomic.CompareAndSwapInt32(&swapped, 0, 1) {
			return
		}
		if err := os.Rename(swap, mov); err != nil {
			t.Errorf("swap: %v", err)
		}
	}
	rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.InPlace = true
	// The claim is looked at the moment ep.mov's result is streamed, which
	// is after its remux has returned and before ep.webm starts.
	claimedAfterMov := false
	rm.Progress = func(fr report.FileResult) {
		if filepath.Base(fr.Path) == "ep.mov" {
			claimedAfterMov = rm.plannedCollision(dest)
		}
	}
	res, err := rm.RemuxPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || swapped != 1 {
		t.Fatalf("%d results, swapped %d", len(res), swapped)
	}
	first, second := res[0], res[1]
	if filepath.Base(first.Path) != "ep.mov" || first.Verdict != report.Fail || first.Has(CodePlaced) || first.Has(CodeOutputExists) || first.Output != "" {
		t.Fatalf("ep.mov: %s %v output=%q", first.Verdict, codes(first), first.Output)
	}
	said := false
	for _, f := range first.Findings {
		if f.Code == CodeRemuxFail && strings.Contains(f.Message, "was replaced") && strings.Contains(f.Message, "the placed output was removed again") {
			said = true
		}
	}
	if !said {
		t.Fatalf("ep.mov does not say the output was removed again: %v", first.Findings)
	}
	if claimedAfterMov {
		t.Error("ep.mkv stayed claimed after the placed output was removed again")
	}
	if filepath.Base(second.Path) != "ep.webm" || !second.Has(CodePlaced) || second.Output != dest || second.Has(CodeOutputExists) {
		t.Fatalf("ep.webm was not rebuilt to the freed name: %s %v output=%q", second.Verdict, codes(second), second.Output)
	}
	if !rm.plannedCollision(dest) {
		t.Error("the placed output of ep.webm is not claimed for the rest of the run")
	}
	// The swapped-in file sits untouched under ep.mov, ep.webm's rebuild is
	// at ep.mkv, ep.webm itself is gone and no temp file remains.
	if fileSHA(t, mov) != swapSum {
		t.Error("the file swapped onto ep.mov was replaced or removed")
	}
	if _, err := os.Lstat(filepath.Join(dir, "ep.webm")); err == nil {
		t.Error("ep.webm still there after its rebuild was placed")
	}
	if fi, err := os.Lstat(dest); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("ep.mkv: %v", err)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temp files left: %v", l)
	}
}

// Guarantee 1 when the pool's keys are not there to help: several workers
// call RemuxScanned for sources that map to one destination at the same
// time, with nothing serialising them, as a caller other than RemuxPath
// might. The first to claim the destination is held inside its temp-file
// window, before anything sits at the destination, until every other
// worker has returned; each of those must have been refused by the claim
// alone, with the OUTPUT_EXISTS message the disk would have produced, and
// none may have created, or removed, the temp name the winner is writing.
// mkvmerge runs exactly once and one output is placed.
func TestClaimGuardsConcurrentRemuxScanned(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, inPlace := range []bool{false, true} {
		mode := "output"
		if inPlace {
			mode = "in place"
		}
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			for name, as := range map[string]string{"sample.mov": "ep.mov", "sample.webm": "ep.webm", "sample.avi": "ep.avi", "sample.ts": "ep.ts"} {
				if err := os.Rename(testutil.Copy(t, name), filepath.Join(dir, as)); err != nil {
					t.Fatal(err)
				}
			}
			rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
			rm.InPlace = inPlace
			destDir := dir
			if !inPlace {
				destDir = filepath.Join(t.TempDir(), "out")
				rm.OutputRoot = destDir
				if err := os.MkdirAll(destDir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			dest := filepath.Join(destDir, "ep.mkv")
			tmp := fsutil.TempName(dest)
			scanned, err := rm.Scanner.ScanPath(context.Background(), dir)
			if err != nil || len(scanned) != 4 {
				t.Fatalf("scan: %v, %d results", err, len(scanned))
			}
			inputRoot, outRoot := rm.Roots(dir, true)

			inWindow := make(chan struct{})
			losersDone := make(chan struct{})
			var seamCalls int32
			t.Cleanup(func() { beforePlace = nil })
			beforePlace = func(_, _ string) {
				if atomic.AddInt32(&seamCalls, 1) != 1 {
					t.Errorf("a second worker reached placement")
					return
				}
				close(inWindow)
				<-losersDone
			}
			results := make(chan report.FileResult, len(scanned))
			start := make(chan struct{})
			var wg sync.WaitGroup
			for _, sc := range scanned {
				wg.Add(1)
				go func(sc scan.Result) {
					defer wg.Done()
					<-start
					results <- rm.RemuxScanned(context.Background(), sc, inputRoot, outRoot)
				}(sc)
			}
			close(start)
			<-inWindow
			// The winner is inside its window: its temp file exists, the
			// destination does not, and the three others return refused
			// while that is so.
			var losers []report.FileResult
			for len(losers) < len(scanned)-1 {
				losers = append(losers, <-results)
			}
			if _, err := os.Lstat(dest); err == nil {
				t.Errorf("%s exists while the winner is still in its window", dest)
			}
			if fi, err := os.Lstat(tmp); err != nil || !fi.Mode().IsRegular() {
				t.Errorf("the winner's temp file %s is gone: a loser removed it", tmp)
			}
			for _, fr := range losers {
				f, ok := finding(fr, CodeOutputExists)
				if !ok || fr.Verdict != report.Fail || f.Message != dest+" already exists" || fr.Has(CodePlaced) || fr.Output != "" {
					t.Errorf("%s: %s %v output=%q", filepath.Base(fr.Path), fr.Verdict, codes(fr), fr.Output)
				}
			}
			close(losersDone)
			wg.Wait()
			winner := <-results
			if !winner.Has(CodePlaced) || winner.Output != dest {
				t.Errorf("winner %s: %v output=%q", filepath.Base(winner.Path), codes(winner), winner.Output)
			}
			merges := 0
			for _, l := range tr.all() {
				if strings.Contains(l, " -o ") {
					merges++
				}
			}
			if merges != 1 {
				t.Errorf("%d mkvmerge runs, want 1:\n%s", merges, strings.Join(tr.writes(), "\n"))
			}
			if _, err := os.Lstat(dest); err != nil {
				t.Errorf("%s not placed: %v", dest, err)
			}
			if l := leftovers(t, dir, destDir); len(l) != 0 {
				t.Errorf("temp files left: %v", l)
			}
			if inPlace {
				remaining := 0
				for _, name := range []string{"ep.mov", "ep.webm", "ep.avi", "ep.ts"} {
					if _, err := os.Lstat(filepath.Join(dir, name)); err == nil {
						remaining++
					}
				}
				if remaining != 3 {
					t.Errorf("%d sources remain, want the three losers", remaining)
				}
			}
		})
	}
}
