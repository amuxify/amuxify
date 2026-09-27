package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/amuxify/amuxify/internal/clean"
	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/ingest"
	"github.com/amuxify/amuxify/internal/remux"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
)

type ingestOpts struct {
	tier, hardlinks, original string
	quarantine                quarantineFlag
	force, removeSidecars     bool
}

// ingestFlags registers the ingest flags; hook passes withForceAndLanguage=false.
func (g *Global) ingestFlags(fs *flag.FlagSet, o *ingestOpts, withForceAndLanguage bool) {
	fs.StringVar(&o.tier, "verify", "", "quick | full | none (default from profile; none is refused)")
	fs.StringVar(&o.hardlinks, "hardlinks", "", "when the file has other hard links: skip | break | copy (default from profile)")
	fs.Var(&o.quarantine, "quarantine", "move BLOCK files under this directory, or under <state-dir>/quarantine when given without a value; write --quarantine=DIR")
	fs.BoolVar(&o.removeSidecars, "remove-blocked-sidecars", false, "delete sidecar files on the profile's block list")
	if withForceAndLanguage {
		fs.BoolVar(&o.force, "force", false, "remux files whose scan verdict is FAIL (BLOCK is never overridden)")
		fs.StringVar(&o.original, "original-language", "", "original language of the title, used to pick the default audio")
	}
}

// validateIngestOpts checks the enums and the quarantine hint; returns "" when fine.
func validateIngestOpts(o *ingestOpts, cmd string, args []string) string {
	if len(args) == 0 {
		return cmd + ": at least one path is required"
	}
	if msg := validateIngestEnums(o, cmd); msg != "" {
		return msg
	}
	if o.quarantine.set && o.quarantine.dir == "" {
		if _, err := os.Lstat(args[0]); err != nil {
			return fmt.Sprintf("%s: %q does not exist; if it was meant as the quarantine directory write --quarantine=%s", cmd, args[0], report.Sanitize(args[0]))
		}
	}
	return ""
}

// checkQuarantineRoots refuses a run whose quarantine directory is one of
// the roots or holds one, before any tool runs; the message is a usage
// error. It returns "" when fine, and always when quarantine is empty.
func checkQuarantineRoots(cmd, quarantine string, paths []string) string {
	if quarantine == "" {
		return ""
	}
	for _, p := range paths {
		if err := scan.CheckQuarantineRoot(p, quarantine); err != nil {
			return cmd + ": " + report.Sanitize(err.Error())
		}
	}
	return ""
}

// validateIngestEnums checks the --verify and --hardlinks values; hook uses
// it on its own because its paths come from the environment, not argv.
func validateIngestEnums(o *ingestOpts, cmd string) string {
	if o.tier != "" && o.tier != "quick" && o.tier != "full" && o.tier != "none" {
		return cmd + ": --verify must be quick, full or none"
	}
	if o.hardlinks != "" && o.hardlinks != "skip" && o.hardlinks != "break" && o.hardlinks != "copy" {
		return cmd + ": --hardlinks must be skip, break or copy"
	}
	return ""
}

// newIngester wires Scanner{VerifyTier:"none", Quarantine}, Remuxer{InPlace:true, ...},
// Cleaner{...}. It returns an error for effective tier none and for a missing
// mkvpropedit; the caller turns it into a usage error. jobs is how many
// files the ingester works on at once; the ingest command passes --jobs and
// the hook adapters pass one.
func (g *Global) newIngester(t *tools, o ingestOpts, jobs int) (*ingest.Ingester, error) {
	if o.tier == "none" {
		return nil, errors.New("ingest: in-place writes require verification; verify tier none is refused (from --verify)")
	}
	if o.tier == "" && t.profile.Verify.Tier == "none" {
		return nil, fmt.Errorf("ingest: in-place writes require verification; verify tier none is refused (from profile %s)", t.profile.Name)
	}
	if !t.runner.Have(exec.MKVPropedit) {
		return nil, errors.New("mkvpropedit not found; run 'amuxify doctor'")
	}
	quarantine := ""
	if !g.DryRun {
		quarantine = o.quarantine.resolve(g.StateDir)
	}
	sc := &scan.Scanner{Runner: t.runner, Prober: t.prober, Verifier: t.verifier, Profile: t.profile,
		VerifyTier: "none", Quarantine: quarantine}
	rm := &remux.Remuxer{Runner: t.runner, Prober: t.prober, Verifier: t.verifier, Profile: t.profile,
		InPlace: true, Hardlinks: o.hardlinks, VerifyTier: o.tier, Force: o.force,
		DryRun: g.DryRun, Original: o.original, Timeout: g.Timeout}
	cl := &clean.Cleaner{Runner: t.runner, Prober: t.prober, Verifier: t.verifier, Profile: t.profile,
		DryRun: g.DryRun, RemoveBlockedSidecars: o.removeSidecars, Hardlinks: o.hardlinks, Timeout: g.Timeout}
	return &ingest.Ingester{
		Scanner: sc, Remuxer: rm, Cleaner: cl, Verifier: t.verifier, Profile: t.profile,
		VerifyTier: o.tier, Hardlinks: o.hardlinks, Force: o.force, Original: o.original,
		RemoveBlockedSidecars: o.removeSidecars, Jobs: jobs,
	}, nil
}

// runIngest builds the summary (command "ingest"), sets in.Progress = g.progress,
// runs IngestPath per path, appends results and records run-level errors.
func (g *Global) runIngest(ctx context.Context, in *ingest.Ingester, paths []string) *report.Summary {
	in.Progress = g.progress
	s := report.NewSummary("amuxify", Version, "ingest", in.Profile.Name)
	for _, p := range paths {
		res, err := in.IngestPath(ctx, p)
		for _, r := range res {
			s.Append(r)
		}
		if err != nil {
			s.Error(fmt.Sprintf("%s: %v", p, err))
		}
	}
	return s
}

func (g *Global) ingest(ctx context.Context, args []string) int {
	fs := g.subFlags("ingest")
	var o ingestOpts
	g.ingestFlags(fs, &o, true)
	if err := g.parse(fs, args); err != nil {
		return int(report.Usage)
	}
	if msg := validateIngestOpts(&o, "ingest", fs.Args()); msg != "" {
		return g.usageErr("%s", msg)
	}
	if !g.DryRun {
		if msg := checkQuarantineRoots("ingest", o.quarantine.resolve(g.StateDir), fs.Args()); msg != "" {
			return g.usageErr("%s", msg)
		}
	}
	t, err := g.setup(!g.DryRun)
	if err != nil {
		return g.usageErr("%v", err)
	}
	in, err := g.newIngester(t, o, g.Jobs)
	if err != nil {
		return g.usageErr("%v", err)
	}
	return g.emit(g.runIngest(ctx, in, fs.Args()))
}
