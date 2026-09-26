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
	"os"
	osexec "os/exec"
	"strings"
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
	TimedOut bool
	Duration time.Duration
	Cmdline  string
}

// Runner locates and runs tools. Zero value is usable.
type Runner struct {
	// Timeout applies to every run unless RunWithTimeout is used.
	Timeout time.Duration
	// Trace, when set, receives each command line before it runs.
	Trace func(string)
	paths map[string]string
}

// ErrNotFound is returned when a tool is not installed.
var ErrNotFound = errors.New("tool not found")

// Path resolves a tool, honouring AMUXIFY_<TOOL> overrides, and caches it.
func (r *Runner) Path(tool string) (string, error) {
	if r.paths == nil {
		r.paths = map[string]string{}
	}
	if p, ok := r.paths[tool]; ok {
		return p, nil
	}
	env := "AMUXIFY_" + strings.ToUpper(tool)
	if p := os.Getenv(env); p != "" {
		if _, err := os.Stat(p); err != nil {
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
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	res := &Result{Cmdline: path + " " + strings.Join(args, " ")}
	if r.Trace != nil {
		r.Trace(res.Cmdline)
	}
	start := time.Now()
	runErr := cmd.Run()
	res.Duration = time.Since(start)
	res.Stdout = out.Bytes()
	res.Stderr = errb.Bytes()
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, fmt.Errorf("%s: timed out after %s", tool, timeout)
	}
	if runErr != nil {
		var ee *osexec.ExitError
		if errors.As(runErr, &ee) {
			res.ExitCode = ee.ExitCode()
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

// cleanEnv keeps only what tools need. In particular LD_PRELOAD and the like
// are dropped, and locale is pinned so output parsing is stable.
func cleanEnv() []string {
	keep := []string{"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "SystemRoot", "USERPROFILE"}
	env := []string{"LC_ALL=C", "LANG=C"}
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
