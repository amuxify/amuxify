// Package cli is the subcommand dispatcher for the amuxify binary. It uses
// only the standard flag package: global flags come first, then the
// subcommand, then subcommand flags, then paths.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/amuxify/amuxify/internal/clean"
	"github.com/amuxify/amuxify/internal/doctor"
	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/remux"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/verify"
)

// Version is set by the linker (-X ...cli.Version=x.y.z) or falls back.
var Version = "dev"

// Global holds flags shared by every subcommand.
type Global struct {
	Profile   string
	JSON      bool
	DryRun    bool
	Verbose   bool
	Quiet     bool
	Timeout   time.Duration
	StateDir  string
	AllowRoot bool
	Trace     bool

	stdout io.Writer
	stderr io.Writer
}

// bind registers the global flags. Defaults are the current values so the
// same flags can be accepted again after the subcommand without resetting
// what was parsed before it.
func (g *Global) bind(fs *flag.FlagSet) {
	if g.Profile == "" {
		g.Profile = policy.Default
		if v := os.Getenv("AMUXIFY_PROFILE"); v != "" {
			g.Profile = v
		}
	}
	if g.StateDir == "" {
		g.StateDir = defaultStateDir()
	}
	fs.StringVar(&g.Profile, "profile", g.Profile, "built-in profile name or path to a TOML file")
	fs.BoolVar(&g.JSON, "json", g.JSON, "write the report as JSON on stdout")
	fs.BoolVar(&g.DryRun, "dry-run", g.DryRun, "decide and report, change nothing")
	fs.BoolVar(&g.Verbose, "verbose", g.Verbose, "show PASS-level findings and per-track actions")
	fs.BoolVar(&g.Quiet, "quiet", g.Quiet, "suppress the human report; exit code only")
	fs.DurationVar(&g.Timeout, "timeout", g.Timeout, "per-tool timeout (default: 60s probe, 1h verify, 6h remux, 2h clean)")
	fs.StringVar(&g.StateDir, "state-dir", g.StateDir, "directory for quarantine and run logs")
	fs.BoolVar(&g.AllowRoot, "allow-root", g.AllowRoot, "run even as root (files would be root-owned)")
	fs.BoolVar(&g.Trace, "trace", g.Trace, "print every external command line to stderr")
}

func defaultStateDir() string {
	if v := os.Getenv("AMUXIFY_STATE_DIR"); v != "" {
		return v
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "amuxify")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".amuxify-state"
	}
	return filepath.Join(home, ".local", "state", "amuxify")
}

// usageText is assembled once from usageHead, the global flag block and
// usageTail. The flag block is generated from the same definitions bind
// installs, so the help page and the flag help strings cannot drift apart.
var usageText = usageHead + globalFlagHelp() + usageTail

const usageHead = `amuxify %s - the ingest gate for self-hosted media

Usage:
  amuxify [global flags] <command> [command flags] <path>...

Commands:
  scan      verify and inspect files, change nothing (default verify: quick)
  remux     rebuild into a sanitized MKV with mkvmerge, then prove it
  clean     strip metadata and extended attributes in place, tracks untouched
  ingest    scan, then rebuild into a verified MKV or clean in place, in one pass
  hook      run ingest for sabnzbd | nzbget | sonarr | radarr and exit the way they expect
  doctor    check tools, versions, profile, and environment
  profile   list built-in profiles or print one:  amuxify profile show homelab
  version   print the version

Global flags (before or after the command):
`

const usageTail = `
The built-in profiles are homelab (the default), archive, anime and strict;
AMUXIFY_PROFILE sets the default profile.

Exit status: 0 PASS, 1 WARN, 2 usage error, 3 FAIL, 4 BLOCK, 130 interrupted.
doctor exits 0 when usable (warnings shown), 2 when required tools are missing.
Tool paths can be overridden with AMUXIFY_FFMPEG, AMUXIFY_MKVMERGE, and so on.
`

// globalFlagHelp renders one line per global flag from the definitions in
// bind: the flag name, a placeholder for its value when it takes one, and
// its help string. Percent signs are doubled because usageText is a format
// string.
func globalFlagHelp() string {
	fs := flag.NewFlagSet("amuxify", flag.ContinueOnError)
	(&Global{}).bind(fs)
	placeholders := map[string]string{"profile": "name|path", "state-dir": "dir", "timeout": "duration"}
	var b strings.Builder
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		if p, ok := placeholders[f.Name]; ok {
			name = p
		}
		col := "--" + f.Name
		if name != "" {
			col += " <" + name + ">"
		}
		fmt.Fprintf(&b, "  %-24s%s\n", col, strings.ReplaceAll(usage, "%", "%%"))
	})
	return b.String()
}

