// Package exec runs the external tools amuxify depends on (ffmpeg, ffprobe,
// mkvmerge, mkvpropedit, exiftool, clamscan) with a timeout, a fixed
// environment, and, for the ffmpeg family, a protocol whitelist so a crafted
// file can never make ffmpeg open a network or device URL.
package exec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osexec "os/exec"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Tool names as used for lookup and for environment overrides such as
// AMUXIFY_FFMPEG=/opt/ffmpeg/bin/ffmpeg.
const (
	FFmpeg      = "ffmpeg"
	FFprobe     = "ffprobe"
	MKVMerge    = "mkvmerge"
	MKVPropedit = "mkvpropedit"
	MKVExtract  = "mkvextract"
	ExifTool    = "exiftool"
	ClamScan    = "clamscan"
)

// ProtocolWhitelist is passed to every ffmpeg and ffprobe invocation.
const ProtocolWhitelist = "file,pipe"

// Result is what a finished tool run produced.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	// Status is how the child ended when it did not exit 0, in the words
	// of the operating system: "exit status 1", or "signal: killed" for a
	// child that died on a signal, which ExitCode reports as -1. Empty for
	// a run that exited 0.
	Status   string
	TimedOut bool
	// StdoutTruncated and StderrTruncated are set when the child wrote more
	// than the runner's MaxOutput to that stream; Stdout or Stderr then
	// holds the first MaxOutput bytes and the rest was dropped while the
	// child kept running, so the child never blocked on a full pipe.
	// OutputTruncated is set when either was. A consumer that parses only
	// standard output checks StdoutTruncated, so that a tool whose
	// diagnostics on standard error ran past the bound does not have a
	// complete document on standard output thrown away.
	StdoutTruncated bool
	StderrTruncated bool
	OutputTruncated bool
	Duration        time.Duration
	Cmdline         string
}

// DefaultWaitDelay is how long a run waits, once the child has been killed
// on timeout or cancellation or has exited on its own, for the child's
// output pipes to close before the pipes are forced shut. A tool that hands
// its stdout to a grandchild and then dies would otherwise block the run
// for as long as the grandchild lives.
const DefaultWaitDelay = 5 * time.Second

// DefaultMaxOutput is how much of the child's standard output and of its
// standard error a run keeps, each, when the Runner does not set its own
// bound. A tool that floods its output, such as an antivirus scanner
// naming every signature it tried or a probe dumping every frame, cannot
// grow the process without limit; what it printed past the bound is read
// and dropped. The largest legitimate outputs amuxify reads back are a
// probe of a file with thousands of chapters and tags and a text subtitle
// track extracted as SRT for the link check, both of which stay far below
// this for any real file. Every consumer that reads back a whole output
// checks Result.StdoutTruncated, or OutputTruncated when it reads both
// streams, and reports a cut rather than treating the part it kept as the
// whole: a cut probe is an unparseable file and a cut subtitle track is
// reported as not fully checked. Streaming runs
// (RunStreaming) hand standard output to the caller's writer and are not
// bounded here.
const DefaultMaxOutput = 16 << 20

// Runner locates and runs tools. Zero value is usable.
type Runner struct {
	// Timeout applies to every run unless RunWithTimeout is used.
	Timeout time.Duration
	// WaitDelay bounds the wait for the child's output pipes after the child
	// was killed or exited; zero means DefaultWaitDelay.
	WaitDelay time.Duration
	// MaxOutput bounds how many bytes of standard output and of standard
	// error, each, a run keeps; zero means DefaultMaxOutput.
	MaxOutput int64
	// Trace, when set, receives each command line before it runs. With
	// parallel jobs it is called from several goroutines at once.
	Trace func(string)

	// mu guards paths, the resolved tool locations, which several workers
	// read and fill at the same time in a parallel run. A Runner must not
	// be copied once it is in use.
	mu    sync.Mutex
	paths map[string]string
}

func (r *Runner) waitDelay() time.Duration {
	if r.WaitDelay > 0 {
		return r.WaitDelay
	}
	return DefaultWaitDelay
}

func (r *Runner) maxOutput() int64 {
	if r.MaxOutput > 0 {
		return r.MaxOutput
	}
	return DefaultMaxOutput
}

