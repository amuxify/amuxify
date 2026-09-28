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
	fakeTool(t, exec.MKVMerge, stdout, stderr, code)
}

// fakeTool installs a shell script under the tool's override variable
// that prints the given text to stdout and stderr and exits with code.
func fakeTool(t *testing.T, tool, stdout, stderr string, code int) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake tool is a POSIX shell script")
	}
	p := filepath.Join(t.TempDir(), tool)
	body := "#!/bin/sh\nprintf '%s\\n' " + shq(stdout) + "\nprintf '%s\\n' " + shq(stderr) + " >&2\nexit " + strconv.Itoa(code) + "\n"
	if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_"+strings.ToUpper(tool), p)
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

// runChecks runs the doctor for the homelab profile with every tool but
// mkvmerge pointed at a missing path so the result does not depend on
// what the host has installed.
func runChecks(t *testing.T) []Check {
	t.Helper()
	checks, _ := runProfile(t, "homelab", exec.MKVMerge)
	return checks
}

// runProfile runs the doctor for the named profile with every tool not
// listed in keep pointed at a missing path, and returns the checks and the
// worst status.
func runProfile(t *testing.T, profile string, keep ...string) ([]Check, report.Severity) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "no-such-tool")
	for _, tool := range []string{exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract, exec.ExifTool, exec.ClamScan} {
		kept := false
		for _, k := range keep {
			kept = kept || k == tool
		}
		if !kept {
			t.Setenv("AMUXIFY_"+strings.ToUpper(tool), missing)
		}
	}
	return Run(context.Background(), &exec.Runner{Timeout: 10 * time.Second}, profile, "")
}

func checkOf(checks []Check, name string) (Check, bool) {
	for _, c := range checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

// The clamscan rows: the version line is reported bounded and sanitised,
// and the signature database version and age are read from it into an
// informational row that never changes the exit status. Without a
// database the row says so; with an old one it names freshclam.
func TestClamscanVersionAndDatabaseAge(t *testing.T) {
	fixed := time.Date(2026, 9, 28, 12, 0, 0, 0, time.Local)
	orig := now
	now = func() time.Time { return fixed }
	t.Cleanup(func() { now = orig })
	cases := []struct {
		name, line, want string
	}{
		{"fresh", "ClamAV 1.2.1/27000/Sat Sep 26 08:33:45 2026", "signatures 27000 from 2026-09-26 (2 day(s) old)"},
		{"zero-padded day", "ClamAV 0.103.8/27001/Sat Sep 05 07:46:16 2026", "signatures 27001 from 2026-09-05 (23 day(s) old); run freshclam to update them"},
		{"stale", "ClamAV 1.4.0/26900/Mon May 11 07:46:16 2026", "signatures 26900 from 2026-05-11 (140 day(s) old); run freshclam to update them"},
		{"future", "ClamAV 1.2.1/27002/Wed Oct 14 08:33:45 2026", "signatures 27002 from 2026-10-14 (0 day(s) old)"},
		{"no database", "ClamAV 1.2.1", "no signature database version in the clamscan version line; run freshclam to download one"},
		{"unparseable date", "ClamAV 1.2.1/27000/yesterday", "signatures 27000 from yesterday (age unknown: the date did not parse)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeTool(t, exec.ClamScan, tc.line, "", 0)
			checks, worst := runProfile(t, "archive", exec.ClamScan)
			tool, ok := checkOf(checks, "clamscan")
			if !ok || tool.Status != report.Pass || !strings.HasSuffix(tool.Detail, " ("+tc.line+")") {
				t.Fatalf("clamscan row %+v", tool)
			}
			db, ok := checkOf(checks, "clamav-db")
			if !ok || db.Status != report.Pass || db.Required || db.Detail != tc.want {
				t.Fatalf("clamav-db row %+v, want detail %q", db, tc.want)
			}
			if _, ok := checkOf(checks, "clamav"); ok {
				t.Fatal("clamav row present although clamscan is installed")
			}
			// Only the missing required tools decide the status.
			if worst != report.Usage {
				t.Fatalf("worst %s with mkvmerge and ffmpeg missing", worst)
			}
		})
	}
	t.Run("hostile version line", func(t *testing.T) {
		line := "ClamAV 1.2.1/27000/Sat Sep 26 08:33:45 2026\x1b[2K\r\u202ePASS everything" + strings.Repeat("x", 1000)
		fakeTool(t, exec.ClamScan, line, "", 0)
		checks, _ := runProfile(t, "archive", exec.ClamScan)
		for _, name := range []string{"clamscan", "clamav-db"} {
			c, _ := checkOf(checks, name)
			for _, bad := range []string{"\x1b", "\r", "\u202e"} {
				if strings.Contains(c.Detail, bad) {
					t.Errorf("%s row carries a raw control or format character: %q", name, c.Detail)
				}
			}
			if len(c.Detail) > 2*versionBytes {
				t.Errorf("%s row not bounded: %d bytes", name, len(c.Detail))
			}
		}
		c, _ := checkOf(checks, "clamscan")
		if !strings.Contains(c.Detail, `\x1b[2K\x0d\u202ePASS`) || !strings.HasSuffix(c.Detail, "...)") {
			t.Errorf("clamscan row not sanitised and cut: %q", c.Detail)
		}
		if !strings.Contains(Format(checks), `\u202ePASS`) {
			t.Error("the terminal form carries the raw override")
		}
	})
	t.Run("version cannot be read", func(t *testing.T) {
		fakeTool(t, exec.ClamScan, "", "", 0)
		checks, _ := runProfile(t, "archive", exec.ClamScan)
		db, ok := checkOf(checks, "clamav-db")
		if !ok || db.Status != report.Pass || !strings.HasPrefix(db.Detail, "no signature database version") {
			t.Fatalf("clamav-db row %+v", db)
		}
	})
}

