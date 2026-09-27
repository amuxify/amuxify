package clean

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/mp4"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
	"github.com/amuxify/amuxify/internal/verify"
	"github.com/pkg/xattr"
)

func mustProfile(t *testing.T, name string) *policy.Profile {
	t.Helper()
	p, err := policy.Load(name)
	if err != nil {
		t.Fatalf("load profile %s: %v", name, err)
	}
	return p
}

type trace struct {
	mu    sync.Mutex
	lines []string
}

func (tr *trace) add(s string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.lines = append(tr.lines, s)
}

func (tr *trace) all() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.lines...)
}

func newCleaner(t *testing.T, r *exec.Runner, p *policy.Profile) (*Cleaner, *trace) {
	t.Helper()
	tr := &trace{}
	if r == nil {
		r = &exec.Runner{Timeout: 2 * time.Minute}
	}
	r.Trace = tr.add
	return &Cleaner{
		Runner:   r,
		Prober:   &probe.Prober{Runner: r, Timeout: 2 * time.Minute},
		Verifier: &verify.Verifier{Runner: r, Timeout: 2 * time.Minute},
		Profile:  p,
		Timeout:  2 * time.Minute,
	}, tr
}

func noTools(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "no-such-tool")
	for _, tool := range []string{exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract, exec.ExifTool, exec.ClamScan} {
		t.Setenv("AMUXIFY_"+strings.ToUpper(tool), missing)
	}
}

