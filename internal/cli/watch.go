package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/watch"
)

// watch polls one directory and runs ingest on each file once it has stayed
// unchanged for the settle window. It is for set-ups where the hook cannot
// run inside the download client's container: the watcher runs in its own
// container or on the host over a shared volume.
//
// Each pass is one ingest run: the per-file lines stream as each file
// finishes, and a pass that ingested at least one file, or met a new error,
// ends with the usual count line, or under --json with one complete report
// document on a single line, so a consumer reads newline-delimited reports.
// With --once the command makes a pass, waits one settle window, makes a
// second pass that ingests what settled, and exits with the worst verdict.
// Without --once it runs until interrupted and exits 0 on a clean interrupt.
func (g *Global) watch(ctx context.Context, args []string) int {
	fs := g.subFlags("watch")
	var o ingestOpts
	g.ingestFlags(fs, &o, true)
	interval := fs.Duration("interval", 5*time.Second, "time between two passes over the directory")
	settle := fs.Duration("settle", 30*time.Second, "how long a file must stay unchanged before it is ingested")
	once := fs.Bool("once", false, "ingest what has settled after one settle window, then exit")
	if err := fs.Parse(args); err != nil {
		return int(report.Usage)
	}
	if fs.NArg() != 1 {
		if fs.NArg() == 0 {
			return g.usageErr("watch: a directory is required")
		}
		return g.usageErr("watch: exactly one directory is watched, got %d arguments", fs.NArg())
	}
	if msg := validateIngestOpts(&o, "watch", fs.Args()); msg != "" {
		return g.usageErr("%s", msg)
	}
	if *interval <= 0 {
		return g.usageErr("watch: --interval must be longer than 0s")
	}
	if *settle < 0 {
		return g.usageErr("watch: --settle must not be negative")
	}
	abs, err := fsutil.Abs(fs.Arg(0))
	if err != nil {
		return g.usageErr("watch: %v", err)
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return g.usageErr("watch: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return g.usageErr("watch: %s is a symbolic link; name the directory itself", report.Sanitize(abs))
	}
	if !fi.IsDir() {
		return g.usageErr("watch: %s is not a directory", report.Sanitize(abs))
	}
	quarantine := ""
	if !g.DryRun {
		quarantine = o.quarantine.resolve(g.StateDir)
		if msg := checkQuarantineRoots("watch", quarantine, []string{abs}); msg != "" {
			return g.usageErr("%s", msg)
		}
	}
	t, err := g.setup(!g.DryRun)
	if err != nil {
		return g.usageErr("%v", err)
	}
	// The ingester is built once here to refuse verify tier none and a
	// missing mkvpropedit before the first pass, and again for every pass
	// below, because a dry run's remuxer remembers the outputs it planned and
	// a file that is ingested again in a later pass must not collide with its
	// own earlier plan.
	// The watcher hands the ingester one file at a time, so the job count is
	// always one here; --jobs is accepted and ignored, as it is for a hook.
	if _, err := g.newIngester(t, o, 1); err != nil {
		return g.usageErr("%v", err)
	}
	// The operator's own lines go to stdout in human mode and to stderr
	// under --json, where stdout carries report documents and nothing else.
	logw := g.stdout
	if g.JSON {
		logw = g.stderr
	}
	if !g.Quiet {
		fmt.Fprintf(logw, "amuxify watch: %s every %s, ingesting each file after %s unchanged\n", report.Sanitize(abs), *interval, *settle)
	}
	w := &watch.Watcher{Root: abs, Settle: *settle, Exclude: scan.QuarantineExcludes(quarantine),
		Notice: func(msg string) { fmt.Fprintf(g.stderr, "amuxify watch: %s\n", report.Sanitize(msg)) }}
	worst := report.Pass
	pass := func() {
		in, err := g.newIngester(t, o, 1)
		if err != nil {
			// Checked above; the tools do not disappear between passes.
			fmt.Fprintln(g.stderr, "amuxify watch:", err)
			worst = report.Fail
			return
		}
		inputRoot, outRoot := in.Remuxer.Roots(abs, true)
		// The context the watcher hands over is not cancelled by the
		// interrupt, so the file in progress is finished, with its tools
		// bounded by their timeouts, and the pass stops before the next one.
		w.Ingest = func(ctx context.Context, p string) report.FileResult {
			fr := in.IngestFile(ctx, p, abs, inputRoot, outRoot)
			g.progress(fr)
			return fr
		}
		s := report.NewSummary("amuxify", Version, "ingest", t.profile.Name)
		res, err := w.Pass(ctx)
		for _, r := range res {
			s.Append(r)
		}
		if err != nil {
			s.Error(err.Error())
		}
		if len(s.Files) == 0 && len(s.Errors) == 0 {
			return
		}
		s.Close()
		if g.JSON {
			// One document per line: the indented form of the other
			// commands would spread a document over many lines.
			if err := json.NewEncoder(g.stdout).Encode(s); err != nil {
				fmt.Fprintln(g.stderr, "amuxify:", err)
			}
		} else if !g.Quiet {
			s.WriteHumanTail(g.stdout)
		}
		worst = report.Worst(worst, s.Verdict)
	}
	if *once {
		pass()
		if *settle > 0 && sleep(ctx, *settle) {
			pass()
		}
		code := report.ExitCode(worst)
		if ctx.Err() != nil && code == 0 {
			return 130
		}
		return code
	}
	for {
		pass()
		if !sleep(ctx, *interval) {
			break
		}
	}
	if !g.Quiet {
		fmt.Fprintln(logw, "amuxify watch: interrupted, stopped")
	}
	return 0
}

// sleep waits for d or until ctx is done; it reports whether the wait
// completed without an interrupt.
func sleep(ctx context.Context, d time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
