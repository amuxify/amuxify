package scan

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/report"
)

// fakeClamscan installs a POSIX shell script as clamscan. The script body
// runs with the arguments amuxify passes ("--no-summary --infected", the
// limit options, "--" and the path), so the last positional, which the
// script's first line puts in $last, is the scanned path. Every other
// tool is pointed at a missing path first, so what the scan does after
// clamscan (attempt the probe) is deterministic: ffprobe is absent and the
// file is reported UNPARSEABLE, which is how the tests tell that the run
// carried on past the scanner.
func fakeClamscan(t *testing.T, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake clamscan is a POSIX shell script")
	}
	noTools(t)
	p := filepath.Join(t.TempDir(), "clamscan")
	script := "#!/bin/sh\nfor last; do :; done\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_CLAMSCAN", p)
}

// clamScanner returns a scanner for the named profile with a short tool
// timeout, and a media file for it to scan. The file is a Matroska header
// so the scan reaches the clamscan step and not an earlier BLOCK.
func clamScanner(t *testing.T, profile string) (*Scanner, string) {
	t.Helper()
	s := newScanner(t, mustProfile(t, profile), nil)
	s.Timeout = 10 * time.Second
	media := write(t, filepath.Join(t.TempDir(), "movie.mkv"), ebml, 0o644)
	return s, media
}

func finding(fr report.FileResult, code string) report.Finding {
	for _, f := range fr.Findings {
		if f.Code == code {
			return f
		}
	}
	return report.Finding{}
}

// hostileText holds every kind of byte a report must never carry raw: an
// ANSI colour sequence, a cursor-erasing sequence, a carriage return that
// would overwrite the line, a bidirectional override and a zero-width
// space that would reorder or hide it, and a NUL.
const hostileText = "\x1b[31mFOUND\x1b[0m \x1b[2K\rPASS \u202eEVIL\u200b\x00"

// cleanOf reports whether s carries none of the raw bytes of hostileText
// that the sanitiser must have escaped.
func cleanOf(s string) bool {
	for _, bad := range []string{"\x1b", "\r", "\u202e", "\u200b", "\x00"} {
		if strings.Contains(s, bad) {
			return false
		}
	}
	return true
}

// Guarantee 9: an infected file is BLOCK from the exit status alone, the
// scan stops there, the detail names the signature from the line about
// this file and nothing else, and with a quarantine set the file is moved
// out of the tree.
func TestClamscanInfectedBlocks(t *testing.T) {
	fakeClamscan(t, `printf '%s: Eicar-Test-Signature FOUND\n' "$last"; exit 1`)
	s, media := clamScanner(t, "archive")
	fr := scanOne(t, s, media)
	expect(t, fr, report.Block, CodeClamInfected)
	if fr.Has(CodeUnparseable) {
		t.Fatal("probe ran after clamscan reported the file infected")
	}
	f := finding(fr, CodeClamInfected)
	if f.Detail != media+": Eicar-Test-Signature FOUND" {
		t.Fatalf("detail %q", f.Detail)
	}
	// The same with a quarantine: BLOCK moves the file, guarantee 1 and 3
	// mechanics are covered by the quarantine tests.
	q := filepath.Join(t.TempDir(), "quarantine")
	s.Quarantine = q
	fr = scanOne(t, s, media)
	expect(t, fr, report.Block, CodeClamInfected, CodeQuarantined)
	if _, err := os.Lstat(media); err == nil {
		t.Fatal("infected file still in the tree")
	}
	if _, err := os.Lstat(filepath.Join(q, "movie.mkv")); err != nil {
		t.Fatalf("infected file not in quarantine: %v", err)
	}
}

