// Package ingest composes scan, remux and clean into one in-place pass with
// one result per file. The routing decision is a pure function.
package ingest

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/amuxify/amuxify/internal/clean"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/policy"
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
	paths, err := scan.Walk(abs)
	if err != nil {
		return nil, err
	}
	var out []report.FileResult
	for _, p := range paths {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		fr := in.IngestFile(ctx, p, abs, inputRoot, outRoot)
		if in.Progress != nil {
			in.Progress(fr)
		}
		out = append(out, fr)
	}
	return out, nil
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
		case fr.Verdict < report.Fail:
			merge(&fr, in.Cleaner.CleanScanned(ctx, sc))
		}
		reasons = []string{"sidecar"}
	} else {
		switch {
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
		case RouteClean:
			if scan.MediaExts[fsutil.Ext(path)] != "subtitle" {
				if err := in.decode(ctx, path); err != nil {
					fr.Addf(scan.CodeDecodeFail, report.Fail, "%v", err)
					break
				}
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

// quarantined reports whether scan moved a BLOCK file away. The scanner
// records QUARANTINED on the result; when quarantine is on and the path is
// gone after the scan the move happened even if the finding was lost.
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
// mkvpropedit edits its headers.
func (in *Ingester) decode(ctx context.Context, path string) error {
	if in.Verifier == nil {
		return errors.New("no verifier configured")
	}
	switch in.tier() {
	case "full":
		return in.Verifier.DecodeFull(ctx, path)
	default:
		return in.Verifier.DecodeHeadTail(ctx, path)
	}
}
