package doctor

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/report"
)

func TestParseVersion(t *testing.T) {
	cases := map[string][]int{
		"mkvmerge v102.0 ('Truth') 64-bit": {102, 0, 0},
		"ffmpeg version 9.0.1 Copyright":   {9, 0, 1},
		"ffmpeg version n7.1-3":            {7, 1, 0},
		"ffmpeg version 4.4.2-0ubuntu0.22": {4, 4, 2},
		"13.55":                            {13, 55, 0},
	}
	for in, want := range cases {
		got, ok := parseVersion(in)
		if !ok || len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
			t.Errorf("%q: got %v ok=%v want %v", in, got, ok, want)
		}
	}
	if !less([]int{4, 3, 9}, []int{4, 4}) || less([]int{5, 0}, []int{4, 4}) || less([]int{50, 0}, []int{50, 0}) {
		t.Fatal("less wrong")
	}
	if _, ok := parseVersion("ffmpeg version N-112345-gabcdef"); ok {
		t.Fatal("git build should be unparseable")
	}
}

// fakeMkvmerge installs a shell script as mkvmerge that prints the given
// text to stdout and stderr and exits with code.
func fakeMkvmerge(t *testing.T, stdout, stderr string, code int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake mkvmerge is a POSIX shell script")
	}
	p := filepath.Join(t.TempDir(), "mkvmerge")
	body := "#!/bin/sh\nprintf '%s\\n' " + shq(stdout) + "\nprintf '%s\\n' " + shq(stderr) + " >&2\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_MKVMERGE", p)
}

func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func localeOf(checks []Check) (Check, bool) {
	for _, c := range checks {
		if c.Name == "locale" {
			return c, true
		}
	}
	return Check{}, false
}

// The locale check warns, in a full sentence naming the fix, when mkvmerge
// rejects the locale the tools are given, whether it says so on stdout or
// stderr, and passes when mkvmerge starts. It never runs a tool through a
// shell: a hostile LANG cannot reach the fake mkvmerge.
func TestLocaleCheck(t *testing.T) {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		t.Setenv(k, "")
	}
	t.Run("rejected on stdout", func(t *testing.T) {
		fakeMkvmerge(t, "The locale could not be set properly. Check that the LC_ALL, LC_CTYPE and LANG environment variables are set correctly.", "", 1)
		c, ok := localeOf(runChecks(t))
		if !ok || c.Status != report.Warn {
			t.Fatalf("locale check %+v", c)
		}
		for _, want := range []string{"C.UTF-8", "Export a UTF-8 locale", "LANG=en_US.UTF-8", "The locale could not be set properly"} {
			if !strings.Contains(c.Detail, want) {
				t.Errorf("detail lacks %q: %s", want, c.Detail)
			}
		}
	})
	t.Run("rejected on stderr with exit 0", func(t *testing.T) {
		fakeMkvmerge(t, "mkvmerge v85.0 ('Nightingale') 64-bit", "Warning: the locale could not be set properly", 0)
		c, _ := localeOf(runChecks(t))
		if c.Status != report.Warn {
			t.Fatalf("locale check %+v", c)
		}
	})
	t.Run("accepted", func(t *testing.T) {
		fakeMkvmerge(t, "mkvmerge v85.0 ('Nightingale') 64-bit", "", 0)
		c, _ := localeOf(runChecks(t))
		if c.Status != report.Pass || c.Detail != "C.UTF-8" {
			t.Fatalf("locale check %+v", c)
		}
	})
	t.Run("caller's locale is the one tested", func(t *testing.T) {
		t.Setenv("LANG", "en_US.UTF-8")
		fakeMkvmerge(t, "mkvmerge v85.0 ('Nightingale') 64-bit", "", 0)
		c, _ := localeOf(runChecks(t))
		if c.Status != report.Pass || c.Detail != "en_US.UTF-8" {
			t.Fatalf("locale check %+v", c)
		}
	})
	t.Run("hostile LANG never reaches the tool", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "pwned")
		t.Setenv("LANG", "en_US.UTF-8; touch "+marker)
		fakeMkvmerge(t, "mkvmerge v85.0 ('Nightingale') 64-bit", "", 0)
		c, _ := localeOf(runChecks(t))
		if c.Status != report.Pass || c.Detail != "C.UTF-8" {
			t.Fatalf("locale check %+v", c)
		}
		if _, err := os.Lstat(marker); err == nil {
			t.Fatal("marker file created")
		}
	})
	t.Run("mkvmerge missing", func(t *testing.T) {
		t.Setenv("AMUXIFY_MKVMERGE", filepath.Join(t.TempDir(), "none"))
		c, ok := localeOf(runChecks(t))
		if !ok || c.Status != report.Pass || !strings.Contains(c.Detail, "not tested") {
			t.Fatalf("locale check %+v", c)
		}
	})
}

// runChecks runs the doctor with every other tool pointed at a missing
// path so the result does not depend on what the host has installed.
func runChecks(t *testing.T) []Check {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "no-such-tool")
	for _, tool := range []string{exec.FFmpeg, exec.FFprobe, exec.MKVPropedit, exec.MKVExtract, exec.ExifTool, exec.ClamScan} {
		t.Setenv("AMUXIFY_"+strings.ToUpper(tool), missing)
	}
	checks, _ := Run(context.Background(), &exec.Runner{Timeout: 10 * time.Second}, "homelab", "")
	return checks
}