// A scanner error (exit 2) under an optional scan is WARN CLAMAV_ERROR,
// the run carries on to the probe, and the garbage on stderr reaches the
// message bounded and sanitised: the first line only, cut to a bounded
// length, every control byte escaped. The required profile's FAIL for the
// same exit is TestClamscanErrorFailsUnderRequiredProfile.
func TestClamscanErrorWarnsAndContinues(t *testing.T) {
	garbage := filepath.Join(t.TempDir(), "garbage")
	long := strings.Repeat("LibClamAV Error: ", 400)
	if err := os.WriteFile(garbage, []byte(hostileText+long+"\n\x1b]0;title\x07second line\n"+strings.Repeat("\xff\xfe", 4096)), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeClamscan(t, `cat '`+garbage+`' >&2; exit 2`)
	s, media := clamScanner(t, "archive")
	fr := scanOne(t, s, media)
	if fr.Verdict >= report.Block || !fr.Has(CodeClamError) {
		t.Fatalf("exit 2: verdict %s %v", fr.Verdict, codes(fr))
	}
	if !fr.Has(CodeUnparseable) {
		t.Fatal("the run did not carry on to the probe after the scanner error")
	}
	f := finding(fr, CodeClamError)
	if !strings.HasPrefix(f.Message, "clamscan exit 2: ") {
		t.Fatalf("message %q", f.Message)
	}
	if !cleanOf(f.Message) || !cleanOf(f.Detail) {
		t.Fatalf("raw control bytes in the finding: %q / %q", f.Message, f.Detail)
	}
	if len(f.Message) > 3*clamMessageBytes || strings.Contains(f.Message, "second line") {
		t.Fatalf("message not bounded to the first line: %d bytes %q", len(f.Message), f.Message)
	}
	if !strings.Contains(f.Message, `\x1b[31m`) {
		t.Fatalf("escape sequence not made visible: %q", f.Message)
	}
	if f.Detail != "no line of the clamscan output refers to this file" {
		t.Fatalf("detail %q", f.Detail)
	}
}

// Several megabytes of output naming the file are kept to a few lines in
// the finding, the runner's own bound is applied, and the verdict is
// unaffected. The bound on the finding is checked against the constants so
// a change to them is deliberate.
func TestClamscanOutputBounded(t *testing.T) {
	// 3 MiB of lines about the scanned file, then one about another path.
	fakeClamscan(t, `i=0; while [ $i -lt 40000 ]; do printf '%s: Win.Test.EICAR_HDB-1 FOUND padding padding padding padding padding padding\n' "$last"; i=$((i+1)); done; printf '/etc/passwd: Other FOUND\n'; exit 1`)
	s, media := clamScanner(t, "archive")
	s.Runner.MaxOutput = 1 << 20
	fr := scanOne(t, s, media)
	expect(t, fr, report.Block, CodeClamInfected)
	f := finding(fr, CodeClamInfected)
	lines := strings.Split(f.Detail, "\n")
	if len(f.Detail) > clamDetailBytes+clamLineBytes+200 || len(lines) > clamDetailLines+2 {
		t.Fatalf("detail not bounded: %d bytes, %d lines", len(f.Detail), len(lines))
	}
	if strings.Contains(f.Detail, "/etc/passwd") {
		t.Fatal("a line about another path reached the detail")
	}
	if !strings.Contains(f.Detail, "more line(s) about this file not shown") {
		t.Fatalf("the cut is not announced: %q", f.Detail)
	}
	if !strings.Contains(f.Detail, "clamscan output was longer than the runner keeps") {
		t.Fatalf("the runner's cut is not announced: %q", f.Detail)
	}
	for _, l := range lines[:clamDetailLines] {
		if !strings.HasPrefix(l, media+": Win.Test.EICAR_HDB-1 FOUND") {
			t.Fatalf("kept line %q", l)
		}
	}
	// One line far longer than the per-line bound is cut on a rune
	// boundary and marked.
	fakeClamscan(t, `printf '%s: ' "$last"; i=0; while [ $i -lt 200 ]; do printf 'éé'; i=$((i+1)); done; printf ' FOUND\n'; exit 1`)
	s, media = clamScanner(t, "archive")
	fr = scanOne(t, s, media)
	f = finding(fr, CodeClamInfected)
	if len(f.Detail) > clamLineBytes+8 || !strings.HasSuffix(f.Detail, "...") || !utf8.ValidString(f.Detail) {
		t.Fatalf("long line: %d bytes %q", len(f.Detail), f.Detail)
	}
	// The bound on the whole detail holds on the sanitised text: eight
	// lines that are each within the per-line bound but grow fourfold when
	// their control bytes are escaped do not reach the detail in full,
	// because a line that would cross the byte bound is left out and
	// counted as not shown rather than appended and only then noticed.
	fakeClamscan(t, `c=$(printf '\001'); pad=; i=0; while [ $i -lt 400 ]; do pad="$pad$c"; i=$((i+1)); done; i=0; while [ $i -lt 8 ]; do printf '%s: %s FOUND\n' "$last" "$pad"; i=$((i+1)); done; exit 1`)
	s, media = clamScanner(t, "archive")
	fr = scanOne(t, s, media)
	f = finding(fr, CodeClamInfected)
	lines = strings.Split(f.Detail, "\n")
	kept, shown := 0, 0
	for _, l := range lines {
		if strings.HasPrefix(l, media+": ") {
			kept += len(l)
			shown++
		}
	}
	if kept > clamDetailBytes || shown == 0 || shown+1 != len(lines) {
		t.Fatalf("detail not bounded on the sanitised text: %d bytes in %d line(s) about the file, %d lines in all", kept, shown, len(lines))
	}
	if !cleanOf(f.Detail) || !strings.Contains(f.Detail, `\x01`) {
		t.Fatalf("control bytes not escaped: %q", f.Detail)
	}
	if want := fmt.Sprintf("%d more line(s) about this file not shown", 8-shown); lines[len(lines)-1] != want {
		t.Fatalf("last line %q, want %q", lines[len(lines)-1], want)
	}
}

// Output crafted to read like a report is neutralised: ANSI escapes and
// bidi characters are escaped, an embedded line that pretends to be a
// verdict for another file or a summary ("Infected files: 0") is dropped
// because only lines about the scanned path are kept, and a newline
// inside the line about this file starts a new line that is dropped the
// same way.
func TestClamscanHostileOutputSanitised(t *testing.T) {
	payload := filepath.Join(t.TempDir(), "payload")
	if err := os.WriteFile(payload, []byte(hostileText), 0o644); err != nil {
		t.Fatal(err)
	}
	fakeClamscan(t, `printf 'Infected files: 0\n'; printf '%s: ' "$last"; cat '`+payload+`'; printf ' FOUND\n'; printf 'PASS  %s\n' "$last"; printf '%s.exe: Clean\n' "$last"; printf 'OK\n'; exit 1`)
	s, media := clamScanner(t, "archive")
	fr := scanOne(t, s, media)
	expect(t, fr, report.Block, CodeClamInfected)
	f := finding(fr, CodeClamInfected)
	if !cleanOf(f.Detail) {
		t.Fatalf("raw control or format bytes in the detail: %q", f.Detail)
	}
	for _, want := range []string{"\\x1b[31m", "\\u202e", "\\u200b", "\\x00", "\\x0d"} {
		if !strings.Contains(f.Detail, want) {
			t.Errorf("detail lacks the visible escape %s: %q", want, f.Detail)
		}
	}
	for _, bad := range []string{"Infected files", "PASS  ", ".exe:", "\nOK"} {
		if strings.Contains(f.Detail, bad) {
			t.Errorf("detail carries a line not about this file (%q): %q", bad, f.Detail)
		}
	}
	if strings.Count(f.Detail, "\n") != 0 {
		t.Fatalf("detail is more than the one line about this file: %q", f.Detail)
	}
	// The verdict rests on the exit status: the same hostile output with
	// exit 0 is a clean file, and the fake summary line cannot lower an
	// exit 1.
	fakeClamscan(t, `printf 'Infected files: 1\n%s: Eicar FOUND\n' "$last"; exit 0`)
	s, media = clamScanner(t, "archive")
	fr = scanOne(t, s, media)
	if fr.Has(CodeClamInfected) || fr.Has(CodeClamError) {
		t.Fatalf("exit 0 with a FOUND line raised a finding: %v", codes(fr))
	}
}

// Guarantee 4: a hung clamscan is killed at the run's timeout and reported
// as WARN CLAMAV_ERROR under an optional scan; the file is not blocked and
// the run carries on. The timeout is the scanner's, which the command line
// sets from --timeout, and defaults to DefaultClamTimeout. Under a required
// scan the kill is a FAIL instead, see
// TestClamscanErrorFailsUnderRequiredProfile.
func TestClamscanHangIsKilledAtTimeout(t *testing.T) {
	fakeClamscan(t, `sleep 5; printf '%s: Eicar FOUND\n' "$last"; exit 1`)
	s, media := clamScanner(t, "archive")
	s.Timeout = 300 * time.Millisecond
	s.Runner.WaitDelay = time.Second
	start := time.Now()
	fr := scanOne(t, s, media)
	if time.Since(start) > 4*time.Second {
		t.Fatalf("scan waited %v for a hung scanner", time.Since(start))
	}
	if fr.Verdict >= report.Block || !fr.Has(CodeClamError) {
		t.Fatalf("hung scanner: verdict %s %v", fr.Verdict, codes(fr))
	}
	f := finding(fr, CodeClamError)
	if !strings.Contains(f.Message, "timed out") || strings.Contains(f.Message, "FOUND") {
		t.Fatalf("message %q", f.Message)
	}
	if !fr.Has(CodeUnparseable) {
		t.Fatal("the run did not carry on after the scanner was killed")
	}
	if (&Scanner{}).clamTimeout() != DefaultClamTimeout || DefaultClamTimeout != 30*time.Minute {
		t.Fatalf("default timeout %v", (&Scanner{}).clamTimeout())
	}
	if (&Scanner{Timeout: time.Minute}).clamTimeout() != time.Minute {
		t.Fatal("the scanner's timeout is not the one used")
	}
	// A cancelled run is not a timeout and not a verdict either: the
	// killed scanner is an error, never an infected file.
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	s.Timeout = 10 * time.Second
	start = time.Now()
	res, _ := s.ScanPath(ctx, media)
	if time.Since(start) > 4*time.Second {
		t.Fatalf("scan waited %v after cancellation", time.Since(start))
	}
	for _, r := range res {
		if r.File.Has(CodeClamInfected) {
			t.Fatal("a killed scanner produced an infected verdict")
		}
	}
}

// A scanner that exits 1 but names some other path is still BLOCK: the
// exit status is the verdict and the tool is trusted for it, while the
// detail says that no line refers to this file rather than quoting the
// other path.
func TestClamscanInfectedOtherPath(t *testing.T) {
	fakeClamscan(t, `printf '/srv/other/file.mkv: Eicar FOUND\n'; exit 1`)
	s, media := clamScanner(t, "archive")
	fr := scanOne(t, s, media)
	expect(t, fr, report.Block, CodeClamInfected)
	f := finding(fr, CodeClamInfected)
	if f.Detail != "no line of the clamscan output refers to this file" || strings.Contains(f.Detail, "/srv/other") {
		t.Fatalf("detail %q", f.Detail)
	}
}

// A clean exit with text on stderr (a database warning, say) is a clean
// file: no CLAMAV finding, and none of the stderr reaches the report.
func TestClamscanExitZeroWithStderr(t *testing.T) {
	fakeClamscan(t, `printf 'LibClamAV Warning: the database is older than 7 days\n%s: OK\n' "$last" >&2; exit 0`)
	s, media := clamScanner(t, "archive")
	fr := scanOne(t, s, media)
	for _, f := range fr.Findings {
		if strings.HasPrefix(f.Code, "CLAMAV_") {
			t.Fatalf("clean exit raised %s: %q", f.Code, f.Message)
		}
		if strings.Contains(f.Message+f.Detail, "LibClamAV") {
			t.Fatalf("stderr of a clean exit reached the report: %q", f.Message+f.Detail)
		}
	}
	if !fr.Has(CodeUnparseable) {
		t.Fatal("the run did not carry on to the probe")
	}
}

// The arguments reach clamscan as separate words with the path after
// "--", so a path that begins with a dash is a file name, never an option.
// The limit options are part of every call: without --max-filesize raised
// to the most libclamav can scan, --max-scansize and --max-scantime
// switched off and --alert-exceeds-max, clamscan reports a file above its
// default 100 MB limit clean without reading it, which almost every media
// file is.
func TestClamscanArgumentsVerbatim(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv")
	fakeClamscan(t, `for a; do printf '%s\n' "$a"; done > '`+log+`'; exit 0`)
	s, _ := clamScanner(t, "archive")
	dir := t.TempDir()
	dash := write(t, filepath.Join(dir, "--remove=yes.mkv"), ebml, 0o644)
	scanOne(t, s, dash)
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	want := "--no-summary\n--infected\n--max-filesize=2047M\n--max-scansize=0\n--max-scantime=0\n--alert-exceeds-max\n--\n" + dash + "\n"
	if string(got) != want {
		t.Fatalf("argv:\n%s", got)
	}
	if ClamMaxFileSize != 2047<<20 || ClamMaxFileSize >= 1<<31 {
		t.Fatalf("ClamMaxFileSize %d is not the largest whole MiB below libclamav's 2 GiB cap", ClamMaxFileSize)
	}
}

// Guarantee 9: a file libclamav cannot scan is never passed by a scan that
// did not read it. A file larger than ClamMaxFileSize is CLAMAV_ERROR in
// plain words before clamscan starts, FAIL and the scan stops under a
// required profile, WARN and the file is probed under an optional one, and
// --clamav lowers none of it. A file exactly at the limit is handed to
// clamscan with the limit options. When clamscan itself refuses a file
// that grew past the limit after the stat, its --alert-exceeds-max output
// (an alert line with exit 2) is CLAMAV_ERROR the same way, so a scanner
// that exits 0 without reading the file is never reached.
func TestClamscanOversizedFileNotScanned(t *testing.T) {
	log := filepath.Join(t.TempDir(), "argv")
	fakeClamscan(t, `for a; do printf '%s\n' "$a"; done > '`+log+`'; exit 0`)
	sparse := func(t *testing.T, size int64) string {
		t.Helper()
		p := write(t, filepath.Join(t.TempDir(), "movie.mkv"), ebml, 0o644)
		if err := os.Truncate(p, size); err != nil {
			t.Skipf("cannot make a %d byte sparse file: %v", size, err)
		}
		if fi, err := os.Stat(p); err != nil || fi.Size() != size {
			t.Skipf("the filesystem did not keep a %d byte sparse file", size)
		}
		return p
	}
	big := sparse(t, ClamMaxFileSize+1)
	for _, flag := range []bool{false, true} {
		s := newScanner(t, mustProfile(t, "strict"), nil)
		s.ClamAV = flag
		fr := scanOne(t, s, big)
		if fr.Verdict != report.Fail || !fr.Has(CodeClamError) || fr.Has(CodeUnparseable) || fr.Has(CodeClamInfected) {
			t.Fatalf("strict clamav=%v: verdict %s %v", flag, fr.Verdict, codes(fr))
		}
		f := finding(fr, CodeClamError)
		if f.Severity != report.Fail || !strings.Contains(f.Message, "cannot scan files larger than 2047 MiB") || !strings.Contains(f.Message, "2 GiB") {
			t.Fatalf("strict: finding %s %q", f.Severity, f.Message)
		}
		if _, err := os.Lstat(log); err == nil {
			t.Fatal("clamscan was started for a file it cannot scan")
		}
	}
	s := newScanner(t, mustProfile(t, "archive"), nil)
	fr := scanOne(t, s, big)
	f := finding(fr, CodeClamError)
	if f.Severity != report.Warn || fr.Verdict >= report.Block || !fr.Has(CodeUnparseable) {
		t.Fatalf("archive: %s %v", fr.Verdict, codes(fr))
	}
	if _, err := os.Lstat(log); err == nil {
		t.Fatal("clamscan was started for a file it cannot scan")
	}
	// At the limit the file is scanned, with the limit options.
	edge := sparse(t, ClamMaxFileSize)
	fr = scanOne(t, s, edge)
	if fr.Has(CodeClamError) || fr.Has(CodeClamInfected) {
		t.Fatalf("a file at the limit raised %v", codes(fr))
	}
	got, err := os.ReadFile(log)
	if err != nil {
		t.Fatal("clamscan was not run for a file at the limit")
	}
	for _, opt := range []string{"--max-filesize=2047M\n", "--alert-exceeds-max\n", "--max-scansize=0\n", "--max-scantime=0\n"} {
		if !strings.Contains(string(got), opt) {
			t.Fatalf("argv lacks %q:\n%s", opt, got)
		}
	}
	// The scanner's own refusal of a file above the limit.
	fakeClamscan(t, `printf '%s: Heuristics.Limits.Exceeded.MaxFileSize FOUND\n' "$last"; exit 2`)
	s, media := clamScanner(t, "strict")
	fr = scanOne(t, s, media)
	if fr.Verdict != report.Fail || !fr.Has(CodeClamError) || fr.Has(CodeUnparseable) || fr.Has(CodeClamInfected) {
		t.Fatalf("strict, scanner alert: verdict %s %v", fr.Verdict, codes(fr))
	}
	f = finding(fr, CodeClamError)
	if f.Message != "clamscan exit 2: "+media+": Heuristics.Limits.Exceeded.MaxFileSize FOUND" || f.Detail != media+": Heuristics.Limits.Exceeded.MaxFileSize FOUND" {
		t.Fatalf("strict, scanner alert: %q / %q", f.Message, f.Detail)
	}
	s, media = clamScanner(t, "archive")
	fr = scanOne(t, s, media)
	if f := finding(fr, CodeClamError); f.Severity != report.Warn || !fr.Has(CodeUnparseable) {
		t.Fatalf("archive, scanner alert: %s %v", fr.Verdict, codes(fr))
	}
}

// The --clamav flag turns an "off" profile into an optional scan and never
// lowers a "required" one: with clamscan missing, strict is FAIL
// CLAMAV_MISSING with and without the flag, homelab and archive carry on
// without a CLAMAV finding, and with a scanner present every profile
// blocks an infected file once the flag is set.
func TestClamAVFlagForcesAndNeverDowngrades(t *testing.T) {
	noTools(t)
	media := write(t, filepath.Join(t.TempDir(), "movie.mkv"), ebml, 0o644)
	modes := map[string]string{}
	for _, name := range []string{"homelab", "anime", "archive", "strict"} {
		modes[name] = mustProfile(t, name).Safety.ClamAV
	}
	if modes["homelab"] != "off" || modes["archive"] != "optional" || modes["strict"] != "required" {
		t.Fatalf("profile modes changed: %v", modes)
	}
	for name, mode := range modes {
		for _, flag := range []bool{false, true} {
			s := newScanner(t, mustProfile(t, name), nil)
			s.ClamAV = flag
			fr := scanOne(t, s, media)
			if mode == "required" {
				if fr.Verdict != report.Fail || !fr.Has(CodeClamMissing) || fr.Has(CodeUnparseable) {
					t.Errorf("%s clamav=%v without clamscan: %s %v", name, flag, fr.Verdict, codes(fr))
				}
				continue
			}
			if fr.Has(CodeClamMissing) || fr.Has(CodeClamError) || !fr.Has(CodeUnparseable) {
				t.Errorf("%s clamav=%v without clamscan: %s %v", name, flag, fr.Verdict, codes(fr))
			}
		}
	}
	// With a scanner present, "off" is skipped without the flag and run
	// with it; the other modes always run it.
	fakeClamscan(t, `printf '%s: Eicar FOUND\n' "$last"; exit 1`)
	for name, mode := range modes {
		for _, flag := range []bool{false, true} {
			s := newScanner(t, mustProfile(t, name), nil)
			s.ClamAV = flag
			fr := scanOne(t, s, media)
			if mode == "off" && !flag {
				if fr.Has(CodeClamInfected) {
					t.Errorf("%s ran clamscan without the flag", name)
				}
				continue
			}
			expect(t, fr, report.Block, CodeClamInfected)
		}
	}
	// The scanner's Have lookup goes through the runner's override, so a
	// non-executable override counts as missing for a required profile.
	plain := filepath.Join(t.TempDir(), "clamscan")
	if err := os.WriteFile(plain, []byte("#!/bin/sh\nexit 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_CLAMSCAN", plain)
	s := newScanner(t, mustProfile(t, "strict"), &exec.Runner{Timeout: time.Minute})
	s.ClamAV = true
	fr := scanOne(t, s, media)
	if fr.Verdict != report.Fail || !fr.Has(CodeClamMissing) {
		t.Fatalf("strict with a non-executable clamscan: %s %v", fr.Verdict, codes(fr))
	}
}

// Guarantee 9, and guarantee 4 for the timeout: under a profile that
// requires the scan, a clamscan that produced no verdict is FAIL
// CLAMAV_ERROR and the scan of the file stops there, so the file is never
// probed, rebuilt or imported unscanned. That covers a scanner killed at a
// short --timeout, a scanner that exits 2 because no signature database
// is loaded, one that exits 2 with nothing to say, and one that cannot
// start. --clamav does not lower any of it. The same stubs under an
// optional profile stay WARN and the run carries on to the probe.
func TestClamscanErrorFailsUnderRequiredProfile(t *testing.T) {
	cases := []struct {
		name, body, want string
		timeout          time.Duration
	}{
		{"timeout", `exec sleep 5`, "timed out", 300 * time.Millisecond},
		{"no database", `printf 'LibClamAV Error: cli_loaddbdir: No supported database files found in /var/lib/clamav\n' >&2; exit 2`, "clamscan exit 2: LibClamAV Error: cli_loaddbdir", 10 * time.Second},
		{"silent error", `exit 2`, "clamscan exit 2 with no message", 10 * time.Second},
		{"cannot start", "", "clamscan: ", 10 * time.Second},
	}
	for _, tc := range cases {
		for _, flag := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s clamav=%v", tc.name, flag), func(t *testing.T) {
				if tc.body == "" {
					// An executable whose interpreter does not exist is a start
					// failure: there is no child and no exit status.
					noTools(t)
					p := filepath.Join(t.TempDir(), "clamscan")
					if err := os.WriteFile(p, []byte("#!"+filepath.Join(t.TempDir(), "no-such-shell")+"\n"), 0o755); err != nil {
						t.Fatal(err)
					}
					t.Setenv("AMUXIFY_CLAMSCAN", p)
				} else {
					fakeClamscan(t, tc.body)
				}
				s, media := clamScanner(t, "strict")
				s.ClamAV = flag
				s.Timeout = tc.timeout
				s.Runner.WaitDelay = time.Second
				fr := scanOne(t, s, media)
				if fr.Verdict != report.Fail || !fr.Has(CodeClamError) {
					t.Fatalf("strict: verdict %s %v", fr.Verdict, codes(fr))
				}
				if fr.Has(CodeUnparseable) || fr.Has(CodeClamInfected) || fr.Has(CodeClamMissing) {
					t.Fatalf("strict: the scan carried on past the scanner error: %v", codes(fr))
				}
				f := finding(fr, CodeClamError)
				if f.Severity != report.Fail || !strings.Contains(f.Message, tc.want) || !cleanOf(f.Message) {
					t.Fatalf("strict: finding %s %q", f.Severity, f.Message)
				}
				// The optional profile keeps the file usable and probes it.
				s, media = clamScanner(t, "archive")
				s.Timeout = tc.timeout
				s.Runner.WaitDelay = time.Second
				fr = scanOne(t, s, media)
				f = finding(fr, CodeClamError)
				if f.Severity != report.Warn || !fr.Has(CodeUnparseable) {
					t.Fatalf("archive: %s %v", fr.Verdict, codes(fr))
				}
			})
		}
	}
}