// capWriter keeps the first max bytes written to it and counts the rest,
// which it accepts and drops so the writer never fails and the child never
// stalls on a full pipe.
type capWriter struct {
	buf     bytes.Buffer
	max     int64
	dropped int64
}

func (w *capWriter) Write(p []byte) (int, error) {
	room := w.max - int64(w.buf.Len())
	keep := int64(len(p))
	if keep > room {
		keep = room
	}
	if keep > 0 {
		w.buf.Write(p[:keep])
	} else {
		keep = 0
	}
	w.dropped += int64(len(p)) - keep
	return len(p), nil
}

// ErrNotFound is returned when a tool is not installed.
var ErrNotFound = errors.New("tool not found")

// Path resolves a tool, honouring AMUXIFY_<TOOL> overrides, and caches it.
func (r *Runner) Path(tool string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.paths == nil {
		r.paths = map[string]string{}
	}
	if p, ok := r.paths[tool]; ok {
		return p, nil
	}
	env := "AMUXIFY_" + strings.ToUpper(tool)
	if p := os.Getenv(env); p != "" {
		if err := checkExecutable(p); err != nil {
			return "", fmt.Errorf("%s=%s: %w", env, p, err)
		}
		r.paths[tool] = p
		return p, nil
	}
	p, err := osexec.LookPath(tool)
	if err != nil {
		return "", fmt.Errorf("%s: %w", tool, ErrNotFound)
	}
	r.paths[tool] = p
	return p, nil
}

// checkExecutable accepts only a regular file that the caller can run. An
// override that names a directory, a socket or a file without the
// executable bit would otherwise be reported as present by Have and only
// fail later when the tool is run. On Windows the executable bit carries no
// meaning, so only the regular-file check applies there.
func checkExecutable(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("not a regular file")
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("not executable")
	}
	return nil
}

// Have reports whether a tool is available.
func (r *Runner) Have(tool string) bool {
	_, err := r.Path(tool)
	return err == nil
}

// Run executes a tool with the runner's default timeout.
func (r *Runner) Run(ctx context.Context, tool string, args ...string) (*Result, error) {
	return r.RunWithTimeout(ctx, r.Timeout, tool, args...)
}

// RunWithTimeout executes a tool; a zero timeout means no limit. A non-zero
// exit status is not an error: callers inspect Result.ExitCode. An error is
// returned only when the tool could not be started or timed out.
func (r *Runner) RunWithTimeout(ctx context.Context, timeout time.Duration, tool string, args ...string) (*Result, error) {
	return r.run(ctx, timeout, tool, nil, args)
}

// RunStreaming is RunWithTimeout with the tool's standard output written to
// stdout as it is produced instead of being collected in Result.Stdout,
// which stays nil. It is for runs whose output is large and consumed once,
// such as decoded samples fed to a hash. Standard error is still collected.
// A write error from stdout is reported like any other failure of the run.
func (r *Runner) RunStreaming(ctx context.Context, timeout time.Duration, tool string, stdout io.Writer, args ...string) (*Result, error) {
	if stdout == nil {
		return nil, errors.New("RunStreaming needs a writer for standard output")
	}
	return r.run(ctx, timeout, tool, stdout, args)
}

func (r *Runner) run(ctx context.Context, timeout time.Duration, tool string, stdout io.Writer, args []string) (*Result, error) {
	path, err := r.Path(tool)
	if err != nil {
		return nil, err
	}
	if tool == FFmpeg || tool == FFprobe {
		args = ffGuard(tool, args)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := osexec.CommandContext(ctx, path, args...)
	cmd.Env = cleanEnv()
	cmd.Stdin = nil
	cmd.WaitDelay = r.waitDelay()
	out := &capWriter{max: r.maxOutput()}
	errb := &capWriter{max: r.maxOutput()}
	cmd.Stdout = out
	if stdout != nil {
		cmd.Stdout = stdout
	}
	cmd.Stderr = errb
	res := &Result{Cmdline: path + " " + strings.Join(args, " ")}
	if r.Trace != nil {
		r.Trace(res.Cmdline)
	}
	start := time.Now()
	runErr := cmd.Run()
	res.Duration = time.Since(start)
	if stdout == nil {
		res.Stdout = out.buf.Bytes()
	}
	res.Stderr = errb.buf.Bytes()
	res.StdoutTruncated = out.dropped > 0
	res.StderrTruncated = errb.dropped > 0
	res.OutputTruncated = res.StdoutTruncated || res.StderrTruncated
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, fmt.Errorf("%s: timed out after %s", tool, timeout)
	}
	if ctx.Err() != nil {
		// The caller's context was cancelled and the child was killed. That
		// is never a completed run, whatever exit status the kill left, so
		// it is reported as an error rather than as an exit code.
		if res.ExitCode = -1; runErr != nil {
			var ee *osexec.ExitError
			if errors.As(runErr, &ee) {
				res.ExitCode = ee.ExitCode()
				res.Status = ee.ProcessState.String()
			}
		}
		return res, fmt.Errorf("%s: %w", tool, ctx.Err())
	}
	if runErr != nil {
		var ee *osexec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
			res.Status = ee.ProcessState.String()
			return res, nil
		}
		return res, fmt.Errorf("%s: %w", tool, runErr)
	}
	return res, nil
}