func codes(fr report.FileResult) []string {
	var out []string
	for _, f := range fr.Findings {
		out = append(out, f.Code+"/"+f.Severity.String())
	}
	return out
}

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func write(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func leftovers(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".amuxify-") {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestXattrPrefixesPerGOOS(t *testing.T) {
	got := xattrPrefixes()
	switch runtime.GOOS {
	case "darwin":
		if len(got) != 1 || got[0] != "com.apple." {
			t.Fatalf("darwin prefixes %v", got)
		}
	case "linux", "freebsd":
		if len(got) != 1 || got[0] != "user." {
			t.Fatalf("%s prefixes %v", runtime.GOOS, got)
		}
	default:
		if got != nil {
			t.Fatalf("%s prefixes %v, want none", runtime.GOOS, got)
		}
	}
	// Whatever the platform, the security and system namespaces are never
	// candidates.
	for _, p := range got {
		for _, forbidden := range []string{"security.", "system.", "trusted.", "com.apple.system", "com.apple.acl"} {
			if strings.HasPrefix(forbidden, p) && !strings.HasPrefix(forbidden, p+"quarantine") {
				if p == "com.apple." && strings.HasPrefix(forbidden, "com.apple.") {
					// com.apple.system.* is protected by the kernel, not by
					// the prefix list; the removal test below covers it.
					continue
				}
				t.Errorf("prefix %q would match %q", p, forbidden)
			}
		}
	}
}

// Guarantee 7: only the listed xattr namespaces are removed.
func TestStripXattrsOnlyListedNamespaces(t *testing.T) {
	noTools(t)
	var keep, remove string
	switch runtime.GOOS {
	case "darwin":
		keep, remove = "local.amuxifytest", "com.apple.amuxifytest"
	case "linux", "freebsd":
		keep, remove = "trusted.amuxifytest", "user.amuxifytest"
	default:
		t.Skip("no xattr namespaces on this platform")
	}
	dir := t.TempDir()
	p := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")
	if err := xattr.LSet(p, remove, []byte("r")); err != nil {
		t.Skipf("filesystem refuses xattrs: %v", err)
	}
	keepSet := true
	if err := xattr.LSet(p, keep, []byte("k")); err != nil {
		// trusted.* needs CAP_SYS_ADMIN on Linux; local.* is always
		// allowed on macOS.
		keepSet = false
		t.Logf("cannot set %s here: %v", keep, err)
	}
	c, tr := newCleaner(t, nil, mustProfile(t, "homelab"))
	fr := c.CleanFile(context.Background(), p)
	if fr.Verdict != report.Pass || !fr.Has(CodeXattr) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	names, err := xattr.LList(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if n == remove {
			t.Errorf("%s not removed", remove)
		}
	}
	if keepSet {
		found := false
		for _, n := range names {
			if n == keep {
				found = true
			}
		}
		if !found {
			t.Errorf("%s removed although it is outside the listed namespaces", keep)
		}
	}
	if len(tr.all()) != 0 {
		t.Errorf("tools ran for a sidecar: %v", tr.all())
	}
	if b, _ := os.ReadFile(p); string(b) != "nfo\n" {
		t.Error("file content changed")
	}

	// Dry run removes nothing and says so.
	if err := xattr.LSet(p, remove, []byte("r")); err != nil {
		t.Fatal(err)
	}
	c.DryRun = true
	fr = c.CleanFile(context.Background(), p)
	if !fr.Has(CodeXattr) {
		t.Fatalf("dry run: %v", codes(fr))
	}
	for _, f := range fr.Findings {
		if f.Code == CodeXattr && !strings.Contains(f.Message, "would remove") {
			t.Errorf("dry run message %q", f.Message)
		}
	}
	if v, err := xattr.LGet(p, remove); err != nil || string(v) != "r" {
		t.Errorf("dry run removed %s", remove)
	}

	// A symlink to the file is never dereferenced: the target keeps its
	// attribute.
	link := filepath.Join(dir, "link.nfo")
	if err := os.Symlink(p, link); err == nil {
		c.DryRun = false
		fr = c.CleanFile(context.Background(), link)
		if fr.Verdict != report.Warn || !fr.Has(CodeSymlink) {
			t.Errorf("symlink: %v", codes(fr))
		}
		if v, err := xattr.LGet(p, remove); err != nil || string(v) != "r" {
			t.Errorf("cleaning a symlink stripped the target's attribute")
		}
	}
}

// Sidecar policy: blocked sidecars are removed only on explicit request.
func TestBlockedSidecarRemovedOnlyWithFlag(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	url := write(t, filepath.Join(dir, "a.url"), "[InternetShortcut]\nURL=http://x\n")
	up := write(t, filepath.Join(dir, "b.URL"), "[InternetShortcut]\nURL=http://x\n")
	exe := write(t, filepath.Join(dir, "movie.mkv.exe"), "MZ\x90\x00")
	nfo := write(t, filepath.Join(dir, "a.nfo"), "nfo\n")

	c, tr := newCleaner(t, nil, mustProfile(t, "homelab"))
	for _, p := range []string{url, up, exe} {
		fr := c.CleanFile(context.Background(), p)
		if fr.Verdict != report.Warn || !fr.Has(CodeSkipped) {
			t.Errorf("%s without flag: %s %v", filepath.Base(p), fr.Verdict, codes(fr))
		}
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s removed without the flag", filepath.Base(p))
		}
	}
	c.RemoveBlockedSidecars = true
	c.DryRun = true
	fr := c.CleanFile(context.Background(), url)
	if !fr.Has(CodeDryRun) || fr.Has(CodeSidecarRemove) {
		t.Errorf("dry run: %v", codes(fr))
	}
	if _, err := os.Lstat(url); err != nil {
		t.Error("dry run removed the file")
	}
	c.DryRun = false
	for _, p := range []string{url, up, exe} {
		fr = c.CleanFile(context.Background(), p)
		if fr.Verdict != report.Pass || !fr.Has(CodeSidecarRemove) {
			t.Errorf("%s with flag: %s %v", filepath.Base(p), fr.Verdict, codes(fr))
		}
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s still present", filepath.Base(p))
		}
	}
	// The flag never touches allowed sidecars, and the strict profile's
	// wider block list is honoured.
	fr = c.CleanFile(context.Background(), nfo)
	if fr.Has(CodeSidecarRemove) || fr.Verdict != report.Pass {
		t.Errorf("nfo: %v", codes(fr))
	}
	if _, err := os.Lstat(nfo); err != nil {
		t.Error("allowed sidecar removed")
	}
	strict, _ := newCleaner(t, nil, mustProfile(t, "strict"))
	fr = strict.CleanFile(context.Background(), nfo)
	if !fr.Has(CodeSkipped) || fr.Verdict != report.Warn {
		t.Errorf("strict nfo: %v", codes(fr))
	}
	if len(tr.all()) != 0 {
		t.Errorf("tools ran for sidecars: %v", tr.all())
	}

	// A blocked sidecar that is a symlink is reported as a symlink and its
	// target survives, even with the flag set.
	target := write(t, filepath.Join(t.TempDir(), "target.url"), "keep\n")
	link := filepath.Join(dir, "link.url")
	if err := os.Symlink(target, link); err == nil {
		fr = c.CleanFile(context.Background(), link)
		if !fr.Has(CodeSymlink) || fr.Has(CodeSidecarRemove) {
			t.Errorf("symlinked blocked sidecar: %v", codes(fr))
		}
		if _, err := os.Lstat(target); err != nil {
			t.Error("symlink target removed")
		}
	}
}