// The CLAMAV_ERROR message is a complete sentence whatever the scanner
// printed: the first line of standard error when there is one, otherwise
// the first line about the scanned file on standard output, which is
// where clamscan reports a file it could not open, otherwise a fixed
// sentence. A standard output line about another path or a summary line
// is never used, and the line that is used is bounded and sanitised.
func TestClamscanErrorMessageWithoutStderr(t *testing.T) {
	cases := []struct {
		name, body string
		want       func(media string) string
	}{
		{"file error on stdout", `printf '%s: Can'"'"'t open file ERROR\n' "$last"; exit 2`,
			func(m string) string { return "clamscan exit 2: " + m + ": Can't open file ERROR" }},
		{"nothing printed", `exit 2`,
			func(string) string { return "clamscan exit 2 with no message" }},
		{"only lines about other paths", `printf 'Infected files: 0\n/etc/passwd: Can'"'"'t open file ERROR\n\n\n' ; exit 3`,
			func(string) string { return "clamscan exit 3 with no message" }},
		{"stderr wins", `printf '%s: Can'"'"'t open file ERROR\n' "$last"; printf 'LibClamAV Error: database\n' >&2; exit 2`,
			func(string) string { return "clamscan exit 2: LibClamAV Error: database" }},
		{"hostile stdout line", `printf '\n\n%s: \033[31mERROR\033[0m \342\200\256evil\r\n' "$last"; exit 2`,
			func(m string) string { return "clamscan exit 2: " + m + ": \\x1b[31mERROR\\x1b[0m \\u202eevil" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeClamscan(t, tc.body)
			s, media := clamScanner(t, "archive")
			fr := scanOne(t, s, media)
			f := finding(fr, CodeClamError)
			if f.Message != tc.want(media) {
				t.Fatalf("message %q, want %q", f.Message, tc.want(media))
			}
			if strings.HasSuffix(f.Message, ": ") || !cleanOf(f.Message) {
				t.Fatalf("message %q", f.Message)
			}
		})
	}
	// A long stdout line about the file is cut to the message bound.
	fakeClamscan(t, `printf '%s: ' "$last"; i=0; while [ $i -lt 100 ]; do printf 'ERRORERROR'; i=$((i+1)); done; printf '\n'; exit 2`)
	s, media := clamScanner(t, "archive")
	f := finding(scanOne(t, s, media), CodeClamError)
	if len(f.Message) > clamMessageBytes+len("clamscan exit 2: ")+4 || !strings.HasSuffix(f.Message, "...") {
		t.Fatalf("message not bounded: %d bytes %q", len(f.Message), f.Message)
	}
}

