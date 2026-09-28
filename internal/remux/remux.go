// Package remux rebuilds a file into a sanitized Matroska container with
// mkvmerge, then proves the result: every kept stream hashes identically to
// the source, the structure matches the decision, and the head and tail
// decode. Only then is the output placed, never over an existing file.
package remux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/pool"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/verify"
)

// Finding codes.
const (
	CodeSkipped          = "SKIPPED"
	CodeRefused          = "REFUSED"
	CodeOutputExists     = "OUTPUT_EXISTS"
	CodeUnsupportedInput = "UNSUPPORTED_INPUT"
	CodeRemuxFail        = "REMUX_FAIL"
	CodeRemuxWarning     = "REMUX_WARNING"
	CodeHashMismatch     = "HASH_MISMATCH"
	CodeHashOK           = "HASH_OK"
	CodeVerifyFail       = "VERIFY_FAIL"
	CodeHDRLost          = "HDR_LOST"
	CodeDecodeFail       = "DECODE_FAIL"
	CodeHardlinked       = "HARDLINKED"
	CodeDryRun           = "DRY_RUN"
	CodePlaced           = "PLACED"
	CodeTrack            = "TRACK"
	CodeAttachment       = "ATTACHMENT"
)

// Remuxer performs remux runs.
type Remuxer struct {
	Runner   *exec.Runner
	Prober   *probe.Prober
	Verifier *verify.Verifier
	Scanner  *scan.Scanner
	Profile  *policy.Profile

	OutputRoot string // mirrored output tree; empty means <root>__remuxed
	InPlace    bool
	Hardlinks  string // skip | break | copy; empty means profile value
	VerifyTier string // quick | full | none; empty means profile value
	Force      bool   // proceed on FAIL scan findings (never on BLOCK)
	DryRun     bool
	Original   string // original language hint for default-audio selection
	Timeout    time.Duration
	// Progress, when set, receives each result as soon as the file is done.
	// With Jobs above one it is called from several goroutines, one file at
	// a time each, so it must be safe to call concurrently.
	Progress func(report.FileResult)
	// Jobs is the number of files RemuxPath works on at once; zero or one
	// means one after the other in walk order.
	Jobs int

	// claimed holds, per directory, the output names this run has taken:
	// in a dry run the names earlier files would write, in a live run the
	// names of outputs being built or already placed. A later file mapping
	// to one of them is reported with the OUTPUT_EXISTS the disk would
	// produce, such as sample.mp4 and sample.avi both becoming sample.mkv,
	// and with parallel jobs it is what keeps two workers from building the
	// same output through the same temp name at the same time. One Remuxer
	// is one run, and RemuxPath does not reset it because the command calls
	// RemuxPath once per path named on the command line. caseFold caches,
	// per directory, whether its filesystem folds case, which decides
	// whether Ep.mkv and ep.mkv are one name there. mu guards both.
	mu       sync.Mutex
	claimed  map[string][]string
	caseFold map[string]bool
}

func (r *Remuxer) hardlinks() string {
	if r.Hardlinks != "" {
		return r.Hardlinks
	}
	return r.Profile.Safety.Hardlinks
}

func (r *Remuxer) tier() string {
	if r.VerifyTier != "" {
		return r.VerifyTier
	}
	return r.Profile.Verify.Tier
}

func (r *Remuxer) timeout() time.Duration {
	if r.Timeout == 0 {
		return 6 * time.Hour
	}
	return r.Timeout
}

// Roots returns the tree relative paths are computed against and the output
// tree used by output mode and by the copy hard-link mode.
func (r *Remuxer) Roots(abs string, isDir bool) (inputRoot, outRoot string) {
	inputRoot = abs
	if !isDir {
		inputRoot = filepath.Dir(abs)
	}
	outRoot = r.OutputRoot
	if outRoot == "" {
		outRoot = filepath.Join(filepath.Dir(inputRoot), filepath.Base(inputRoot)+"__remuxed")
	}
	return inputRoot, outRoot
}

// RemuxPath processes a file or a directory tree.
func (r *Remuxer) RemuxPath(ctx context.Context, root string) ([]report.FileResult, error) {
	abs, err := fsutil.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	inputRoot, outRoot := r.Roots(abs, fi.IsDir())
	if !r.InPlace && !r.DryRun {
		if err := r.ensureOutRoot(outRoot); err != nil {
			return nil, err
		}
	}
	if r.tier() == "none" && r.InPlace {
		return nil, fmt.Errorf("refusing --in-place together with verify tier none")
	}
	if err := scan.CheckQuarantineRoot(abs, r.Scanner.Quarantine); err != nil {
		return nil, err
	}
	scanRoot := abs
	if !fi.IsDir() {
		scanRoot = filepath.Dir(abs)
	}
	// A tree with an unreadable corner still yields every readable file;
	// the walk error naming the corner is returned after them so the run is
	// reported as FAIL at run level, the same as scan, clean and ingest do.
	paths, walkErr := scan.Walk(abs, scan.QuarantineExcludes(r.Scanner.Quarantine)...)
	if walkErr != nil && len(paths) == 0 {
		return nil, walkErr
	}
	// Every file is scanned before any file is rebuilt, whatever Jobs says.
	// The scan is the record of the tree as the run found it: with
	// --in-place and hardlinks set to break, the second name of a
	// hard-linked pair is reported with the two links it had when the run
	// began, even though the rebuild of the first name has broken the link
	// by the time the second is remuxed. Scanning and remuxing each file in
	// one step would report the second name as a plain file, and the
	// report would then depend on how far the run had got. The scan phase
	// changes nothing on disk unless the scanner quarantines, which the
	// scanner's own keys and claims order; each remux result is still
	// streamed the moment that file is done.
	scanKeys := make([][]string, len(paths))
	for i, p := range paths {
		scanKeys[i] = r.Scanner.SerialKeys(p, scanRoot)
	}
	scanned := make([]scan.Result, len(paths))
	ran := pool.Run(ctx, r.Jobs, len(paths), scanKeys, func(i int) {
		sc := r.Scanner.ScanFile(ctx, paths[i], scanRoot)
		if r.Scanner.Progress != nil {
			r.Scanner.Progress(sc)
		}
		scanned[i] = sc
	})
	if !all(ran) {
		return nil, ctx.Err()
	}
	keys := make([][]string, len(paths))
	for i, p := range paths {
		keys[i] = r.SerialKeys(p, inputRoot, outRoot)
	}
	results := make([]report.FileResult, len(paths))
	ran = pool.Run(ctx, r.Jobs, len(paths), keys, func(i int) {
		fr := r.RemuxScanned(ctx, scanned[i], inputRoot, outRoot)
		if r.Progress != nil {
			r.Progress(fr)
		}
		results[i] = fr
	})
	var out []report.FileResult
	for i, ok := range ran {
		if ok {
			out = append(out, results[i])
		}
	}
	if len(out) < len(paths) {
		return out, ctx.Err()
	}
	return out, walkErr
}