// Main runs the program and returns the exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	g := &Global{stdout: stdout, stderr: stderr}
	fs := flag.NewFlagSet("amuxify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprintf(stderr, usageText, Version) }
	g.bind(fs)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return int(report.Usage)
	}
	rest := fs.Args()
	if len(rest) == 0 {
		fs.Usage()
		return int(report.Usage)
	}
	cmd, rest := rest[0], rest[1:]

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var code int
	switch cmd {
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "amuxify %s\n", Version)
		return 0
	case "help", "--help", "-h":
		fs.Usage()
		return 0
	case "profile":
		code = g.profile(rest)
	case "doctor":
		code = g.doctor(ctx, rest)
	case "scan":
		code = g.scan(ctx, rest)
	case "remux":
		code = g.remux(ctx, rest)
	case "clean":
		code = g.clean(ctx, rest)
	case "ingest":
		code = g.ingest(ctx, rest)
	case "hook":
		code = g.hook(ctx, rest)
	default:
		fmt.Fprintf(stderr, "amuxify: unknown command %q\n\n", cmd)
		fs.Usage()
		return int(report.Usage)
	}
	if ctx.Err() != nil && code == 0 {
		return 130
	}
	return code
}

func (g *Global) usageErr(format string, a ...interface{}) int {
	fmt.Fprintf(g.stderr, "amuxify: "+format+"\n", a...)
	return int(report.Usage)
}

func (g *Global) subFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet("amuxify "+name, flag.ContinueOnError)
	fs.SetOutput(g.stderr)
	// Global flags are accepted after the subcommand too, for convenience.
	g.bind(fs)
	return fs
}

// tools builds the shared runner, prober and verifier.
type tools struct {
	runner   *exec.Runner
	prober   *probe.Prober
	verifier *verify.Verifier
	profile  *policy.Profile
}

func (g *Global) setup(requireWriter bool) (*tools, error) {
	if fsutil.IsRoot() && requireWriter && !g.AllowRoot {
		return nil, errors.New("refusing to modify files as root; pass --allow-root if intended")
	}
	p, err := policy.Load(g.Profile)
	if err != nil {
		return nil, err
	}
	r := &exec.Runner{Timeout: g.Timeout}
	if g.Trace {
		r.Trace = func(s string) { fmt.Fprintln(g.stderr, "+", s) }
	}
	for _, t := range []string{exec.FFmpeg, exec.FFprobe, exec.MKVMerge} {
		if !r.Have(t) {
			return nil, fmt.Errorf("%s not found; run 'amuxify doctor'", t)
		}
	}
	return &tools{
		runner:   r,
		prober:   &probe.Prober{Runner: r, Timeout: g.Timeout},
		verifier: &verify.Verifier{Runner: r, Timeout: g.Timeout},
		profile:  p,
	}, nil
}

// streaming reports whether per-file lines go out as files finish.
func (g *Global) streaming() bool { return !g.JSON && !g.Quiet }

// progress prints one result immediately in human mode.
func (g *Global) progress(fr report.FileResult) {
	if g.streaming() {
		fr.WriteHuman(g.stdout, g.Verbose)
	}
}

// emit finishes the run: the whole JSON document, or just the summary line
// because the per-file lines were already streamed.
func (g *Global) emit(s *report.Summary) int {
	s.Close()
	if g.JSON {
		if err := s.WriteJSON(g.stdout); err != nil {
			fmt.Fprintln(g.stderr, "amuxify:", err)
		}
	} else if !g.Quiet {
		s.WriteHumanTail(g.stdout)
	}
	return report.ExitCode(s.Verdict)
}