// ffGuard prepends the safety flags every ffmpeg/ffprobe call must carry.
func ffGuard(tool string, args []string) []string {
	guard := []string{"-nostdin", "-hide_banner", "-protocol_whitelist", ProtocolWhitelist}
	if tool == FFprobe {
		guard = []string{"-hide_banner", "-protocol_whitelist", ProtocolWhitelist}
	}
	return append(guard, args...)
}

// FallbackLocale is the locale the tools run under when the caller's own
// environment does not name a UTF-8 locale.
const FallbackLocale = "C.UTF-8"

// Locale returns the locale every tool runs under. The tools need a UTF-8
// locale: under plain "C" mkvmerge treats its arguments as ASCII and
// truncates a path at the first non-ASCII byte. When the caller's LC_ALL,
// LC_CTYPE or LANG (checked in that order) already names a UTF-8 locale it
// is kept, because that locale is known to exist on the host, whereas
// C.UTF-8 is missing on older glibc systems and makes mkvmerge exit with
// "The locale could not be set properly". Otherwise C.UTF-8 is used.
func Locale() string {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(k); utf8Locale(v) {
			return v
		}
	}
	return FallbackLocale
}

// utf8Locale reports whether v is a plausible locale name that selects a
// UTF-8 encoding. Only the characters a locale name is built from are
// accepted (letters, digits, "_", "-", ".", "@" and "+"), so a value
// carrying whitespace, quotes or shell metacharacters is never forwarded.
func utf8Locale(v string) bool {
	if v == "" || len(v) > 64 {
		return false
	}
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.', c == '@', c == '+':
		default:
			return false
		}
	}
	l := strings.ToLower(v)
	return strings.Contains(l, "utf-8") || strings.Contains(l, "utf8")
}

// cleanEnv keeps only what tools need. In particular LD_PRELOAD and the like
// are dropped, and locale is pinned so output parsing is stable.
func cleanEnv() []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "SystemRoot", "USERPROFILE"}
	// The locale is pinned so tool output parses the same everywhere; see
	// Locale for why it is a UTF-8 one and where it comes from.
	// LC_ALL is left unset on purpose: LANG and LC_CTYPE carry the UTF-8
	// locale, and LC_MESSAGES=C asks every tool for English messages, so
	// the text the remuxer reads back (mkvmerge's "Warning:" lines) does
	// not change with the user's language.
	loc := Locale()
	env := []string{"LANG=" + loc, "LC_CTYPE=" + loc, "LC_MESSAGES=C"}
	for _, k := range keep {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// Version returns the first line of `<tool> --version` or `-version`.
func (r *Runner) Version(ctx context.Context, tool string) (string, error) {
	flag := "--version"
	switch tool {
	case FFmpeg, FFprobe:
		flag = "-version"
	case ExifTool:
		flag = "-ver"
	}
	res, err := r.RunWithTimeout(ctx, 20*time.Second, tool, flag)
	if err != nil {
		return "", err
	}
	line := strings.SplitN(strings.TrimSpace(string(res.Stdout)), "\n", 2)[0]
	if line == "" {
		line = strings.SplitN(strings.TrimSpace(string(res.Stderr)), "\n", 2)[0]
	}
	return line, nil
}