// all reports whether every item of a pool run ran.
func all(ran []bool) bool {
	for _, ok := range ran {
		if !ok {
			return false
		}
	}
	return true
}

// SerialKeys lists what keeps path from running beside another file of the
// same walk: its inode when it has other hard links, so two names of one
// file are handled one after the other, and the outputs it could map to,
// spelled in lower case, so files that would collide on one destination are
// decided in walk order and the earlier one wins, as in a sequential run.
// A destination whose name holds a character outside ASCII also keys its
// directory, because a filesystem may treat two Unicode spellings of that
// name as one entry; pool.PathKeys explains the rule. Only a media file gets
// destination keys: RemuxScanned skips every other file before it computes
// a destination, so a sidecar that shares its stem with a media file,
// Movie.nfo beside Movie.mkv, need not wait for that file's rebuild. The
// claim set guards the destinations whatever the keys say; the keys make
// the outcome deterministic.
func (r *Remuxer) SerialKeys(path, inputRoot, outRoot string) []string {
	var keys []string
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() {
		if k := fsutil.InodeKey(fi); k != "" {
			keys = append(keys, "inode:"+k)
		}
	}
	if !scan.IsMedia(path) {
		return keys
	}
	rel, err := filepath.Rel(inputRoot, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	stem := strings.TrimSuffix(rel, filepath.Ext(rel)) + ".mkv"
	keys = append(keys, pool.PathKeys("dest", filepath.Join(outRoot, stem))...)
	if r.InPlace {
		keys = append(keys, pool.PathKeys("dest", filepath.Join(filepath.Dir(path), strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))+".mkv"))...)
	}
	return keys
}