// Guarantee 4: the memory clamDetail takes is bounded by the detail it
// returns, not by what the scanner printed. Sixteen MiB of bare newlines
// on each stream, which is the most the runner keeps and would cost a
// string header per line if the output were split first, is walked in
// place: the call allocates a few kilobytes and still finds the one line
// about the file at the very end. The same flood from a stub scanner
// completes and reports the empty detail.
func TestClamDetailMemoryBounded(t *testing.T) {
	const n = 16 << 20
	// The path is longer than the compiler's stack buffer for a small
	// string-to-bytes conversion, so a conversion done once per line would
	// show up in the measurement.
	path := "/srv/media/" + strings.Repeat("d/", 40) + "movie.mkv"
	stdout := append(bytes.Repeat([]byte{'\n'}, n), []byte(path+": Eicar FOUND")...)
	stderr := bytes.Repeat([]byte{'\n'}, n)
	res := &exec.Result{Stdout: stdout, Stderr: stderr, ExitCode: 1}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	detail := clamDetail(res, path)
	msg := clamErrorMessage(res, path)
	first := firstLine(stderr)
	runtime.ReadMemStats(&after)
	if detail != path+": Eicar FOUND" || msg != "clamscan exit 1: "+path+": Eicar FOUND" || first != "" {
		t.Fatalf("detail %q message %q first %q", detail, msg, first)
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("clamDetail allocated %d bytes for %d bytes of newline-only output", grew, 2*n)
	}
	// One line as long as the whole stream that starts with the path is
	// not copied in full before it is cut: only the per-line bound of it is.
	stdout = append([]byte(path+": "), bytes.Repeat([]byte{'A'}, n)...)
	res = &exec.Result{Stdout: stdout, ExitCode: 1}
	runtime.GC()
	runtime.ReadMemStats(&before)
	detail = clamDetail(res, path)
	runtime.ReadMemStats(&after)
	if !strings.HasPrefix(detail, path+": AAAA") || !strings.HasSuffix(detail, "...") || len(detail) > clamLineBytes+8 {
		t.Fatalf("long line detail: %d bytes %q", len(detail), detail[:min(len(detail), 80)])
	}
	if grew := after.TotalAlloc - before.TotalAlloc; grew > 1<<20 {
		t.Fatalf("clamDetail allocated %d bytes for one %d byte line", grew, n)
	}
	// The same flood from the scanner itself, through the runner. The
	// newlines come from head and tr rather than a shell variable: bash's
	// printf builtin writes a megabyte argument a byte at a time, which on
	// a slow macOS runner took longer than the scanner's timeout.
	fakeClamscan(t, `head -c 4194304 /dev/zero | tr '\0' '\n'; head -c 4194304 /dev/zero | tr '\0' '\n' >&2; exit 1`)
	s, media := clamScanner(t, "archive")
	s.Runner.MaxOutput = 2 << 20
	fr := scanOne(t, s, media)
	expect(t, fr, report.Block, CodeClamInfected)
	f := finding(fr, CodeClamInfected)
	if f.Detail != "no line of the clamscan output refers to this file\nclamscan output was longer than the runner keeps and was cut" {
		t.Fatalf("detail %q", f.Detail)
	}
}
