package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/testutil"
)

// ebml is a minimal Matroska header, enough for the scan to reach the
// clamscan step: the file sniffs as media and carries no earlier BLOCK.
const ebml = "\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1f\x42\x86\x81\x01\x42\x82\x88matroska"

// noClamscan points the clamscan override at a missing path so the result
// does not depend on whether the host has ClamAV installed. The optional
// tools that Stubs leaves alone are pointed away as well.
func noClamscan(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "no-such-tool")
	for _, tool := range []string{exec.ClamScan, exec.MKVExtract, exec.ExifTool} {
		t.Setenv("AMUXIFY_"+strings.ToUpper(tool), missing)
	}
}

// fakeClamscan installs a POSIX shell script as clamscan that names the
// scanned file (its last argument) as infected and exits 1.
func fakeClamscan(t *testing.T) {
	t.Helper()
	fakeClamscanScript(t, `printf '%s: Eicar-Test-Signature FOUND\n' "$last"; exit 1`)
}

// fakeClamscanScript installs a POSIX shell script as clamscan with the
// given body, in which "$last" is the scanned path.
func fakeClamscanScript(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake clamscan is a POSIX shell script")
	}
	noClamscan(t)
	p := filepath.Join(t.TempDir(), "clamscan")
	script := "#!/bin/sh\nfor last; do :; done\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_CLAMSCAN", p)
}

// findingCodes returns the finding codes of every file in a JSON report.
func findingCodes(t *testing.T, data string) []string {
	t.Helper()
	var doc struct {
		Files []struct {
			Findings []struct {
				Code string `json:"code"`
			} `json:"findings"`
		} `json:"files"`
	}
	if err := json.Unmarshal([]byte(data), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, data)
	}
	var codes []string
	for _, f := range doc.Files {
		for _, fd := range f.Findings {
			codes = append(codes, fd.Code)
		}
	}
	return codes
}

func hasCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// The strict profile requires clamscan. With clamscan missing every entry
// point reports the media file FAIL CLAMAV_MISSING and exits the way a
// FAIL exits there: scan 3, ingest 3 (the file is refused, --force does
// not help because the scan produced no probe result), the hook adapters
// their caller's failure code under the default --fail-on fail and their
// success code under --fail-on block, and doctor 2 with its fixed message.
// Nothing under the directory changes, and --clamav never lowers the
// requirement.
func TestClamscanMissingWithStrictProfile(t *testing.T) {
	testutil.Stubs(t)
	noClamscan(t)
	asUser(t, 1000)
	dir := t.TempDir()
	media := write(t, filepath.Join(dir, "a.mkv"), ebml)
	before := tree(t, dir)
	wantLine := "      FAIL  CLAMAV_MISSING     clamscan required by profile but not installed\n"

	for _, args := range [][]string{
		{"--profile", "strict", "scan", media},
		{"--profile", "strict", "scan", "--clamav", media},
		{"--profile", "strict", "scan", "--verify", "none", media},
		{"--profile", "strict", "ingest", media},
		{"--profile", "strict", "ingest", "--force", media},
		{"--profile", "strict", "--dry-run", "ingest", media},
	} {
		code, out, errs := run(t, args...)
		if code != 3 {
			t.Errorf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errs)
		}
		if !strings.HasPrefix(out, "FAIL  "+media+"\n"+wantLine) {
			t.Errorf("%v: output\n%s", args, out)
		}
		if !strings.HasSuffix(out, "\nFAIL: 1 file(s) FAIL=1\n") {
			t.Errorf("%v: tail\n%s", args, out)
		}
		code, out, _ = run(t, append([]string{"--json"}, args...)...)
		codes := findingCodes(t, out)
		if code != 3 || !hasCode(codes, scan.CodeClamMissing) || hasCode(codes, scan.CodeClamInfected) {
			t.Errorf("%v --json: exit %d codes %v", args, code, codes)
		}
	}
	code, _, _ := run(t, "--profile", "strict", "--json", "ingest", "--force", media)
	if code != 3 {
		t.Errorf("forced ingest without clamscan: exit %d", code)
	}

	matrix := map[string]map[string]int{ // adapter -> fail-on -> exit
		"sabnzbd": {"fail": 1, "block": 0},
		"nzbget":  {"fail": 94, "block": 93},
		"sonarr":  {"fail": 1, "block": 0},
		"radarr":  {"fail": 1, "block": 0},
	}
	for _, a := range adapters {
		for _, failOn := range []string{"", "fail", "block"} {
			args := []string{"--profile", "strict", "hook", a}
			if failOn != "" {
				args = append(args, "--fail-on", failOn)
			}
			want := matrix[a]["fail"]
			if failOn != "" {
				want = matrix[a][failOn]
			}
			hookEnv(t, jobEnv(a, media)...)
			code, out, errs := run(t, args...)
			if code != want {
				t.Errorf("%v: exit %d, want %d\nstdout: %s\nstderr: %s", args, code, want, out, errs)
			}
			if !strings.Contains(out, "FAIL  "+media+"\n") || !strings.Contains(out, "CLAMAV_MISSING") {
				t.Errorf("%v: no FAIL line for the file:\n%s", args, out)
			}
			if a == "nzbget" {
				checkNZBGetOutput(t, out, errs, false)
			}
			if a == "nzbget" {
				// NZBGet logs stdout line by line, so --json is refused
				// there; the JSON form is covered by the other adapters.
				continue
			}
			hookEnv(t, jobEnv(a, media)...)
			code, out, _ = run(t, append([]string{"--json"}, args...)...)
			codes := findingCodes(t, out)
			if code != want || !hasCode(codes, scan.CodeClamMissing) {
				t.Errorf("%v --json: exit %d codes %v", args, code, codes)
			}
		}
	}
	unchanged(t, before, tree(t, dir))

	code, out, _ := run(t, "--profile", "strict", "doctor")
	if code != 2 || !strings.Contains(out, "MISSING  clamav       profile requires clamscan but it is not installed\n") {
		t.Errorf("doctor: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "WARN     clamscan     not found (optional: safety.clamav = optional|required)\n") || strings.Contains(out, "clamav-db") {
		t.Errorf("doctor tool rows:\n%s", out)
	}
	code, out, _ = run(t, "--profile", "strict", "--json", "doctor")
	if code != 2 || !hasCode(findingCodes(t, out), "CLAMAV") {
		t.Errorf("doctor --json: exit %d\n%s", code, out)
	}
	// The profiles that do not require clamscan carry on without it: the
	// stubbed ffprobe cannot parse the file, so the verdict is FAIL for
	// another reason, and no CLAMAV code appears.
	for _, p := range []string{"homelab", "archive"} {
		_, out, _ := run(t, "--profile", p, "--json", "scan", "--clamav", media)
		codes := findingCodes(t, out)
		if hasCode(codes, scan.CodeClamMissing) || hasCode(codes, scan.CodeClamError) {
			t.Errorf("%s --clamav without clamscan: %v", p, codes)
		}
	}
}

// Guarantee 9: an infected file is BLOCK at every entry point and no flag
// lowers it. scan exits 4, ingest refuses the file with or without
// --force, and every hook adapter reports its caller's failure code even
// under --fail-on block; NZBGet gets its MARK=BAD line. The scanner's
// output reaches the report only as the sanitised line about this file.
func TestClamscanInfectedBlocksEverywhere(t *testing.T) {
	testutil.Stubs(t)
	fakeClamscan(t)
	asUser(t, 1000)
	dir := t.TempDir()
	media := write(t, filepath.Join(dir, "a.mkv"), ebml)
	before := tree(t, dir)
	for _, args := range [][]string{
		{"--profile", "strict", "scan", media},
		{"--profile", "archive", "scan", media},
		{"--profile", "homelab", "scan", "--clamav", media},
		{"--profile", "strict", "ingest", media},
		{"--profile", "strict", "ingest", "--force", media},
		{"--profile", "archive", "ingest", "--force", media},
	} {
		code, out, errs := run(t, append([]string{"--verbose"}, args...)...)
		if code != 4 || !strings.HasPrefix(out, "BLOCK "+media+"\n      BLOCK CLAMAV_INFECTED    clamscan reports infected\n            "+media+": Eicar-Test-Signature FOUND\n") {
			t.Errorf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errs)
		}
	}
	// homelab says off: without the flag clamscan does not run.
	_, out, _ := run(t, "--profile", "homelab", "--json", "scan", media)
	if hasCode(findingCodes(t, out), scan.CodeClamInfected) {
		t.Error("homelab ran clamscan without --clamav")
	}
	for _, a := range adapters {
		for _, failOn := range []string{"fail", "block"} {
			want := 1
			if a == "nzbget" {
				want = 94
			}
			hookEnv(t, jobEnv(a, media)...)
			code, out, errs := run(t, "--profile", "archive", "hook", a, "--fail-on", failOn)
			if code != want || !strings.Contains(out, "CLAMAV_INFECTED") {
				t.Errorf("hook %s --fail-on %s: exit %d, want %d\nstdout: %s\nstderr: %s", a, failOn, code, want, out, errs)
			}
			if a == "nzbget" {
				checkNZBGetOutput(t, out, errs, true)
			}
		}
	}
	unchanged(t, before, tree(t, dir))
}