// RemuxScanned remuxes one already scanned file. r.Scanner may be nil.
func (r *Remuxer) RemuxScanned(ctx context.Context, sc scan.Result, inputRoot, outRoot string) report.FileResult {
	start := time.Now()
	fr := sc.File
	defer func() { fr.Duration = time.Since(start) }()

	if sc.Info == nil || !scan.IsMedia(fr.Path) {
		if fr.Verdict < report.Fail {
			fr.Addf(CodeSkipped, report.Pass, "not a media container; scanned only")
		}
		return fr
	}
	if fr.Verdict >= report.Block {
		fr.Addf(CodeRefused, report.Block, "scan blocked this file; not remuxed")
		return fr
	}
	if fr.Verdict >= report.Fail && !r.Force {
		fr.Addf(CodeRefused, report.Fail, "scan failed this file; use --force to remux anyway")
		return fr
	}
	if sc.Info.Container != "matroska" && sc.Info.Container != "webm" && !sc.Info.MkvSupported {
		fr.Addf(CodeUnsupportedInput, report.Fail, "mkvmerge cannot read this %s file", sc.Info.Container)
		return fr
	}
	if len(sc.Info.StreamsOf("video")) == 0 {
		fr.Addf(CodeSkipped, report.Warn, "no video stream; remux handles video containers only")
		return fr
	}

	// The verdict describes the file the scanner examined, and this remux
	// may begin long after that scan: RemuxPath scans the whole tree before
	// it rebuilds any file, and under ingest the routing sits between. The
	// entry at the path must still be that file, with the size and the
	// modification time the scanner saw, before the verdict is acted on. A
	// file swapped onto the path since was never scanned, and rebuilding it
	// on the strength of the earlier verdict would carry it past the scan
	// (guarantee 9). fi is that entry, and every later check requires the
	// same file.
	fi, err := scannedFile(fr.Path, sc)
	if err != nil {
		fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
		return fr
	}

	// Destination.
	rel, err := filepath.Rel(inputRoot, fr.Path)
	if err != nil {
		rel = filepath.Base(fr.Path)
	}
	rel = strings.TrimSuffix(rel, filepath.Ext(rel)) + ".mkv"
	dest := filepath.Join(outRoot, rel)
	inPlace := r.InPlace
	if inPlace && fsutil.Nlink(fi) > 1 {
		switch r.hardlinks() {
		case "skip":
			fr.Addf(CodeHardlinked, report.Warn, "file has %d hard links; skipped (safety.hardlinks = skip)", fsutil.Nlink(fi))
			return fr
		case "copy":
			inPlace = false
			fr.Addf(CodeHardlinked, report.Pass, "hard-linked; output written to %s instead of in place", outRoot)
		default:
			fr.Addf(CodeHardlinked, report.Warn, "hard-linked; replacing in place breaks the link")
		}
	}
	// sameEntry is set when dest and fr.Path are two spellings of one
	// directory entry, which is what X.MKV against X.mkv is on a
	// case-insensitive filesystem. That is not a collision: the verified
	// output replaces the source under its own name and takes the .mkv
	// spelling last. A hard-linked twin that really is spelled X.mkv is a
	// distinct entry and stays a collision.
	sameEntry := false
	if inPlace {
		dest = filepath.Join(filepath.Dir(fr.Path), strings.TrimSuffix(filepath.Base(fr.Path), filepath.Ext(fr.Path))+".mkv")
		if dest != fr.Path {
			if dfi, err := os.Lstat(dest); err == nil {
				if os.SameFile(fi, dfi) && !hasEntry(filepath.Dir(dest), filepath.Base(dest)) {
					sameEntry = true
				} else {
					fr.Addf(CodeOutputExists, report.Fail, "%s already exists", dest)
					return fr
				}
			}
		}
	} else if _, err := os.Lstat(dest); err == nil {
		fr.Addf(CodeOutputExists, report.Fail, "%s already exists", dest)
		return fr
	}
	// The run's own claim set is asked before the disk is touched: a dry
	// run never writes, so only the claims tell it that an earlier file of
	// this run has taken dest, and in a live run with parallel jobs the
	// claim is what keeps a second worker from creating the same temp name
	// while the first is still building the output (guarantee 1). The claim
	// is given back on every path that leaves nothing behind, so a file
	// whose remux failed does not block a later file with the same
	// destination, as it would not in a sequential run.
	if !r.claim(dest) {
		fr.Addf(CodeOutputExists, report.Fail, "%s already exists", dest)
		return fr
	}
	keep := false
	defer func() {
		if !keep {
			r.release(dest)
		}
	}()

	d := r.Profile.Decide(sc.Info, policy.Options{OriginalLanguage: r.Original})
	for _, f := range d.Findings {
		fr.Add(f)
	}
	if fr.Verdict >= report.Fail && !r.Force {
		return fr
	}
	r.describe(&fr, d)
	if r.DryRun {
		keep = true
		fr.Addf(CodeDryRun, report.Pass, "would write %s", dest)
		fr.Output = dest
		return fr
	}

	// The scan refused symlinks, but a remux takes minutes and mkvmerge
	// opens whatever the path names when it starts, so the source is
	// checked again right before it is handed over (guarantee 3). The
	// check also pins the file's identity: fi is what sat at the path when
	// this remux began, and every later check requires the same file.
	if err := sourceUnchanged(fr.Path, fi); err != nil {
		fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
		return fr
	}

	// In place, the destination sits beside the source in a directory that
	// exists and was walked without following symlinks. Otherwise the
	// output root is checked first, because a caller may hand a scanned
	// result to this function without going through RemuxPath, as ingest
	// and watch do, and a defaulted root must not be a planted link (see
	// ensureOutRoot); the mirrored directory chain is then
	// created component by component so a symlink planted inside the output
	// tree cannot redirect the remuxed file outside the root the user named
	// (guarantee 3). The refusal is reported as REMUX_FAIL because the
	// finding codes are frozen and, from the caller's point of view, the
	// remux of this file did not happen; the message carries the reason.
	if !inPlace {
		if err := r.ensureOutRoot(outRoot); err != nil {
			fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
			return fr
		}
		if err := fsutil.MkdirAllUnder(outRoot, filepath.Dir(dest)); err != nil {
			fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
			return fr
		}
	}
	// The temp file is created here, empty and exclusively, before its name
	// is handed to mkvmerge, so a symlink planted at the name in the
	// meantime can only get there by replacing this run's own entry, which
	// tempUnchanged notices after mkvmerge returns and again before the
	// output is placed. The handle from the creation stays open until this
	// function returns, which keeps the inode allocated: a filesystem that
	// reuses a freed inode number at once (ext4 does) could otherwise give
	// it to a file created at the name after a removal, and that file would
	// pass every identity check as this run's own. In place, the output then
	// takes the source's mode,
	// ownership and time through a descriptor that takeIdentity opens
	// without following a link and checks against the same identity, so no
	// metadata write ever goes through the temp name; placedOwn and
	// replacedOwn examine the placed entry afterwards, because the
	// placement primitive uses the name once more after that last check.
	// mkvmerge writing through a planted link would
	// otherwise be verified through that link and the link itself placed.
	// cleanup removes whatever sits at the name; os.Remove never follows a
	// link, so a planted one is removed and its target is left alone.
	tmp := fsutil.TempName(dest)
	own, err := createTemp(tmp)
	if err != nil {
		fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
		return fr
	}
	defer own.Close()
	created := own.Info()
	cleanup := func() { _ = os.Remove(tmp) }

	args := r.mkvmergeArgs(tmp, sc.Info, d)
	res, err := r.Runner.RunWithTimeout(ctx, r.timeout(), exec.MKVMerge, args...)
	if err != nil {
		cleanup()
		fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
		return fr
	}
	if res.ExitCode >= 2 {
		cleanup()
		fr.Add(report.Finding{Code: CodeRemuxFail, Severity: report.Fail, Message: "mkvmerge failed", Detail: strings.TrimSpace(string(res.Stdout) + string(res.Stderr))})
		return fr
	}
	if err := tempUnchanged(tmp, created); err != nil {
		cleanup()
		fr.Addf(CodeRemuxFail, report.Fail, "%v; nothing was placed", err)
		return fr
	}
	if res.ExitCode == 1 {
		fr.Add(report.Finding{Code: CodeRemuxWarning, Severity: report.Warn, Message: "mkvmerge warnings: " + warningsOf(res.Stdout), Detail: strings.TrimSpace(string(res.Stdout))})
	}
	if d.StripProvenance || d.StripTitle {
		pargs := []string{tmp, "--edit", "info"}
		if d.StripProvenance {
			pargs = append(pargs, "--set", "muxing-application=", "--set", "writing-application=", "--delete", "date")
		}
		if d.StripTitle {
			pargs = append(pargs, "--delete", "title")
		}
		pres, err := r.Runner.RunWithTimeout(ctx, 10*time.Minute, exec.MKVPropedit, pargs...)
		if err != nil || pres.ExitCode >= 2 {
			cleanup()
			msg := ""
			if err != nil {
				msg = err.Error()
			} else {
				msg = strings.TrimSpace(string(pres.Stdout) + string(pres.Stderr))
			}
			fr.Addf(CodeRemuxFail, report.Fail, "mkvpropedit: %s", msg)
			return fr
		}
	}

	if !r.verifyOutput(ctx, &fr, sc.Info, d, tmp) {
		cleanup()
		return fr
	}
	// The flush goes through the handle held since the creation, so the
	// name is not opened again for it.
	if err := own.Sync(); err != nil {
		cleanup()
		fr.Addf(CodeRemuxFail, report.Fail, "fsync: %v", err)
		return fr
	}
	// Verification read the temp file by name, so the name must still lead
	// to the file this run created before that file is placed anywhere.
	if err := tempUnchanged(tmp, created); err != nil {
		cleanup()
		fr.Addf(CodeRemuxFail, report.Fail, "%v; nothing was placed", err)
		return fr
	}
	if !inPlace {
		// Only a file that appeared at dest is OUTPUT_EXISTS; any other
		// placement failure, such as a directory that became unwritable,
		// is a failed remux and says so.
		if beforePlace != nil {
			beforePlace(tmp, dest)
		}
		if err := fsutil.PlaceNoClobber(tmp, dest); err != nil {
			cleanup()
			if errors.Is(err, fsutil.ErrExists) {
				fr.Addf(CodeOutputExists, report.Fail, "%v", err)
			} else {
				fr.Addf(CodeRemuxFail, report.Fail, "place: %v", err)
			}
			return fr
		}
		// tempUnchanged looked at the name and PlaceNoClobber then used
		// it again, so the entry now at dest is checked against the file
		// this run created before it is reported as placed.
		if err := placedOwn(dest, created); err != nil {
			cleanup()
			fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
			return fr
		}
		keep = true
		fr.Output = dest
		fr.Addf(CodePlaced, report.Pass, "written and verified")
		return fr
	}
	// In place. The source is checked once more before it is replaced or
	// removed: a symlink planted during the run is never followed, and a
	// different file renamed onto the path after the last read of the
	// source is never replaced or deleted, because what was rebuilt and
	// verified is not that file.
	if err := sourceUnchanged(fr.Path, fi); err != nil {
		cleanup()
		fr.Addf(CodeRemuxFail, report.Fail, "%v; the source was left untouched and the unplaced output was discarded", err)
		return fr
	}
	switch {
	case dest == fr.Path || sameEntry:
		// The verified output replaces the source under the source's own
		// name. When only the extension case differs the entry is renamed
		// to the .mkv spelling last; it is the same entry, so that rename
		// cannot replace any other file.
		//
		// The output first takes the source's mode, ownership and time
		// through a descriptor of the file this run created; a symlink
		// swapped onto the temp name is refused there and its target is
		// never touched.
		//
		// The rename over the source cannot be checked before it happens
		// without renameat2 or renamex_np, which amuxify does not use, so
		// a window stays open between that identity copy and the rename:
		// it needs write access to the source's directory, and a symlink
		// swapped onto the temp name in it is renamed over the source as a
		// link. The entry is examined right after the rename; when it is
		// not the file this run built the source is already gone, so the
		// entry is reported and left alone, never followed and never
		// removed.
		if beforeIdentity != nil {
			beforeIdentity(tmp, fr.Path)
		}
		if err := takeIdentity(tmp, fi, created); err != nil {
			cleanup()
			fr.Addf(CodeRemuxFail, report.Fail, "%v; nothing was placed", err)
			return fr
		}
		if beforePlace != nil {
			beforePlace(tmp, fr.Path)
		}
		if err := os.Rename(tmp, fr.Path); err != nil {
			cleanup()
			fr.Addf(CodeRemuxFail, report.Fail, "replace: %v", err)
			return fr
		}
		keep = true
		if err := replacedOwn(fr.Path, created); err != nil {
			fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
			return fr
		}
		if sameEntry {
			if err := os.Rename(fr.Path, dest); err != nil {
				fr.Output = fr.Path
				fr.Addf(CodeRemuxFail, report.Fail, "rename to %s: %v; the rebuilt file was left under its original name", dest, err)
				return fr
			}
			if !hasEntry(filepath.Dir(dest), filepath.Base(dest)) {
				// The filesystem kept the original spelling; report the
				// name that is really there.
				dest = fr.Path
			}
		}
	default:
		// The source keeps a different name (a.mp4 becomes a.mkv). The
		// output takes the source's identity and is placed under the new
		// name with the no-clobber primitive, and only then is the source
		// removed: a file that appeared at dest during the run is never
		// replaced (guarantee 1), and a crash leaves the source or the
		// placed output, never Matroska content under the old name. The
		// identity is copied through a descriptor of the file this run
		// created, so a symlink swapped onto the temp name is refused and
		// its target keeps its own mode and time.
		if beforeIdentity != nil {
			beforeIdentity(tmp, dest)
		}
		if err := takeIdentity(tmp, fi, created); err != nil {
			cleanup()
			fr.Addf(CodeRemuxFail, report.Fail, "%v; nothing was placed", err)
			return fr
		}
		if beforePlace != nil {
			beforePlace(tmp, dest)
		}
		if err := fsutil.PlaceNoClobber(tmp, dest); err != nil {
			cleanup()
			if errors.Is(err, fsutil.ErrExists) {
				fr.Addf(CodeOutputExists, report.Fail, "%s appeared while the file was being rebuilt; the source was left untouched and the unplaced output was discarded", dest)
			} else {
				fr.Addf(CodeRemuxFail, report.Fail, "place: %v", err)
			}
			return fr
		}
		// The entry at dest must be the file this run created before the
		// source is removed on its account; a foreign entry is discarded
		// and the source stays.
		if err := placedOwn(dest, created); err != nil {
			cleanup()
			fr.Addf(CodeRemuxFail, report.Fail, "%v; the source was left untouched", err)
			return fr
		}
		// The source is checked one last time now that the output sits at
		// dest: were it replaced in the meantime, removing it would delete
		// a file that was never rebuilt. The output is then taken back
		// again, provided dest still is the file this run placed there,
		// and the claim on dest goes with it, so a later file of the run
		// that maps to dest finds the name free, as the disk says it is.
		// The claim is kept only once dest holds an output that stays.
		if err := sourceUnchanged(fr.Path, fi); err != nil {
			if rerr := removeOwn(dest, created); rerr != nil {
				keep = true
				fr.Output = dest
				fr.Addf(CodeRemuxFail, report.Fail, "%v; the source was left untouched and the rebuilt file stays at %s: %v", err, dest, rerr)
				return fr
			}
			fr.Addf(CodeRemuxFail, report.Fail, "%v; the source was left untouched and the placed output was removed again", err)
			return fr
		}
		keep = true
		if err := os.Remove(fr.Path); err != nil {
			fr.Output = dest
			fr.Addf(CodeRemuxFail, report.Fail, "the rebuilt file was placed at %s but the source could not be removed: %v", dest, err)
			return fr
		}
	}
	fr.Output = dest
	fr.Addf(CodePlaced, report.Pass, "written and verified")
	return fr
}