func (g *Global) profile(args []string) int {
	if len(args) == 0 || args[0] == "list" {
		for _, n := range policy.Names() {
			p, _ := policy.Load(n)
			d := ""
			if p != nil {
				d = p.Description
			}
			mark := " "
			if n == policy.Default {
				mark = "*"
			}
			fmt.Fprintf(g.stdout, "%s %-8s %s\n", mark, n, d)
		}
		return 0
	}
	switch args[0] {
	case "show":
		if len(args) != 2 {
			return g.usageErr("usage: amuxify profile show <name>")
		}
		src, err := policy.Source(args[1])
		if err != nil {
			return g.usageErr("%v", err)
		}
		g.stdout.Write(src)
		return 0
	case "check":
		if len(args) != 2 {
			return g.usageErr("usage: amuxify profile check <path>")
		}
		p, err := policy.Load(args[1])
		if err != nil {
			fmt.Fprintln(g.stderr, "amuxify:", err)
			return int(report.Fail)
		}
		fmt.Fprintf(g.stdout, "OK %s: %s\n", args[1], p.Description)
		return 0
	}
	return g.usageErr("usage: amuxify profile [list | show <name> | check <path>]")
}

func (g *Global) doctor(ctx context.Context, args []string) int {
	fs := g.subFlags("doctor")
	if err := fs.Parse(args); err != nil {
		return int(report.Usage)
	}
	r := &exec.Runner{Timeout: 30 * time.Second}
	checks, worst := doctor.Run(ctx, r, g.Profile, g.StateDir)
	// doctor answers "can amuxify run here?". Optional tools that are absent
	// are reported as WARN but do not fail the check, so `amuxify doctor &&
	// ...` works on a plain install. Missing or too-old required tools and an
	// invalid profile exit 2.
	usable := worst <= report.Warn
	if g.JSON {
		s := report.NewSummary("amuxify", Version, "doctor", g.Profile)
		for _, c := range checks {
			fr := report.FileResult{Path: c.Name}
			fr.Add(report.Finding{Code: strings.ToUpper(c.Name), Severity: c.Status, Message: c.Detail})
			s.Append(fr)
		}
		g.emit(s)
		if usable {
			return 0
		}
		return report.ExitCode(worst)
	}
	fmt.Fprintf(g.stdout, "amuxify %s\n%s", Version, doctor.Format(checks))
	switch {
	case worst == report.Pass:
		fmt.Fprintln(g.stdout, "\nOK: ready")
	case usable:
		fmt.Fprintln(g.stdout, "\nWARN: usable with warnings")
	default:
		fmt.Fprintln(g.stdout, "\nMISSING: required tools or settings are missing")
	}
	if usable {
		return 0
	}
	return report.ExitCode(worst)
}

func (g *Global) scan(ctx context.Context, args []string) int {
	fs := g.subFlags("scan")
	tier := fs.String("verify", "", "decode verification: quick | full | none (default from profile)")
	clam := fs.Bool("clamav", false, "run clamscan on every file regardless of profile")
	quarantine := fs.String("quarantine", "", "move BLOCK files under this directory (mirrored tree)")
	if err := fs.Parse(args); err != nil {
		return int(report.Usage)
	}
	if fs.NArg() == 0 {
		return g.usageErr("scan: at least one path is required")
	}
	if *tier != "" && *tier != "quick" && *tier != "full" && *tier != "none" {
		return g.usageErr("scan: --verify must be quick, full or none")
	}
	if !g.DryRun {
		if msg := checkQuarantineRoots("scan", *quarantine, fs.Args()); msg != "" {
			return g.usageErr("%s", msg)
		}
	}
	t, err := g.setup(*quarantine != "")
	if err != nil {
		return g.usageErr("%v", err)
	}
	sc := &scan.Scanner{Runner: t.runner, Prober: t.prober, Verifier: t.verifier, Profile: t.profile,
		VerifyTier: *tier, ClamAV: *clam, Quarantine: *quarantine}
	if g.DryRun {
		sc.Quarantine = ""
	}
	s := report.NewSummary("amuxify", Version, "scan", t.profile.Name)
	sc.Progress = func(r scan.Result) { g.progress(r.File) }
	for _, p := range fs.Args() {
		res, err := sc.ScanPath(ctx, p)
		for _, r := range res {
			s.Append(r.File)
		}
		if err != nil {
			s.Error(fmt.Sprintf("%s: %v", p, err))
		}
	}
	return g.emit(s)
}