func TestMatroskaAlreadyClean(t *testing.T) {
	r := testutil.Need(t, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit)
	src := testutil.Copy(t, "conforming.mkv")
	before := fileSHA(t, src)
	c, tr := newCleaner(t, r, mustProfile(t, "homelab"))
	fr := c.CleanFile(context.Background(), src)
	if fr.Verdict != report.Pass || !fr.Has(CodeNothing) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	for _, l := range tr.all() {
		if strings.Contains(l, "mkvpropedit") {
			t.Fatalf("mkvpropedit ran on a clean file: %s", l)
		}
	}
	if fileSHA(t, src) != before {
		t.Fatal("file changed")
	}
}

func TestMatroskaStripsTitleAndTags(t *testing.T) {
	r := testutil.Need(t, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit)
	src := testutil.Copy(t, "multi.mkv")
	before := fileSHA(t, src)
	c, tr := newCleaner(t, r, mustProfile(t, "homelab"))

	c.DryRun = true
	fr := c.CleanFile(context.Background(), src)
	if !fr.Has(CodeDryRun) || fr.Has(CodeMetadata) {
		t.Fatalf("dry run: %v", codes(fr))
	}
	if fileSHA(t, src) != before {
		t.Fatal("dry run changed the file")
	}
	for _, l := range tr.all() {
		if strings.Contains(l, "mkvpropedit") {
			t.Fatalf("dry run ran mkvpropedit: %s", l)
		}
	}

	c.DryRun = false
	fr = c.CleanFile(context.Background(), src)
	if fr.Verdict != report.Pass || !fr.Has(CodeMetadata) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	if fileSHA(t, src) == before {
		t.Fatal("file unchanged")
	}
	pr := &probe.Prober{Runner: r, Timeout: time.Minute}
	info, err := pr.Probe(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != "" || info.GlobalTagN != 0 || info.TrackTagN != 0 || info.MuxingApp != "" || info.WritingApp != "" {
		t.Fatalf("still dirty: title=%q tags=%d/%d apps=%q/%q", info.Title, info.GlobalTagN, info.TrackTagN, info.MuxingApp, info.WritingApp)
	}
	// homelab keeps track names; strict removes them. Both are checked so
	// the profile switch is proven to reach mkvpropedit.
	kept := 0
	for _, s := range info.Streams {
		if s.Title != "" {
			kept++
		}
	}
	if kept == 0 {
		t.Fatalf("homelab removed track names although strip_track_titles is false")
	}
	strict, _ := newCleaner(t, r, mustProfile(t, "strict"))
	if fr := strict.CleanFile(context.Background(), src); !fr.Has(CodeMetadata) {
		t.Fatalf("strict: %v", codes(fr))
	}
	if info, err = pr.Probe(context.Background(), src); err != nil {
		t.Fatal(err)
	}
	for _, s := range info.Streams {
		if s.Title != "" {
			t.Errorf("strict left track name %q on stream #%d", s.Title, s.Index)
		}
	}
	// The stream count is untouched, so no track was dropped.
	if len(info.StreamsOf("video")) != 1 || len(info.StreamsOf("audio")) == 0 {
		t.Fatalf("streams changed: %d", len(info.Streams))
	}
	fr = c.CleanFile(context.Background(), src)
	if !fr.Has(CodeNothing) {
		t.Fatalf("second pass: %v", codes(fr))
	}
	if l := leftovers(t, filepath.Dir(src)); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}

func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ffmpegWrapper installs a shell script as AMUXIFY_FFMPEG that runs the
// real ffmpeg with the original arguments, then the after snippet, and exits
// with ffmpeg's status. The Runner strips the environment, so every path a
// snippet needs must be baked into it with shq. Tests use it to change the
// filesystem after ffmpeg has written the temp file and before the cleaner
// replaces the source with it.
func ffmpegWrapper(t *testing.T, r *exec.Runner, after string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper is a POSIX shell script")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	real, err := r.Path(exec.FFmpeg)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "ffmpeg")
	body := "#!/bin/sh\n" + shq(real) + " \"$@\"\nrc=$?\n" + after + "\nexit $rc\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_FFMPEG", script)
}