// scannedFile returns the entry at path when it still is the regular file
// the scanner examined, with the size and the modification time the scanner
// recorded in sc.Stat, and an error otherwise: a symlink planted since the
// scan, a directory, a device, nothing at all, another regular file renamed
// onto the path, or the same file written to since. A result that carries
// no Stat was never examined by the scanner and is refused for that reason,
// because the verdict it carries cannot be tied to any file.
func scannedFile(path string, sc scan.Result) (os.FileInfo, error) {
	now, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("changed since the scan: %v", err)
	}
	if now.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is now a symlink; refusing to follow it (changed since the scan)", path)
	}
	if !now.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is no longer a regular file (changed since the scan)", path)
	}
	if sc.Stat == nil {
		return nil, fmt.Errorf("%s was not examined by the scan; nothing can be proven about it", path)
	}
	if !fsutil.Unchanged(sc.Stat, now) {
		return nil, fmt.Errorf("%s changed since the scan; the file that was scanned is not the file at this path", path)
	}
	return now, nil
}

// sourceUnchanged reports an error when path no longer names the regular
// file described by was: a symlink planted after the scan, a directory, a
// device, nothing at all, or another regular file renamed onto the path,
// which is told apart by inode, size and modification time. The scan's own
// symlink check does not cover the time a remux takes, so this runs right
// before the source is opened by a tool and right before it is replaced or
// removed. A nil was means the file could not be examined when the remux
// began, and nothing can be proven about it since.
func sourceUnchanged(path string, was os.FileInfo) error {
	now, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("source changed since the scan: %v", err)
	}
	if now.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is now a symlink; refusing to follow it (changed since the scan)", path)
	}
	if !now.Mode().IsRegular() {
		return fmt.Errorf("%s is no longer a regular file (changed since the scan)", path)
	}
	if was == nil {
		return fmt.Errorf("%s could not be examined when the remux began; nothing was placed", path)
	}
	if !fsutil.Unchanged(was, now) {
		return fmt.Errorf("%s was replaced while it was being rebuilt", path)
	}
	return nil
}