func (g *Global) remux(ctx context.Context, args []string) int {
	fs := g.subFlags("remux")
	output := fs.String("output", "", "output root (default: <root>__remuxed beside the input)")
	inPlace := fs.Bool("in-place", false, "replace each source after verification (extension becomes .mkv)")
	hardlinks := fs.String("hardlinks", "", "when the source has other hard links: skip | break | copy (default from profile)")
	tier := fs.String("verify", "", "quick | full | none (default from profile; none refused with --in-place)")
	force := fs.Bool("force", false, "remux files whose scan verdict is FAIL (BLOCK is never overridden)")
	original := fs.String("original-language", "", "original language of the title, used to pick the default audio")
	if err := fs.Parse(args); err != nil {
		return int(report.Usage)
	}
	if fs.NArg() == 0 {
		return g.usageErr("remux: at least one path is required")
	}
	if *tier != "" && *tier != "quick" && *tier != "full" && *tier != "none" {
		return g.usageErr("remux: --verify must be quick, full or none")
	}
	if *hardlinks != "" && *hardlinks != "skip" && *hardlinks != "break" && *hardlinks != "copy" {
		return g.usageErr("remux: --hardlinks must be skip, break or copy")
	}
	if *inPlace && *output != "" {
		return g.usageErr("remux: --in-place and --output are mutually exclusive")
	}
	t, err := g.setup(!g.DryRun)
	if err != nil {
		return g.usageErr("%v", err)
	}
	effectiveTier := *tier
	if effectiveTier == "" {
		effectiveTier = t.profile.Verify.Tier
	}
	if *inPlace && effectiveTier == "none" {
		return g.usageErr("remux: --in-place requires verification; --verify none is refused")
	}
	if !t.runner.Have(exec.MKVPropedit) {
		return g.usageErr("mkvpropedit not found; run 'amuxify doctor'")
	}
	sc := &scan.Scanner{Runner: t.runner, Prober: t.prober, Verifier: t.verifier, Profile: t.profile, VerifyTier: "none"}
	rm := &remux.Remuxer{Runner: t.runner, Prober: t.prober, Verifier: t.verifier, Scanner: sc, Profile: t.profile,
		OutputRoot: *output, InPlace: *inPlace, Hardlinks: *hardlinks, VerifyTier: *tier, Force: *force,
		DryRun: g.DryRun, Original: *original, Timeout: g.Timeout, Progress: g.progress}
	s := report.NewSummary("amuxify", Version, "remux", t.profile.Name)
	for _, p := range fs.Args() {
		res, err := rm.RemuxPath(ctx, p)
		for _, r := range res {
			s.Append(r)
		}
		if err != nil {
			s.Error(fmt.Sprintf("%s: %v", p, err))
		}
	}
	return g.emit(s)
}

func (g *Global) clean(ctx context.Context, args []string) int {
	fs := g.subFlags("clean")
	removeSidecars := fs.Bool("remove-blocked-sidecars", false, "delete sidecar files on the profile's block list")
	audioTags := fs.Bool("strip-audio-tags", false, "also rewrite mp3/flac/ogg/wav without tags")
	hardlinks := fs.String("hardlinks", "", "skip | break (default from profile)")
	if err := fs.Parse(args); err != nil {
		return int(report.Usage)
	}
	if fs.NArg() == 0 {
		return g.usageErr("clean: at least one path is required")
	}
	if *hardlinks != "" && *hardlinks != "skip" && *hardlinks != "break" {
		return g.usageErr("clean: --hardlinks must be skip or break")
	}
	t, err := g.setup(!g.DryRun)
	if err != nil {
		return g.usageErr("%v", err)
	}
	if !t.runner.Have(exec.MKVPropedit) {
		return g.usageErr("mkvpropedit not found; run 'amuxify doctor'")
	}
	cl := &clean.Cleaner{Runner: t.runner, Prober: t.prober, Verifier: t.verifier, Profile: t.profile,
		DryRun: g.DryRun, RemoveBlockedSidecars: *removeSidecars, StripAudioTags: *audioTags, Hardlinks: *hardlinks, Timeout: g.Timeout, Progress: g.progress}
	s := report.NewSummary("amuxify", Version, "clean", t.profile.Name)
	for _, p := range fs.Args() {
		res, err := cl.CleanPath(ctx, p)
		for _, r := range res {
			s.Append(r)
		}
		if err != nil {
			s.Error(fmt.Sprintf("%s: %v", p, err))
		}
	}
	return g.emit(s)
}