// Guarantee 3 and 1 for the MP4 rewrite: once ffmpeg has written the temp
// file, its name is swapped for a symlink to a victim that is itself a clean
// copy of the same media, so every read that followed the link would pass.
// The cleaner checks the temp name against the file it created as soon as
// ffmpeg returns and refuses the link there; should that check ever be
// lost, the replacement itself refuses the link as well rather than give
// the victim the source's mode and time and rename the link over the
// source. Either way the refusal must be reported: the rewrite is a failed
// clean, not a successful one. The victim is private with an old stamp and
// the source is world-writable with a different one, so a path-based
// identity copy would show on the victim. The source must keep its bytes,
// its mode, its time and its single name, and only the planted link may be
// removed.
func TestMp4RewriteRefusesSwappedTemp(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge)
	if runtime.GOOS == "windows" {
		t.Skip("mode bits and symlinks differ on windows")
	}
	// The victim is a copy of the fixture that has already been cleaned,
	// so that a probe, a stream hash and an atom parse through the link
	// all look like this run's own output.
	victim := testutil.Copy(t, "purchased.mp4")
	prep, _ := newCleaner(t, r, mustProfile(t, "homelab"))
	if pr := prep.CleanFile(context.Background(), victim); !pr.Has(CodeMetadata) {
		t.Fatalf("the victim could not be prepared: %v", codes(pr))
	}
	victimBefore := fileSHA(t, victim)
	if err := os.Chmod(victim, 0o600); err != nil {
		t.Fatal(err)
	}
	victimStamp := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if err := os.Chtimes(victim, victimStamp, victimStamp); err != nil {
		t.Fatal(err)
	}
	src := testutil.Copy(t, "purchased.mp4")
	dir := filepath.Dir(src)
	srcBefore := fileSHA(t, src)
	if err := os.Chmod(src, 0o666); err != nil {
		t.Fatal(err)
	}
	srcStamp := time.Date(2010, 11, 12, 13, 14, 15, 0, time.UTC)
	if err := os.Chtimes(src, srcStamp, srcStamp); err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(dir, ".amuxify-purchased.mp4.tmp")
	// The swap runs after every ffmpeg call, but only turns a regular file
	// at the temp name into the link once; the stream hash calls that
	// follow see the link and leave it.
	ffmpegWrapper(t, r, "if [ -f "+shq(tmp)+" ] && [ ! -L "+shq(tmp)+" ]; then rm -f "+shq(tmp)+" && ln -s "+shq(victim)+" "+shq(tmp)+"; fi")

	c, tr := newCleaner(t, nil, mustProfile(t, "homelab"))
	fr := c.CleanFile(context.Background(), src)
	wroteTmp := false
	for _, l := range tr.all() {
		if strings.Contains(l, "ffmpeg") && strings.Contains(l, "-y ") && strings.HasSuffix(l, tmp) {
			wroteTmp = true
		}
	}
	if !wroteTmp {
		t.Fatalf("ffmpeg never wrote the temp name: %v", tr.all())
	}
	if fr.Verdict != report.Fail || !fr.Has(CodeCleanFail) || fr.Has(CodeMetadata) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	said := false
	for _, f := range fr.Findings {
		if f.Code == CodeCleanFail && strings.Contains(f.Message, "symlink") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the finding does not name the symlink: %v", fr.Findings)
	}
	fi, err := os.Lstat(victim)
	if err != nil || !fi.Mode().IsRegular() || fsutil.Nlink(fi) != 1 {
		t.Fatalf("victim is no longer a plain regular file with one name: %v", err)
	}
	if fi.Mode().Perm() != 0o600 || !fi.ModTime().Equal(victimStamp) {
		t.Fatalf("victim identity rewritten through the planted link: mode %o mtime %v", fi.Mode().Perm(), fi.ModTime())
	}
	if fileSHA(t, victim) != victimBefore {
		t.Fatal("victim rewritten")
	}
	fi, err = os.Lstat(src)
	if err != nil || !fi.Mode().IsRegular() || fsutil.Nlink(fi) != 1 {
		t.Fatalf("source is no longer a plain regular file: %v", err)
	}
	if fi.Mode().Perm() != 0o666 || !fi.ModTime().Equal(srcStamp) {
		t.Fatalf("source identity changed: mode %o mtime %v", fi.Mode().Perm(), fi.ModTime())
	}
	if fileSHA(t, src) != srcBefore {
		t.Fatal("source changed")
	}
	if _, err := os.Lstat(tmp); err == nil {
		t.Fatal("the planted link is still there")
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}