// ensureOutRoot makes sure the output root is a directory this run may
// write into, creating it when it is missing. A root the user named with
// --output is the user's choice: it is created with os.MkdirAll and, when
// it is a symbolic link, followed, as MkdirAllUnder documents for the root
// it is given. The defaulted root, <root>__remuxed beside the input tree,
// was named by nobody, and every remux without --output and --in-place
// writes there, so an entry that already sits at that name is trusted only
// when it is a real directory: anyone with write access to the parent of
// the tree, which a shared download directory gives away, could otherwise
// plant a symbolic link under that name and have every verified output
// placed wherever it points (guarantee 3). A missing defaulted root is
// created with a single mkdir, which fails rather than follows when a link
// appears at the name between the check and the creation, and the entry is
// examined afterwards without following it, so what is accepted is what is
// really there.
func (r *Remuxer) ensureOutRoot(outRoot string) error {
	if r.OutputRoot != "" {
		return os.MkdirAll(outRoot, 0o755)
	}
	if err := os.Mkdir(outRoot, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("output root %s: %w", outRoot, err)
	}
	fi, err := os.Lstat(outRoot)
	if err != nil {
		return fmt.Errorf("output root %s: %w", outRoot, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("output root %s is a symlink; refusing to follow it (name the directory it points to with --output to write there)", outRoot)
	}
	if !fi.IsDir() {
		return fmt.Errorf("output root %s exists and is not a directory", outRoot)
	}
	return nil
}

// createTemp clears tmp and creates it empty and exclusively, so that the
// name mkvmerge writes to belongs to this run before the tool starts. A
// leftover entry that cannot be removed is an error, as is anything that
// appears at the name between the removal and the creation. The file gets
// the mode mkvmerge would give a file it created itself, 0666 under the
// umask, because in output mode the file is placed as it is; in place,
// takeIdentity gives it the source's mode before placement. The handle
// keeps the file open so its inode number cannot be reused by a file
// swapped onto the name; the caller closes it after the last identity
// check.
func createTemp(tmp string) (*fsutil.Temp, error) {
	own, err := fsutil.CreateTemp(tmp)
	if err != nil {
		return nil, fmt.Errorf("temp file: %v", err)
	}
	return own, nil
}

// tempUnchanged reports an error when tmp no longer names the file
// createTemp made: a symlink, a named pipe or another entry renamed onto
// the name, or a hard link added to the file, any of which would let the
// verification or the placement reach a file this run did not create, or
// let a tool that opens the name block on a pipe. mkvmerge and mkvpropedit
// write into the existing file rather than unlinking and recreating it
// (checked against mkvmerge and mkvpropedit v102, which keep the inode),
// so the identity survives both tools.
func tempUnchanged(tmp string, created os.FileInfo) error {
	now, err := os.Lstat(tmp)
	if err != nil {
		return fmt.Errorf("temp file: %v", err)
	}
	if now.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is now a symlink; refusing to follow it", tmp)
	}
	if !now.Mode().IsRegular() || !os.SameFile(created, now) {
		return fmt.Errorf("%s is not the file this run created", tmp)
	}
	if n := fsutil.Nlink(now); n != 1 {
		return fmt.Errorf("%s has %d hard links; expected 1", tmp, n)
	}
	return nil
}

// takeIdentity gives the temp file the mode, ownership and modification
// time the source had, as src describes it. src is the os.Lstat this remux
// pinned the source with and checked with sourceUnchanged right before, so
// nothing is read from the source's name here: a symbolic link swapped onto
// that name after the check cannot supply the mode or the time of whatever
// it points at. The temp name is opened without following a symlink and
// the open file must be the regular file created describes with no other
// name; the metadata is then written through that descriptor and the
// descriptor is closed. Nothing is written by name either, so a symlink or
// a different file swapped onto the temp name after the last tempUnchanged
// check is refused rather than followed, and the swap is reported in the
// words tempUnchanged uses.
func takeIdentity(tmp string, src, created os.FileInfo) error {
	f, err := fsutil.OpenOwn(tmp, created)
	if err != nil {
		return err
	}
	if err := fsutil.CopyIdentityTo(f, src); err != nil {
		_ = f.Close()
		return fmt.Errorf("identity: %v", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("identity: %v", err)
	}
	return nil
}

// placedOwn reports an error when the entry PlaceNoClobber left at dest is
// not the regular file described by created. tempUnchanged checks the temp
// name before placement, but PlaceNoClobber then uses the name again, and
// os.Link follows a symlink on some systems (macOS among them), so a
// symlink swapped onto the temp name between the check and the link would
// place a hard link to the link's target at dest; on the rename fallback
// the symlink itself would land there. The foreign entry is removed only
// when removing it cannot delete the last name of any file: a symlink, or
// a regular file that still has another name, which is what a hard link to
// a symlink's target is. A regular file whose only name is dest is left in
// place, because on a filesystem whose inode numbers change across a rename
// this run's own output would look foreign after the rename fallback, and
// removing it would delete the only copy of the output. The error says
// which happened.
func placedOwn(dest string, created os.FileInfo) error {
	now, err := os.Lstat(dest)
	if err != nil {
		return fmt.Errorf("the output placed at %s could not be examined: %v", dest, err)
	}
	if now.Mode().IsRegular() && os.SameFile(created, now) {
		return nil
	}
	if now.Mode()&os.ModeSymlink != 0 || (now.Mode().IsRegular() && fsutil.Nlink(now) >= 2) {
		_ = os.Remove(dest)
		return fmt.Errorf("the output was swapped before placement; the entry placed at %s was discarded", dest)
	}
	return fmt.Errorf("the entry placed at %s is not the file this run built; it was left in place", dest)
}

// replacedOwn reports an error when the entry the in-place rename left at path
// is not the regular file described by created. Unlike placedOwn it never
// removes anything: the rename has already replaced the source, so whatever
// sits at path is the only entry left under that name, and a symlink there
// is reported rather than followed or deleted.
func replacedOwn(path string, created os.FileInfo) error {
	now, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("the entry placed at %s could not be examined: %v; the source it replaced is gone", path, err)
	}
	if now.Mode().IsRegular() && os.SameFile(created, now) {
		return nil
	}
	return fmt.Errorf("the entry placed at %s is not the file this run built; the source it replaced is gone and the entry was left in place", path)
}

