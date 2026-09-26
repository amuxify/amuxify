package cli

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/hook"
	"github.com/amuxify/amuxify/internal/report"
)

// environ supplies the caller's environment to the hook adapters. Tests
// replace it to feed values that t.Setenv cannot express.
var environ = os.Environ

// hook runs ingest for a download client or media manager and exits the way
// that caller expects. Everything the adapter reads from its caller is data:
// paths go to ingest unchanged and names go into log lines and the report.
func (g *Global) hook(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return g.usageErr("hook: adapter required: %s", strings.Join(hook.Adapters(), ", "))
	}
	a := hook.Adapter(args[0])
	if !hook.Valid(a) {
		return g.usageErr("hook: unknown adapter %q (%s)", args[0], strings.Join(hook.Adapters(), ", "))
	}
	// NZBGet classifies each stdout line by its leading tag and executes
	// lines that begin with [NZB]. Wrapping the writers here, before any
	// flag parsing, is the single place the prefixes are added; only nzbOut
	// can write a control line.
	var nzbOut *hook.PrefixWriter
	if a == hook.NZBGet {
		nzbOut = hook.NewPrefixWriter(g.stdout, "[INFO] ")
		errw := hook.NewPrefixWriter(g.stderr, "[ERROR] ")
		g.stdout, g.stderr = nzbOut, errw
		defer nzbOut.Flush()
		defer errw.Flush()
	}
	usage := func(format string, v ...interface{}) int {
		g.usageErr(format, v...)
		return hook.ExitCode(a, report.Pass, report.Fail, hook.UsageError)
	}
	fs := g.subFlags("hook " + string(a))
	var o ingestOpts
	g.ingestFlags(fs, &o, false)
	failOnS := fs.String("fail-on", "fail", "the verdict from which the caller sees a failure: warn | fail | block")
	category := fs.String("category", "", "act only when the job's category matches this glob (SABnzbd and NZBGet)")
	jsonOut := fs.String("json-out", "", "also write the JSON report to this file, which must not exist yet")
	if err := fs.Parse(args[1:]); err != nil {
		return hook.ExitCode(a, report.Pass, report.Fail, hook.UsageError)
	}
	failOn, err := hook.ParseFailOn(*failOnS)
	if err != nil {
		return usage("hook %s: %v", a, err)
	}
	if msg := validateIngestEnums(&o, "hook "+string(a)); msg != "" {
		return usage("%s", msg)
	}
	if a == hook.NZBGet && g.JSON {
		return usage("hook nzbget: --json is not supported because NZBGet logs stdout line by line; use --json-out <file>")
	}
	job, err := hook.Parse(a, environ(), fs.Args())
	if err != nil {
		return usage("hook %s: %v", a, err)
	}
	if job.Test {
		return g.hookTest(ctx, a)
	}
	if *category != "" && job.Category != "" {
		ok, err := hook.MatchCategory(*category, job.Category)
		if err != nil {
			return usage("hook %s: --category: %v", a, err)
		}
		if !ok {
			job.Skip = fmt.Sprintf("category %s does not match --category %s", job.Category, *category)
		}
	}
	if job.Skip != "" {
		fmt.Fprintf(g.stdout, "amuxify hook %s: skipping, %s\n", a, oneLine(job.Skip))
		return hook.ExitCode(a, report.Pass, failOn, hook.Skipped)
	}
	t, err := g.setup(!g.DryRun)
	if err != nil {
		return usage("%v", err)
	}
	o.original = job.Original
	in, err := g.newIngester(t, o)
	if err != nil {
		return usage("%v", err)
	}
	label := job.Label
	if label == "" {
		label = strings.Join(job.Paths, ", ")
	}
	fmt.Fprintf(g.stdout, "amuxify hook %s: %s (%s)\n", a, oneLine(label), oneLine(job.Event))
	s := g.runIngest(ctx, in, job.Paths)
	outcome := hook.Ran
	if ctx.Err() != nil {
		outcome = hook.Interrupted
	}
	code := hook.ExitCode(a, s.Verdict, failOn, outcome)
	s.Hook = &report.HookInfo{Adapter: string(a), Event: job.Event, Label: job.Label,
		Category: job.Category, FailOn: failOn.String(), ExitCode: code}
	g.emit(s)
	if *jsonOut != "" {
		g.writeJSONOut(s, *jsonOut)
	}
	if a == hook.SABnzbd && !g.Quiet {
		fmt.Fprintf(g.stdout, "amuxify: %s, %d file(s)\n", s.Verdict, len(s.Files))
	}
	if (a == hook.Sonarr || a == hook.Radarr) && s.Verdict >= failOn {
		s.WriteHumanTail(g.stderr)
	}
	for _, l := range hook.ControlLines(a, s.Verdict) {
		if nzbOut != nil {
			nzbOut.Control(l)
		} else {
			fmt.Fprintln(g.stdout, l)
		}
	}
	return code
}

// hookTest answers the Sonarr and Radarr connection test: the profile loads,
// the required tools are present and report a version, and the process is
// allowed to write. The result is printed on stdout, and on stderr too when
// it failed, because the arrs show stderr in the test dialog.
func (g *Global) hookTest(ctx context.Context, a hook.Adapter) int {
	fail := func(err error) int {
		line := fmt.Sprintf("amuxify hook %s: test failed: %v\n", a, err)
		fmt.Fprint(g.stdout, line)
		fmt.Fprint(g.stderr, line)
		return 1
	}
	t, err := g.setup(!g.DryRun)
	if err != nil {
		return fail(err)
	}
	if !t.runner.Have(exec.MKVPropedit) {
		return fail(fmt.Errorf("mkvpropedit not found; run 'amuxify doctor'"))
	}
	ffmpeg, err := t.runner.Version(ctx, exec.FFmpeg)
	if err != nil {
		return fail(err)
	}
	mkvmerge, err := t.runner.Version(ctx, exec.MKVMerge)
	if err != nil {
		return fail(err)
	}
	fmt.Fprintf(g.stdout, "amuxify %s: hook %s ready (profile %s; ffmpeg: %s; mkvmerge: %s)\n",
		Version, a, t.profile.Name, oneLine(ffmpeg), oneLine(mkvmerge))
	return 0
}

// writeJSONOut writes the report to a new file. An existing file, a symlink
// in its place or any other failure is reported on stderr and never changes
// the exit code; nothing is ever overwritten.
func (g *Global) writeJSONOut(s *report.Summary, path string) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		fmt.Fprintf(g.stderr, "amuxify: json-out: %v\n", err)
		return
	}
	err = s.WriteJSON(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		fmt.Fprintf(g.stderr, "amuxify: json-out: %s: %v\n", path, err)
	}
}

// oneLine keeps caller-supplied text on the line it was printed on: every
// control character, including a newline or a carriage return that could
// forge a further log line, becomes a space.
func oneLine(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
}