// Guarantees 2, 5 and 6: the MP4 rewrite goes through a temp file, keeps the
// stream bytes and the file identity, and removes the purchase atoms.
func TestMp4RewriteRemovesProvenance(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge)
	src := testutil.Copy(t, "purchased.mp4")
	dir := filepath.Dir(src)
	v := &verify.Verifier{Runner: r, Timeout: time.Minute}
	h0, err := v.StreamHash(context.Background(), src, 0)
	if err != nil {
		t.Fatal(err)
	}
	h1, err := v.StreamHash(context.Background(), src, 1)
	if err != nil {
		t.Fatal(err)
	}
	stamp := time.Date(2021, 7, 8, 9, 10, 11, 0, time.UTC)
	if runtime.GOOS != "windows" {
		if err := os.Chmod(src, 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(src, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	in, err := mp4.Parse(src)
	if err != nil || len(in.ProvenanceKeys()) == 0 {
		t.Fatalf("fixture has no provenance keys: %v", err)
	}

	c, tr := newCleaner(t, r, mustProfile(t, "homelab"))
	fr := c.CleanFile(context.Background(), src)
	if fr.Verdict != report.Pass || !fr.Has(CodeMetadata) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	out, err := mp4.Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	if keys := out.ProvenanceKeys(); len(keys) != 0 || out.HasXMP {
		t.Fatalf("provenance survived: %v xmp=%v", keys, out.HasXMP)
	}
	if g0, _ := v.StreamHash(context.Background(), src, 0); g0 != h0 {
		t.Errorf("video stream changed")
	}
	if g1, _ := v.StreamHash(context.Background(), src, 1); g1 != h1 {
		t.Errorf("audio stream changed")
	}
	fi, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %o want 640", fi.Mode().Perm())
	}
	if !fi.ModTime().Truncate(time.Second).Equal(stamp) {
		t.Errorf("mtime %v want %v", fi.ModTime(), stamp)
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temp files left: %v", l)
	}
	// ffmpeg wrote the hidden temp name, never the live path.
	wroteTmp := false
	for _, l := range tr.all() {
		if strings.Contains(l, "ffmpeg") && strings.Contains(l, "-y ") {
			if strings.HasSuffix(l, " "+src) {
				t.Errorf("ffmpeg wrote the live path: %s", l)
			}
			if strings.HasSuffix(l, filepath.Join(dir, ".amuxify-purchased.mp4.tmp")) {
				wroteTmp = true
			}
		}
	}
	if !wroteTmp {
		t.Errorf("no ffmpeg run targeting the temp name in %v", tr.all())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries after cleaning", len(entries))
	}
	// The rewrite is idempotent: ffprobe always reports the ftyp brand
	// fields and the muxer's default handler names as tags, and those must
	// not count as metadata, so a second pass finds nothing to clean and
	// does not touch the file.
	sum := fileSHA(t, src)
	fi, _ = os.Lstat(src)
	ffmpegRuns := len(tr.all())
	fr = c.CleanFile(context.Background(), src)
	if fr.Verdict != report.Pass || !fr.Has(CodeNothing) || fr.Has(CodeMetadata) {
		t.Fatalf("second pass: %s %v", fr.Verdict, codes(fr))
	}
	if fileSHA(t, src) != sum {
		t.Error("second pass rewrote the file")
	}
	for _, l := range tr.all()[ffmpegRuns:] {
		if strings.Contains(l, "ffmpeg") && strings.Contains(l, "-y ") {
			t.Errorf("second pass ran a rewrite: %s", l)
		}
	}
	if out, err := mp4.Parse(src); err != nil || len(out.ProvenanceKeys()) != 0 {
		t.Fatalf("second pass: %v %v", err, out)
	}
	if fi2, _ := os.Lstat(src); !fi2.ModTime().Equal(fi.ModTime()) {
		t.Error("second pass changed the mtime")
	}
}

// A handler name of someone's choosing is not one of the muxer defaults and
// still counts as metadata: a file carrying one is rewritten, and the
// rewrite leaves the default name behind.
func TestMp4CustomHandlerNameIsCleaned(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge)
	src := testutil.Copy(t, "sample.m4v")
	c, _ := newCleaner(t, r, mustProfile(t, "homelab"))
	fr := c.CleanFile(context.Background(), src)
	if fr.Verdict != report.Pass {
		t.Fatalf("first pass: %s %v", fr.Verdict, codes(fr))
	}
	// Plant a hostile handler name with ffmpeg itself, keeping everything
	// else as the clean pass left it.
	tagged := filepath.Join(filepath.Dir(src), "tagged.m4v")
	res, err := r.RunWithTimeout(context.Background(), time.Minute, exec.FFmpeg,
		"-v", "error", "-i", src, "-map", "0", "-c", "copy", "-fflags", "+bitexact", "-flags", "+bitexact",
		"-metadata:s:v:0", "handler_name=https://evil.example/track", "-f", "mp4", "-y", tagged)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("ffmpeg: %v %s", err, res.Stderr)
	}
	fr = c.CleanFile(context.Background(), tagged)
	if fr.Verdict != report.Pass || !fr.Has(CodeMetadata) {
		t.Fatalf("tagged: %s %v", fr.Verdict, codes(fr))
	}
	p := &probe.Prober{Runner: r, Timeout: time.Minute}
	info, err := p.Probe(context.Background(), tagged)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range info.Streams {
		if strings.Contains(st.Tags["handler_name"], "evil") {
			t.Errorf("stream #%d still carries the planted handler name", st.Index)
		}
	}
	fr = c.CleanFile(context.Background(), tagged)
	if !fr.Has(CodeNothing) {
		t.Errorf("third pass not idempotent: %v", codes(fr))
	}
}