// removeOwn removes path only when it still is the regular file described
// by own, so that a file someone else put there in the meantime is left
// alone.
func removeOwn(path string, own os.FileInfo) error {
	now, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !now.Mode().IsRegular() || !os.SameFile(own, now) {
		return fmt.Errorf("%s is not the file this run placed", path)
	}
	return os.Remove(path)
}

// plan records dest as a destination this run has taken, whether or not it
// was free. claim is the checked form RemuxScanned uses.
func (r *Remuxer) plan(dest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.planLocked(dest)
}

func (r *Remuxer) planLocked(dest string) {
	if r.claimed == nil {
		r.claimed = map[string][]string{}
	}
	dir := filepath.Dir(dest)
	r.claimed[dir] = append(r.claimed[dir], filepath.Base(dest))
}

// claim takes dest for the calling file and reports whether it was free.
// The check and the record happen under one lock, so two workers that map
// to the same name cannot both succeed.
func (r *Remuxer) claim(dest string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.collisionLocked(dest) {
		return false
	}
	r.planLocked(dest)
	return true
}

// release gives dest back after a remux that placed nothing, so a later
// file with the same destination gets its turn. Only the exact spelling the
// claim recorded is removed.
func (r *Remuxer) release(dest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	dir, base := filepath.Dir(dest), filepath.Base(dest)
	names := r.claimed[dir]
	for i, n := range names {
		if n == base {
			r.claimed[dir] = append(names[:i:i], names[i+1:]...)
			return
		}
	}
}

// plannedCollision reports whether an earlier file of this run already
// took dest. Names are compared exactly, and without regard to case as well
// when the destination directory's filesystem folds case, because there
// Ep.mkv and ep.mkv are one entry and the live run fails the second file
// with OUTPUT_EXISTS.
func (r *Remuxer) plannedCollision(dest string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.collisionLocked(dest)
}

func (r *Remuxer) collisionLocked(dest string) bool {
	dir, base := filepath.Dir(dest), filepath.Base(dest)
	names := r.claimed[dir]
	if len(names) == 0 {
		return false
	}
	fold := r.foldsCaseLocked(dir)
	for _, n := range names {
		if n == base || (fold && strings.EqualFold(n, base)) {
			return true
		}
	}
	return false
}

// foldsCase reports whether the filesystem holding dir treats two spellings
// of a name that differ only in case as one entry, as APFS does by default
// and Windows filesystems do, and caches the answer per directory for the
// run. Nothing is written to find out, since a dry run changes nothing:
// starting at dir, or at its nearest existing ancestor when dir does not
// exist yet (an output tree is not created by a dry run, and a directory
// is created on the filesystem of its parent), the entry's own name is
// looked up under a case-flipped spelling and compared with the entry
// itself. A name without an ASCII letter cannot be flipped, so the walk
// continues upward; when it reaches the root without an answer the
// filesystem is taken to be case-sensitive, which compares names exactly.
func (r *Remuxer) foldsCase(dir string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.foldsCaseLocked(dir)
}

func (r *Remuxer) foldsCaseLocked(dir string) bool {
	if v, ok := r.caseFold[dir]; ok {
		return v
	}
	if r.caseFold == nil {
		r.caseFold = map[string]bool{}
	}
	v := foldsCase(dir)
	r.caseFold[dir] = v
	return v
}

func foldsCase(dir string) bool {
	cur := filepath.Clean(dir)
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		if fi, err := os.Lstat(cur); err == nil {
			if flipped := flipCase(filepath.Base(cur)); flipped != "" {
				other, err := os.Lstat(filepath.Join(parent, flipped))
				return err == nil && os.SameFile(fi, other)
			}
		}
		cur = parent
	}
}

// flipCase swaps the case of every ASCII letter in name and returns the
// empty string when name holds none.
func flipCase(name string) string {
	b := []byte(name)
	found := false
	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - 'a' + 'A'
			found = true
		case 'A' <= c && c <= 'Z':
			b[i] = c - 'A' + 'a'
			found = true
		}
	}
	if !found {
		return ""
	}
	return string(b)
}

// hasEntry reports whether dir holds an entry spelled exactly name. On a
// case-insensitive filesystem Lstat resolves X.mkv to the entry X.MKV, and
// only the directory listing tells two spellings of one entry from two
// hard-linked files with different names. When the listing cannot be read
// the answer is true, which callers treat as a collision.
func hasEntry(dir, name string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if e.Name() == name {
			return true
		}
	}
	return false
}

func warningsOf(out []byte) string {
	var w []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Warning:") {
			w = append(w, strings.TrimSpace(strings.TrimPrefix(line, "Warning:")))
		}
	}
	if len(w) == 0 {
		return "(see detail)"
	}
	return strings.Join(w, "; ")
}

func (r *Remuxer) describe(fr *report.FileResult, d *policy.Decision) {
	for _, t := range d.Tracks {
		s := t.Stream
		if s.Type == "attachment" || s.Type == "data" {
			continue
		}
		verb := "drop"
		if t.Keep {
			verb = "keep"
		}
		flags := ""
		if t.Keep && t.Default && s.Type != "video" {
			flags += " default"
		}
		if t.Keep && t.Forced {
			flags += " forced"
		}
		if t.SetLanguage != "" {
			flags += " lang=" + t.SetLanguage
		}
		fr.Actions = append(fr.Actions, fmt.Sprintf("%s #%d %s %s %s%s: %s", verb, s.Index, s.Type, s.Codec, s.Language, flags, t.Reason))
	}
	for _, a := range d.Attachments {
		verb := "drop"
		if a.Keep {
			verb = "keep"
		}
		fr.Actions = append(fr.Actions, fmt.Sprintf("%s attachment #%d %s: %s", verb, a.Attachment.ID, a.Attachment.FileName, a.Reason))
	}
	if d.KeepChapters {
		fr.Actions = append(fr.Actions, "keep chapters")
	} else {
		fr.Actions = append(fr.Actions, "drop chapters")
	}
}

