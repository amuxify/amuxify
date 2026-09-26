// Package ingest composes scan, remux and clean into one in-place pass with
// one result per file. The routing decision is a pure function.
package ingest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amuxify/amuxify/internal/clean"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/remux"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/verify"
)

// Ingester holds the three stages and the run options.
type Ingester struct {
	Scanner  *scan.Scanner  // VerifyTier "none"; Quarantine set here; Progress nil
	Remuxer  *remux.Remuxer // InPlace true; Hardlinks, VerifyTier, Force, DryRun, Original, Timeout set; Scanner nil; Progress nil
	Cleaner  *clean.Cleaner // DryRun, RemoveBlockedSidecars, Hardlinks set; Progress nil
	Verifier *verify.Verifier
	Profile  *policy.Profile

	VerifyTier            string // quick | full; "" means profile value; "none" is refused by IngestPath
	Hardlinks             string // skip | break | copy; "" means profile value
	Force                 bool
	Original              string
	RemoveBlockedSidecars bool
	// Progress, when set, receives each result as soon as the file is done.
	Progress func(report.FileResult)

	// planned holds the destinations earlier files of a dry run would
	// write, so a later file that maps to the same path is reported with
	// the OUTPUT_EXISTS the live run would produce (review C33).
	planned map[string]bool
}

// ErrVerifyNone is returned by IngestPath when the effective verify tier is none.
var ErrVerifyNone = errors.New("ingest writes in place and requires verification; verify tier none is refused")

func (in *Ingester) tier() string {
	if in.VerifyTier != "" {
		return in.VerifyTier
	}
	if in.Profile != nil {
		return in.Profile.Verify.Tier
	}
	return ""
}

func (in *Ingester) hardlinks() string {
	if in.Hardlinks != "" {
		return in.Hardlinks
	}
	if in.Profile != nil {
		return in.Profile.Safety.Hardlinks
	}
	return "skip"
}

// IngestPath processes one file or every file under a directory. It returns
// an error for effective verify tier none before touching anything.
func (in *Ingester) IngestPath(ctx context.Context, root string) ([]report.FileResult, error) {
	if in.tier() == "none" {
		return nil, ErrVerifyNone
	}
	abs, err := fsutil.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	inputRoot, outRoot := in.Remuxer.Roots(abs, fi.IsDir())
	// The scan root is the tree quarantine mirrors. For a single file it is
	// the file's directory, as in scan.ScanPath; the file itself would make
	// the mirrored path "." and the quarantine destination the quarantine
	// root, which can never be a file.
	scanRoot := abs
	if !fi.IsDir() {
		scanRoot = filepath.Dir(abs)
	}
	// Walk lists every readable entry and names the unreadable ones in
	// walkErr; those are reported at run level after the readable files,
	// which raises the run verdict to FAIL.
	paths, walkErr := scan.Walk(abs)
	in.planned = map[string]bool{}
	var out []report.FileResult
	for _, p := range paths {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		fr := in.IngestFile(ctx, p, scanRoot, inputRoot, outRoot)
		if in.Progress != nil {
			in.Progress(fr)
		}
		out = append(out, fr)
	}
	return out, walkErr
}

// merge folds a stage result into fr so the verdict rises with its findings.
func merge(fr *report.FileResult, r report.FileResult) {
	for _, f := range r.Findings {
		fr.Add(f)
	}
	if r.Output != "" {
		fr.Output = r.Output
	}
	fr.Actions = append(fr.Actions, r.Actions...)
}