// A rewrite that fails leaves the original untouched and no temp file.
func TestRewriteFailureKeepsOriginal(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge)
	src := testutil.Copy(t, "purchased.mp4")
	before := fileSHA(t, src)
	c, _ := newCleaner(t, r, mustProfile(t, "homelab"))
	c.Timeout = time.Nanosecond
	fr := c.CleanFile(context.Background(), src)
	if fr.Verdict != report.Fail || !fr.Has(CodeCleanFail) || fr.Has(CodeMetadata) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	if fileSHA(t, src) != before {
		t.Fatal("original changed after a failed rewrite")
	}
	if l := leftovers(t, filepath.Dir(src)); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}

// Files that are not what their name says never reach a writer.
func TestHostileMediaNeverRewritten(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit)
	dir := t.TempDir()
	bodies := map[string]string{
		"pe.mp4":      "MZ\x90\x00\x03\x00\x00\x00 not a movie",
		"text.mkv":    "just text\n",
		"html.mp4":    "<!doctype html><script>x</script>",
		"empty.mkv":   "",
		"garbage.avi": "RIFF\x00\x00\x00\x00AVI LIST",
	}
	c, tr := newCleaner(t, r, mustProfile(t, "homelab"))
	for n, body := range bodies {
		p := write(t, filepath.Join(dir, n), body)
		fr := c.CleanFile(context.Background(), p)
		if fr.Has(CodeMetadata) || fr.Verdict < report.Fail {
			t.Errorf("%s: %s %v", n, fr.Verdict, codes(fr))
		}
		if b, _ := os.ReadFile(p); string(b) != body {
			t.Errorf("%s: content changed", n)
		}
	}
	for _, l := range tr.all() {
		if strings.Contains(l, "mkvpropedit") || (strings.Contains(l, "ffmpeg") && strings.Contains(l, "-y ")) {
			t.Errorf("writer ran on hostile input: %s", l)
		}
	}
	if l := leftovers(t, dir); len(l) != 0 {
		t.Errorf("temp files left: %v", l)
	}
	// A truncated MP4 fails closed as well.
	tr.lines = nil
	src := testutil.Copy(t, "truncated.mp4")
	before := fileSHA(t, src)
	fr := c.CleanFile(context.Background(), src)
	if fr.Verdict < report.Fail || fr.Has(CodeMetadata) {
		t.Errorf("truncated: %s %v", fr.Verdict, codes(fr))
	}
	if fileSHA(t, src) != before {
		t.Error("truncated file changed")
	}
}