// mkvmergeArgs builds the command line from the decision.
func (r *Remuxer) mkvmergeArgs(out string, m *probe.MediaInfo, d *policy.Decision) []string {
	args := []string{"-o", out, "--disable-track-statistics-tags", "--no-date"}
	if d.StripTitle {
		args = append(args, "--title", "")
	}
	if d.StripTags {
		args = append(args, "--no-global-tags", "--no-track-tags")
	}
	if !d.KeepChapters {
		args = append(args, "--no-chapters")
	}
	var keepAtt []string
	for _, a := range d.Attachments {
		if a.Keep {
			keepAtt = append(keepAtt, fmt.Sprint(a.Attachment.ID))
		}
	}
	if len(keepAtt) == 0 {
		args = append(args, "--no-attachments")
	} else {
		args = append(args, "--attachments", strings.Join(keepAtt, ","))
	}
	ids := map[string][]string{}
	var trackOpts []string
	for _, t := range d.Tracks {
		if !t.Keep {
			continue
		}
		id := t.Stream.MkvID
		if id < 0 {
			id = t.Stream.Index
		}
		sid := fmt.Sprint(id)
		ids[t.Stream.Type] = append(ids[t.Stream.Type], sid)
		if t.Stream.Type == "audio" || t.Stream.Type == "subtitle" {
			trackOpts = append(trackOpts, "--default-track-flag", sid+":"+boolFlag(t.Default))
			trackOpts = append(trackOpts, "--forced-display-flag", sid+":"+boolFlag(t.Forced))
		}
		if t.SetLanguage != "" {
			trackOpts = append(trackOpts, "--language", sid+":"+t.SetLanguage)
		}
		if t.ClearTitle {
			trackOpts = append(trackOpts, "--track-name", sid+":")
		}
		if t.Stream.Type == "video" {
			trackOpts = append(trackOpts, colourArgs(sid, t.Stream)...)
		}
	}
	args = append(args, "--video-tracks", strings.Join(ids["video"], ","))
	if len(ids["audio"]) == 0 {
		args = append(args, "--no-audio")
	} else {
		args = append(args, "--audio-tracks", strings.Join(ids["audio"], ","))
	}
	if len(ids["subtitle"]) == 0 {
		args = append(args, "--no-subtitles")
	} else {
		args = append(args, "--subtitle-tracks", strings.Join(ids["subtitle"], ","))
	}
	args = append(args, "--no-buttons")
	args = append(args, trackOpts...)
	args = append(args, "(", m.Path, ")")
	return args
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// colourArgs names, for one kept video track, the colour and HDR values
// that the source's ffprobe view holds and mkvmerge's own view of the same
// track does not, as the mkvmerge options that write them into the output
// track header. mkvmerge's MP4 and MOV reader carries the primaries,
// transfer and matrix of a colr atom into the Matroska Colour element but
// not the atom's range flag, and it reads neither the mdcv box that holds
// the mastering display nor the clli box that holds the content light
// levels, so a plain remux of such a file loses signalling that the
// verifier then reports as HDR_LOST. A value is named here only when the
// merged Color holds it, which a malformed value never does, and MkvColor
// lacks it; a value mkvmerge saw for itself it carries for itself, so a
// Matroska source gets no option at all. A measured value is written in
// the decimal form mkvmerge parses and only when that form reads back as
// the value held, otherwise the option is left out and the verifier
// reports the loss rather than a wrong value being written. The Dolby
// Vision configuration record has no mkvmerge option; mkvmerge v102
// carries it from an MP4 as a block addition mapping on its own, and with
// a version that does not the verifier reports the record as lost.
func colourArgs(sid string, s probe.Stream) []string {
	c, mk := s.Color, s.MkvColor
	var opts []string
	if mk.Range == "" {
		switch c.Range {
		case "tv":
			opts = append(opts, "--colour-range", sid+":1")
		case "pc":
			opts = append(opts, "--colour-range", sid+":2")
		}
	}
	if m := c.Mastering; m != nil {
		mkPrimaries := mk.Mastering != nil && mk.Mastering.HasPrimaries
		mkLuminance := mk.Mastering != nil && mk.Mastering.HasLuminance
		if m.HasPrimaries && !mkPrimaries {
			coords, ok := mkvNumbers(m.RedX, m.RedY, m.GreenX, m.GreenY, m.BlueX, m.BlueY)
			white, ok2 := mkvNumbers(m.WhiteX, m.WhiteY)
			if ok && ok2 {
				opts = append(opts, "--chromaticity-coordinates", sid+":"+strings.Join(coords, ","))
				opts = append(opts, "--white-colour-coordinates", sid+":"+strings.Join(white, ","))
			}
		}
		if m.HasLuminance && !mkLuminance {
			if lum, ok := mkvNumbers(m.MinLuminance, m.MaxLuminance); ok {
				opts = append(opts, "--min-luminance", sid+":"+lum[0])
				opts = append(opts, "--max-luminance", sid+":"+lum[1])
			}
		}
	}
	if c.Light != nil && mk.Light == nil {
		opts = append(opts, "--max-content-light", sid+":"+strconv.Itoa(c.Light.MaxCLL))
		opts = append(opts, "--max-frame-light", sid+":"+strconv.Itoa(c.Light.MaxFALL))
	}
	return opts
}

// mkvNumbers formats validated chromaticity or luminance values for an
// mkvmerge option. mkvmerge reads a plain decimal with no exponent and
// misreads a fractional part longer than eighteen digits, so each value
// is printed in fixed notation rounded to twelve fractional digits with
// trailing zeros trimmed. The result is false when a rounded string does
// not parse back to a value the verifier would accept as the same, and
// the caller then writes nothing for the group. Twelve digits are finer
// than the verifier's absolute floor, so no value in range fails this
// today; the check is what makes that a fact rather than an assumption.
func mkvNumbers(vals ...float64) ([]string, bool) {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		str := strconv.FormatFloat(v, 'f', 12, 64)
		if strings.Contains(str, ".") {
			str = strings.TrimRight(strings.TrimRight(str, "0"), ".")
		}
		back, err := strconv.ParseFloat(str, 64)
		if err != nil || !probe.CloseEnough(back, v) {
			return nil, false
		}
		out = append(out, str)
	}
	return out, true
}