// Guarantee 9, from the command line: under the strict profile a
// --timeout short enough to kill clamscan does not turn the scan into a
// pass. scan and ingest report FAIL CLAMAV_ERROR and exit 3, --force does
// not rebuild the file, every hook adapter reports its caller's failure
// code under the default --fail-on, and nothing under the directory
// changes. A clamscan with no signature database is the same FAIL at run
// time, and doctor --profile strict exits 2 for it with the database row
// marked missing, so the two states doctor reports as ready and the scan
// lets through agree.
func TestClamscanErrorFailsStrictEverywhere(t *testing.T) {
	testutil.Stubs(t)
	asUser(t, 1000)
	dir := t.TempDir()
	media := write(t, filepath.Join(dir, "a.mkv"), ebml)
	before := tree(t, dir)
	fakeClamscanScript(t, `exec sleep 5`)
	for _, args := range [][]string{
		{"--profile", "strict", "--timeout", "300ms", "scan", media},
		{"--profile", "strict", "--timeout", "300ms", "scan", "--clamav", media},
		{"--profile", "strict", "--timeout", "300ms", "ingest", media},
		{"--profile", "strict", "--timeout", "300ms", "ingest", "--force", media},
	} {
		code, out, errs := run(t, args...)
		if code != 3 || !strings.HasPrefix(out, "FAIL  "+media+"\n      FAIL  CLAMAV_ERROR       clamscan: timed out after 300ms\n") {
			t.Errorf("%v: exit %d\nstdout: %s\nstderr: %s", args, code, out, errs)
		}
		if strings.Contains(out, "UNPARSEABLE") {
			t.Errorf("%v: the file was probed after the scanner was killed:\n%s", args, out)
		}
	}
	for _, a := range adapters {
		want := 1
		if a == "nzbget" {
			want = 94
		}
		hookEnv(t, jobEnv(a, media)...)
		code, out, errs := run(t, "--profile", "strict", "--timeout", "300ms", "hook", a)
		if code != want || !strings.Contains(out, "FAIL  CLAMAV_ERROR") {
			t.Errorf("hook %s: exit %d, want %d\nstdout: %s\nstderr: %s", a, code, want, out, errs)
		}
	}
	unchanged(t, before, tree(t, dir))
	// The archive profile under the same timeout warns and carries on.
	code, out, _ := run(t, "--profile", "archive", "--timeout", "300ms", "--json", "scan", media)
	codes := findingCodes(t, out)
	if code != 3 || !hasCode(codes, scan.CodeClamError) || !hasCode(codes, scan.CodeUnparseable) {
		t.Errorf("archive: exit %d codes %v", code, codes)
	}

	fakeClamscanScript(t, `case "$1" in --version) printf 'ClamAV 1.4.2\n'; exit 0;; esac; printf 'LibClamAV Error: cli_loaddbdir: No supported database files found in /var/lib/clamav\n' >&2; exit 2`)
	code, out, _ = run(t, "--profile", "strict", "--verbose", "scan", media)
	if code != 3 || !strings.Contains(out, "      FAIL  CLAMAV_ERROR       clamscan exit 2: LibClamAV Error: cli_loaddbdir: No supported database files found in /var/lib/clamav\n") {
		t.Errorf("no database: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "--profile", "strict", "doctor")
	if code != 2 || !strings.Contains(out, "MISSING  clamav-db    profile requires clamscan but it has no signature database; run freshclam to download one\n") {
		t.Errorf("doctor without a database: exit %d\n%s", code, out)
	}
	if !strings.Contains(out, "PASS     clamscan     ") || strings.Contains(out, "MISSING  clamav       ") {
		t.Errorf("doctor tool rows:\n%s", out)
	}
	code, out, _ = run(t, "--profile", "archive", "doctor")
	if strings.Contains(out, "MISSING  clamav-db") || !strings.Contains(out, "PASS     clamav-db    no signature database version") {
		t.Errorf("archive doctor without a database: exit %d\n%s", code, out)
	}
}