func TestHardlinkedMediaSkipped(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.mkv"), "\x1a\x45\xdf\xa3")
	if err := os.Link(a, filepath.Join(dir, "b.mkv")); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	c, tr := newCleaner(t, nil, mustProfile(t, "homelab"))
	fr := c.CleanFile(context.Background(), a)
	if fr.Verdict != report.Warn || !fr.Has(CodeHardlinked) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	if len(tr.all()) != 0 {
		t.Fatalf("tools ran: %v", tr.all())
	}
	// Break and copy modes go on to probe, which fails here because no
	// tool is installed; the file itself is still untouched.
	for _, mode := range []string{"break", "copy"} {
		c.Hardlinks = mode
		fr = c.CleanFile(context.Background(), a)
		if fr.Has(CodeHardlinked) || !fr.Has(CodeCleanFail) {
			t.Errorf("%s: %v", mode, codes(fr))
		}
	}
	if b, _ := os.ReadFile(a); string(b) != "\x1a\x45\xdf\xa3" {
		t.Fatal("file changed")
	}
}

func TestCleanPathWalk(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	write(t, filepath.Join(dir, ".DS_Store"), "x")
	write(t, filepath.Join(dir, ".amuxify-x.tmp"), "x")
	write(t, filepath.Join(dir, "sub", "a.nfo"), "nfo\n")
	write(t, filepath.Join(dir, "b.url"), "x\n")
	if err := os.Symlink(t.TempDir(), filepath.Join(dir, "dirlink")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	c, _ := newCleaner(t, nil, mustProfile(t, "homelab"))
	c.RemoveBlockedSidecars = true
	var order []string
	c.Progress = func(fr report.FileResult) { order = append(order, filepath.Base(fr.Path)) }
	res, err := c.CleanPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(order, ",") != "b.url,dirlink,a.nfo" {
		t.Fatalf("order %v", order)
	}
	if len(res) != 3 {
		t.Fatalf("%d results", len(res))
	}
	if _, err := os.Lstat(filepath.Join(dir, ".DS_Store")); err != nil {
		t.Error(".DS_Store touched")
	}
	if _, err := c.CleanPath(context.Background(), filepath.Join(dir, "missing")); err == nil {
		t.Error("missing root accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.CleanPath(ctx, dir); err == nil {
		t.Error("cancelled context ignored")
	}
}

// An unreadable directory inside the tree is reported as an error after the
// readable files are cleaned, never dropped (review C2).
func TestCleanReportsUnreadableDirectory(t *testing.T) {
	noTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	ok := write(t, filepath.Join(root, "a", "ok.nfo"), "nfo\n")
	locked := filepath.Join(root, "locked")
	write(t, filepath.Join(locked, "b.nfo"), "nfo\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	c, _ := newCleaner(t, nil, mustProfile(t, "homelab"))
	res, err := c.CleanPath(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "cannot read "+locked) {
		t.Fatalf("err %v, want one naming %s", err, locked)
	}
	if len(res) != 1 || res[0].Path != ok {
		t.Fatalf("results %v", res)
	}
}

// Review C10: the audio-tags advice names --strip-audio-tags only for the
// clean command, which has that flag; under ingest it names no flag.
func TestAudioTagsAdviceFollowsTheCommand(t *testing.T) {
	noTools(t)
	info := &probe.MediaInfo{Container: "mp3", Streams: []probe.Stream{{Index: 0, Type: "audio", Codec: "mp3"}}}
	for _, tc := range []struct{ command, want string }{
		{"", "audio tags left alone (use --strip-audio-tags)"},
		{"clean", "audio tags left alone (use --strip-audio-tags)"},
		{"ingest", "audio tags left alone; ingest does not strip audio tags"},
	} {
		c, tr := newCleaner(t, nil, mustProfile(t, "homelab"))
		c.Command = tc.command
		path := write(t, filepath.Join(t.TempDir(), "a.mp3"), "ID3\x03\x00\x00\x00\x00\x00\x00")
		sum := fileSHA(t, path)
		fr := report.FileResult{Path: path, Info: map[string]string{}}
		c.cleanMedia(context.Background(), &fr, path, "mp3", info)
		var got string
		for _, f := range fr.Findings {
			if f.Code == CodeSkipped {
				got = f.Message
			}
		}
		if got != tc.want {
			t.Errorf("command %q: SKIPPED %q, want %q", tc.command, got, tc.want)
		}
		if fr.Verdict != report.Pass || fileSHA(t, path) != sum || len(tr.all()) != 0 {
			t.Errorf("command %q: %s, file changed %v, tools %v", tc.command, fr.Verdict, fileSHA(t, path) != sum, tr.all())
		}
	}
}

// Review C17, guarantee 7: the attribute-name predicate is tested on every
// platform with hostile names, including the namespaces the filesystem test
// above cannot set without privileges.
func TestInNamespaceHostileNames(t *testing.T) {
	user := []string{"user."}
	apple := []string{"com.apple."}
	both := []string{"user.", "com.apple."}
	cases := []struct {
		name     string
		prefixes []string
		want     bool
	}{
		{"", both, false},
		{"user", user, false},
		{"user.", user, false},
		{"user.x", user, true},
		{"user.x", apple, false},
		{"USER.x", user, false},
		{"User.x", user, false},
		{" user.x", user, false},
		{"trusted.user.x", user, false},
		{"trusted.x", both, false},
		{"security.selinux", both, false},
		{"security.capability", both, false},
		{"system.posix_acl_access", both, false},
		{"com.apple.quarantine", apple, true},
		{"com.apple.quarantine", user, false},
		{"com.apple.", apple, false},
		{"com.apple", apple, false},
		{"com.applex.y", apple, false},
		{"COM.APPLE.quarantine", apple, false},
		{"local.com.apple.x", apple, false},
		{"user\x00.x", user, false},
		{"\x00user.x", user, false},
		{"\nuser.x", user, false},
		{"user.x\n", user, true},
		{"user.\x00", user, true},
		{"user.x", nil, false},
		{"user.x", []string{""}, false},
		{"", []string{""}, false},
	}
	for _, tc := range cases {
		if got := inNamespace(tc.name, tc.prefixes); got != tc.want {
			t.Errorf("inNamespace(%q, %q) = %v, want %v", tc.name, tc.prefixes, got, tc.want)
		}
	}
	// The live prefixes: security labels and the trusted namespace are
	// outside on every platform, and nothing matches where amuxify strips
	// no attributes at all.
	for _, n := range []string{"security.selinux", "trusted.amuxifytest", "system.nfs4_acl", "", "user", "com.apple"} {
		if inNamespace(n, xattrPrefixes()) {
			t.Errorf("%q counted as removable on %s", n, runtime.GOOS)
		}
	}
	switch runtime.GOOS {
	case "darwin":
		if !inNamespace("com.apple.quarantine", xattrPrefixes()) || inNamespace("user.x", xattrPrefixes()) {
			t.Error("darwin prefixes wrong")
		}
	case "linux", "freebsd":
		if !inNamespace("user.x", xattrPrefixes()) || inNamespace("com.apple.quarantine", xattrPrefixes()) {
			t.Error("linux prefixes wrong")
		}
	default:
		if xattrPrefixes() != nil {
			t.Errorf("unexpected prefixes on %s", runtime.GOOS)
		}
	}
}