// verifyOutput runs the verification battery. Returns false on failure.
func (r *Remuxer) verifyOutput(ctx context.Context, fr *report.FileResult, src *probe.MediaInfo, d *policy.Decision, tmp string) bool {
	out, err := r.Prober.Probe(ctx, tmp)
	if err != nil {
		fr.Addf(CodeVerifyFail, report.Fail, "output unreadable: %v", err)
		return false
	}
	if !out.MkvIdentified {
		fr.Addf(CodeVerifyFail, report.Fail, "mkvmerge cannot identify output: %s", strings.Join(out.MkvErrors, "; "))
		return false
	}
	fail := func(format string, a ...interface{}) bool {
		fr.Addf(CodeVerifyFail, report.Fail, format, a...)
		return false
	}
	// Structure.
	for _, typ := range []string{"video", "audio", "subtitle"} {
		want := len(d.KeptOf(typ))
		got := len(out.StreamsOf(typ))
		if want != got {
			return fail("expected %d %s stream(s), output has %d", want, typ, got)
		}
	}
	if len(out.StreamsOf("attachment")) > 0 || len(out.Attachments) > 0 {
		keep := 0
		for _, a := range d.Attachments {
			if a.Keep {
				keep++
			}
		}
		if len(out.Attachments) != keep {
			return fail("expected %d attachment(s), output has %d", keep, len(out.Attachments))
		}
	}
	if !d.KeepChapters && out.ChapterN > 0 {
		return fail("chapters present in output")
	}
	if d.KeepChapters && src.ChapterN > 0 && out.ChapterN == 0 {
		return fail("chapters lost in output")
	}
	if d.StripTags && (out.GlobalTagN > 0 || out.TrackTagN > 0) {
		return fail("tags present in output (%d global, %d track)", out.GlobalTagN, out.TrackTagN)
	}
	if d.StripTitle && out.Title != "" {
		return fail("title present in output")
	}
	if d.StripProvenance && (out.MuxingApp != "" || out.WritingApp != "") {
		return fail("muxing/writing application still set")
	}
	// HDR and Dolby Vision survive only if mkvmerge carried the colour
	// description, the static metadata and the configuration record. Every
	// kept video stream is compared with its copy, and any value lost,
	// gained or changed fails the file (guarantee 5 applies to the
	// signalling as it does to the packets: the output is discarded).
	ov := out.StreamsOf("video")
	for i, t := range d.KeptOf("video") {
		if i >= len(ov) {
			break
		}
		if diffs := hdrDiff(t.Stream, ov[i]); len(diffs) > 0 {
			fr.Addf(CodeHDRLost, report.Fail, "stream #%d: %s", t.Stream.Index, strings.Join(diffs, "; "))
			return false
		}
	}
	// Stream hashes, kept streams in order versus output streams in order.
	if r.Profile.Verify.StreamHash && r.tier() != "none" {
		outByType := map[string][]probe.Stream{"video": out.StreamsOf("video"), "audio": out.StreamsOf("audio"), "subtitle": out.StreamsOf("subtitle")}
		pos := map[string]int{}
		n := 0
		for _, t := range d.Tracks {
			if !t.Keep || (t.Stream.Type != "video" && t.Stream.Type != "audio" && t.Stream.Type != "subtitle") {
				continue
			}
			o := outByType[t.Stream.Type][pos[t.Stream.Type]]
			pos[t.Stream.Type]++
			ok, how, err := r.sameStream(ctx, src, t.Stream, out, o)
			if err != nil {
				return fail("stream #%d hash: %v", t.Stream.Index, err)
			}
			if !ok {
				fr.Addf(CodeHashMismatch, report.Fail, "stream #%d (%s) differs from source", t.Stream.Index, t.Stream.Type)
				return false
			}
			_ = how
			n++
		}
		fr.Addf(CodeHashOK, report.Pass, "%d stream(s) verified identical to source", n)
	}
	// The output carries the source's video and audio streams, so the
	// source probe decides whether there is anything to decode.
	if tier := r.tier(); tier != "none" {
		if err := r.Verifier.Decode(ctx, tmp, src, tier == "full"); err != nil {
			fr.Addf(CodeDecodeFail, report.Fail, "%v", err)
			return false
		}
	}
	return true
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// hdrDiff lists what differs in the HDR signalling between a source video
// stream and the output stream that carries it: the family labels the scan
// reports (hdr10, hlg, dovi, hdr10plus), and then every colour and HDR
// property the probe normalised, in the words probe.ColorDiff uses. An SDR
// source is compared as strictly as an HDR one, so an output that gained
// signalling is reported too.
func hdrDiff(src, out probe.Stream) []string {
	var diffs []string
	sl, ol := strings.Join(src.HDR, "+"), strings.Join(out.HDR, "+")
	if sl != ol {
		diffs = append(diffs, fmt.Sprintf("source video is %s, output is %s", orNone(sl), orNone(ol)))
	}
	return append(diffs, probe.ColorDiff(&src.Color, &out.Color)...)
}

// sameStream compares packet hashes first and falls back to decoded hashes
// when the source container frames packets differently from Matroska.
func (r *Remuxer) sameStream(ctx context.Context, src *probe.MediaInfo, s probe.Stream, out *probe.MediaInfo, o probe.Stream) (bool, string, error) {
	h1, err1 := streamHash(ctx, r.Verifier, src.Path, s.Index)
	h2, err2 := streamHash(ctx, r.Verifier, out.Path, o.Index)
	if err1 == nil && err2 == nil && h1 != "" && h1 == h2 {
		return true, "packet", nil
	}
	if src.IsMatroska() && err1 == nil && err2 == nil {
		return false, "packet", nil
	}
	// Different framing is expected across containers; compare samples.
	d1, err := decodedHash(ctx, r.Verifier, src.Path, s)
	if err != nil {
		return false, "", err
	}
	d2, err := decodedHash(ctx, r.Verifier, out.Path, o)
	if err != nil {
		return false, "", err
	}
	return d1 != "" && d1 == d2, "decoded", nil
}