// A profile that requires clamscan fails the doctor (exit 2 in the CLI)
// with the same message as before when clamscan is missing, and there is
// no database row to report on. An optional profile only warns.
func TestClamscanMissingWithRequiredProfile(t *testing.T) {
	checks, worst := runProfile(t, "strict")
	c, ok := checkOf(checks, "clamav")
	if !ok || c.Status != report.Usage || !c.Required || c.Detail != "profile requires clamscan but it is not installed" {
		t.Fatalf("clamav row %+v", c)
	}
	if worst != report.Usage {
		t.Fatalf("worst %s", worst)
	}
	if _, ok := checkOf(checks, "clamav-db"); ok {
		t.Fatal("clamav-db row present without clamscan")
	}
	tool, _ := checkOf(checks, "clamscan")
	if tool.Status != report.Warn || tool.Required {
		t.Fatalf("clamscan row %+v", tool)
	}
	checks, _ = runProfile(t, "archive", exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract, exec.FFmpeg, exec.FFprobe)
	if _, ok := checkOf(checks, "clamav"); ok {
		t.Fatal("optional profile produced the required-clamscan row")
	}
}

// A clamscan that is installed and executable but cannot be run at all,
// here one whose interpreter does not exist, is a third state between
// missing and present: the tool row can only warn that the version could
// not be read. Under a profile that requires the scan every media file
// would be FAIL CLAMAV_ERROR, so the doctor must report a missing
// requirement (exit 2 in the CLI) and not a warning; under an optional
// profile the warning is all there is.
func TestClamscanCannotRunWithRequiredProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a POSIX interpreter line")
	}
	p := filepath.Join(t.TempDir(), "clamscan")
	if err := os.WriteFile(p, []byte("#!"+filepath.Join(t.TempDir(), "no-such-shell")+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_CLAMSCAN", p)
	checks, worst := runProfile(t, "strict", exec.ClamScan, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract, exec.FFmpeg, exec.FFprobe)
	tool, ok := checkOf(checks, "clamscan")
	if !ok || tool.Status != report.Warn || !strings.Contains(tool.Detail, "cannot read version") {
		t.Fatalf("clamscan row %+v", tool)
	}
	c, ok := checkOf(checks, "clamav")
	if !ok || c.Status != report.Usage || !c.Required || !strings.HasPrefix(c.Detail, "profile requires clamscan but it cannot be run: ") {
		t.Fatalf("clamav row %+v", c)
	}
	if worst != report.Usage {
		t.Fatalf("worst %s", worst)
	}
	if _, ok := checkOf(checks, "clamav-db"); ok {
		t.Fatal("clamav-db row present for a clamscan that printed no version")
	}
	if n := strings.Count(Format(checks), "MISSING  clamav "); n != 1 {
		t.Fatalf("%d MISSING clamav rows in the terminal form:\n%s", n, Format(checks))
	}
	checks, _ = runProfile(t, "archive", exec.ClamScan, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract, exec.FFmpeg, exec.FFprobe)
	if _, ok := checkOf(checks, "clamav"); ok {
		t.Fatal("optional profile produced the required-clamscan row")
	}
	other := report.Pass
	for _, c := range checks {
		if c.Name != "clamscan" && c.Status > other {
			other = c.Status
		}
	}
	if tool, _ := checkOf(checks, "clamscan"); tool.Status != report.Warn || other > report.Warn {
		t.Fatalf("archive: clamscan row %+v, other rows %s", tool, other)
	}
}

// The scan passes --alert-exceeds-max, which ClamAV 0.103 introduced, so
// the doctor holds clamscan to that floor the way it holds the other tools
// to theirs: an older release is reported too old (exit 2 in the CLI,
// since every scan with it would be CLAMAV_ERROR), the floor release and
// every later one pass, and a version line the floor cannot be read from
// is a warning rather than a pass.
func TestClamscanVersionFloor(t *testing.T) {
	cases := []struct {
		line string
		want report.Severity
	}{
		{"ClamAV 0.102.4/27000/Sat Sep 26 08:33:45 2026", report.Usage},
		{"ClamAV 0.99.4", report.Usage},
		{"ClamAV 0.103.0/27000/Sat Sep 26 08:33:45 2026", report.Pass},
		{"ClamAV 0.103.8/27000/Sat Sep 26 08:33:45 2026", report.Pass},
		{"ClamAV 1.5.4/28134/Fri Sep 25 08:25:58 2026", report.Pass},
		{"clamscan, no version here", report.Warn},
	}
	for _, tc := range cases {
		t.Run(tc.line, func(t *testing.T) {
			fakeTool(t, exec.ClamScan, tc.line, "", 0)
			checks, _ := runProfile(t, "archive", exec.ClamScan)
			tool, ok := checkOf(checks, "clamscan")
			if !ok || tool.Status != tc.want {
				t.Fatalf("clamscan row %+v, want %s", tool, tc.want)
			}
			if tc.want == report.Usage && !strings.Contains(tool.Detail, "is older than ClamAV 0.103 or newer for --alert-exceeds-max") {
				t.Fatalf("detail %q", tool.Detail)
			}
		})
	}
}

// Under a profile that requires the scan, a clamscan with no signature
// database is a missing requirement (exit 2 in the CLI), because every
// scan would exit 2 and fail every media file; with a database, old or
// not, the row passes as it does for an optional profile. The row is
// keyed on the version line, so a scanner whose version cannot be parsed
// for a database counts as having none.
func TestClamscanDatabaseRequiredByProfile(t *testing.T) {
	cases := []struct {
		name, line string
		want       report.Severity
	}{
		{"no database", "ClamAV 1.2.1", report.Usage},
		{"blank version", "", report.Usage},
		{"hostile version", "ClamAV 1.2.1\x1b[2K\r‮/27000/Sat Sep 26 08:33:45 2026", report.Pass},
		{"stale database", "ClamAV 1.4.0/26900/Mon May 11 07:46:16 2026", report.Pass},
		{"fresh database", "ClamAV 1.2.1/27000/Sat Sep 26 08:33:45 2026", report.Pass},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeTool(t, exec.ClamScan, tc.line, "", 0)
			checks, worst := runProfile(t, "strict", exec.ClamScan, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract, exec.FFmpeg, exec.FFprobe)
			db, ok := checkOf(checks, "clamav-db")
			if !ok || db.Status != tc.want || db.Required != (tc.want == report.Usage) {
				t.Fatalf("clamav-db row %+v, want %s", db, tc.want)
			}
			if _, ok := checkOf(checks, "clamav"); ok {
				t.Fatal("clamav row present although clamscan is installed")
			}
			if tc.want == report.Usage {
				if db.Detail != "profile requires clamscan but it has no signature database; run freshclam to download one" {
					t.Fatalf("detail %q", db.Detail)
				}
				if !strings.Contains(Format(checks), "MISSING  clamav-db    profile requires clamscan but it has no signature database") {
					t.Fatalf("terminal form:\n%s", Format(checks))
				}
			}
			// Only the database row can raise the status here: the required
			// tools resolve to real or stubbed executables whose version
			// lines the doctor may or may not accept, so the worst status is
			// checked against every other row rather than against a fixed
			// value.
			other := report.Pass
			for _, c := range checks {
				if c.Name != "clamav-db" && c.Status > other {
					other = c.Status
				}
			}
			if tc.want == report.Usage && worst != report.Usage {
				t.Fatalf("worst %s", worst)
			}
			if tc.want == report.Pass && worst != other {
				t.Fatalf("worst %s with the database row passing (other rows %s)", worst, other)
			}
			// An optional profile keeps the same version line informational.
			checks, _ = runProfile(t, "archive", exec.ClamScan)
			if db, _ := checkOf(checks, "clamav-db"); db.Status != report.Pass || db.Required {
				t.Fatalf("archive clamav-db row %+v", db)
			}
		})
	}
}