// IngestFile runs the per-file algorithm for one path. scanRoot is the tree
// quarantine mirrors; inputRoot and outRoot come from Remuxer.Roots.
func (in *Ingester) IngestFile(ctx context.Context, path, scanRoot, inputRoot, outRoot string) (fr report.FileResult) {
	start := time.Now()
	if in.Cleaner != nil {
		// The cleaner's advice must not name --strip-audio-tags, which
		// only the clean command has (review C10).
		in.Cleaner.Command = "ingest"
	}
	sc := in.Scanner.ScanFile(ctx, path, scanRoot)
	fr = sc.File
	if fr.Info == nil {
		fr.Info = map[string]string{}
	}
	defer func() { fr.Duration = time.Since(start) }()

	route, reasons := RouteSkip, []string(nil)
	if !scan.IsMedia(path) {
		switch {
		case fr.Verdict >= report.Block:
			if in.RemoveBlockedSidecars && fr.Has(scan.CodeSidecarBlocked) && !in.quarantined(&fr, path) {
				merge(&fr, in.Cleaner.CleanScanned(ctx, sc))
			}
		case fr.Has(scan.CodeSymlink):
			// The scanner already reported the link and never followed it;
			// the cleaner would only repeat SYMLINK (review C11).
			reasons = []string{"sidecar", "symlink skipped"}
		case fr.Verdict < report.Fail:
			merge(&fr, in.Cleaner.CleanScanned(ctx, sc))
		}
		if reasons == nil {
			reasons = []string{"sidecar"}
		}
	} else {
		switch {
		case fr.Has(scan.CodeSymlink):
			// Guarantee 3: a symlink is never followed and never aborts a
			// run. The scanner already reported it as WARN SYMLINK; it is
			// skipped here without a REFUSED finding because nothing was
			// refused, the link simply is not media to be ingested.
			reasons = []string{"symlink skipped"}
		case fr.Verdict >= report.Block:
			fr.Addf(remux.CodeRefused, report.Block, "scan blocked this file; not ingested")
			reasons = []string{"scan verdict BLOCK"}
		case fr.Verdict >= report.Fail && !in.Force:
			fr.Addf(remux.CodeRefused, report.Fail, "scan failed this file; use --force to remux anyway")
			reasons = []string{"scan verdict FAIL"}
		case sc.Info == nil:
			fr.Addf(remux.CodeRefused, report.Fail, "scan produced no probe result; not ingested")
			reasons = []string{"no probe result"}
		default:
			d := in.Profile.Decide(sc.Info, policy.Options{OriginalLanguage: in.Original})
			route, reasons = Decide(&fr, sc, d, in.hardlinks(), in.Force)
		}
		switch route {
		case RouteRemux:
			sc.File = fr
			fr = in.Remuxer.RemuxScanned(ctx, sc, inputRoot, outRoot)
			if fr.Info == nil {
				fr.Info = map[string]string{}
			}
			fr = in.planDestination(sc, fr)
		case RouteClean:
			if err := in.decode(ctx, path, sc.Info); err != nil {
				fr.Addf(scan.CodeDecodeFail, report.Fail, "%v", err)
				break
			}
			merge(&fr, in.Cleaner.CleanScanned(ctx, sc))
		}
	}
	if route == RouteClean && len(reasons) == 0 {
		reasons = []string{"tracks, flags, attachments and chapters already match the profile"}
	}
	fr.Addf(CodeRoute, report.Pass, "%s: %s", route, strings.Join(reasons, "; "))
	fr.Info["route"] = string(route)
	return fr
}

// planDestination makes a dry run predict the OUTPUT_EXISTS collision a live
// run hits when two files of one run rebuild to the same destination, such as
// sample.mp4 and sample.avi both becoming sample.mkv. The remuxer only checks
// the disk, which a dry run never changes, so the destinations it promised
// earlier in this run are remembered here. A later file that maps to one of
// them gets the same result the live run gives: the scan findings, the
// hard-link note if any, and OUTPUT_EXISTS FAIL instead of the dry-run plan.
func (in *Ingester) planDestination(sc scan.Result, fr report.FileResult) report.FileResult {
	if !in.Remuxer.DryRun || fr.Output == "" || !fr.Has(remux.CodeDryRun) {
		return fr
	}
	if in.planned == nil {
		in.planned = map[string]bool{}
	}
	dest := fr.Output
	if !in.planned[dest] {
		in.planned[dest] = true
		return fr
	}
	out := sc.File
	// fr was built from sc.File by appending, so the two may share one
	// backing array; give out its own copy before adding to it.
	out.Findings = append([]report.Finding(nil), sc.File.Findings...)
	if out.Info == nil {
		out.Info = map[string]string{}
	}
	out.Output = ""
	for _, f := range fr.Findings {
		if f.Code == remux.CodeHardlinked {
			out.Add(f)
		}
	}
	out.Addf(remux.CodeOutputExists, report.Fail, "%s already exists", dest)
	return out
}

// quarantined reports whether scan moved a BLOCK file away. The scanner
// records QUARANTINED on the result it returns; the Lstat fallback is kept
// as a second line of defence so that a file which is gone after the scan
// is never handed to the cleaner, whatever the findings say.
func (in *Ingester) quarantined(fr *report.FileResult, path string) bool {
	if fr.Has(scan.CodeQuarantined) {
		return true
	}
	if in.Scanner.Quarantine == "" {
		return false
	}
	_, err := os.Lstat(path)
	return err != nil
}

// decode runs the read-only decode check for the clean route. The scanner
// ran with tier none, so this is the only decode of the file before
// mkvpropedit edits its headers. A container without video or audio, such
// as a subtitle-only .mks, has nothing to decode and passes; the decision
// rests on the probed streams, not on the file extension.
func (in *Ingester) decode(ctx context.Context, path string, info *probe.MediaInfo) error {
	if in.Verifier == nil {
		return errors.New("no verifier configured")
	}
	return in.Verifier.Decode(ctx, path, info, in.tier() == "full")
}
