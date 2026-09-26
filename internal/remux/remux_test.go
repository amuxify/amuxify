package remux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/testutil"
	"github.com/amuxify/amuxify/internal/verify"
)

func mustProfile(t *testing.T, name string) *policy.Profile {
	t.Helper()
	p, err := policy.Load(name)
	if err != nil {
		t.Fatalf("load profile %s: %v", name, err)
	}
	return p
}

// trace collects every command line the runner starts.
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

// writes reports the command lines that would create a file: mkvmerge -o
// and mkvpropedit runs.
func (tr *trace) writes() []string {
	var out []string
	for _, l := range tr.all() {
		if strings.Contains(l, " -o ") || strings.Contains(l, "mkvpropedit") {
			out = append(out, l)
		}
	}
	return out
}

func newRemuxer(t *testing.T, r *exec.Runner, p *policy.Profile) (*Remuxer, *trace) {
	t.Helper()
	tr := &trace{}
	if r == nil {
		r = &exec.Runner{Timeout: 2 * time.Minute}
	}
	r.Trace = tr.add
	pr := &probe.Prober{Runner: r, Timeout: 2 * time.Minute}
	v := &verify.Verifier{Runner: r, Timeout: 2 * time.Minute}
	sc := &scan.Scanner{Runner: r, Prober: pr, Verifier: v, Profile: p}
	return &Remuxer{Runner: r, Prober: pr, Verifier: v, Scanner: sc, Profile: p, Timeout: 2 * time.Minute}, tr
}

// noTools points every tool override at a missing path.
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

func leftovers(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err == nil && strings.HasPrefix(d.Name(), ".amuxify-") {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

// mediaResult builds a scan result for a fake video file so remuxScanned can
// be exercised without any tool.
func mediaResult(path string, verdict report.Severity) scan.Result {
	fr := report.FileResult{Path: path, Info: map[string]string{}}
	switch verdict {
	case report.Block:
		fr.Addf(scan.CodePolyglot, report.Block, "test block")
	case report.Fail:
		fr.Addf(scan.CodeDecodeFail, report.Fail, "test fail")
	case report.Warn:
		fr.Addf(scan.CodeLinkInTag, report.Warn, "test warn")
	}
	return scan.Result{
		File: fr,
		Info: &probe.MediaInfo{
			Path: path, Container: "matroska", MkvSupported: true, MkvIdentified: true,
			Streams: []probe.Stream{
				{Index: 0, MkvID: 0, Type: "video", Codec: "h264", Language: "und"},
				{Index: 1, MkvID: 1, Type: "audio", Codec: "aac", Language: "eng", Default: true},
			},
		},
	}
}

// Guarantee 7: in-place needs verification.
func TestInPlaceVerifyNoneRefused(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		tier    string
		profile string
	}{
		{"flag", "none", ""},
		{"profile", "", "[verify]\ntier=\"none\"\n"},
		{"flag over profile", "none", "[verify]\ntier=\"full\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mustProfile(t, "homelab")
			if tc.profile != "" {
				var err error
				if p, err = policy.Load(testutil.Profile(t, tc.profile)); err != nil {
					t.Fatal(err)
				}
			}
			rm, tr := newRemuxer(t, nil, p)
			rm.Scanner = nil // any use of the scanner would panic
			rm.InPlace = true
			rm.VerifyTier = tc.tier
			rm.Force = true
			before := fileSHA(t, src)
			_, err := rm.RemuxPath(context.Background(), dir)
			if err == nil || !strings.Contains(err.Error(), "refusing --in-place together with verify tier none") {
				t.Fatalf("err = %v", err)
			}
			if got := tr.all(); len(got) != 0 {
				t.Fatalf("tools ran: %v", got)
			}
			if fileSHA(t, src) != before {
				t.Fatal("source changed")
			}
		})
	}
	// The flag being lifted from none to quick makes the same call legal
	// as far as this check goes: it now reaches the scanner.
	rm, _ := newRemuxer(t, nil, mustProfile(t, "homelab"))
	rm.InPlace = true
	rm.VerifyTier = "quick"
	if _, err := rm.RemuxPath(context.Background(), dir); err != nil {
		t.Fatalf("quick tier refused: %v", err)
	}
}

// Guarantee 4: BLOCK is never overridden, not even by --force.
func TestBlockRefusedEvenWithForce(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outRoot := filepath.Join(t.TempDir(), "out")
	for _, inPlace := range []bool{false, true} {
		for _, force := range []bool{false, true} {
			rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
			rm.Force = force
			rm.InPlace = inPlace
			rm.VerifyTier = "quick"
			fr := rm.RemuxScanned(context.Background(), mediaResult(src, report.Block), dir, outRoot)
			if fr.Verdict != report.Block || !fr.Has(CodeRefused) {
				t.Errorf("force=%v inPlace=%v: %s %v", force, inPlace, fr.Verdict, codes(fr))
			}
			if got := tr.all(); len(got) != 0 {
				t.Errorf("force=%v inPlace=%v: tools ran: %v", force, inPlace, got)
			}
			if fr.Output != "" {
				t.Errorf("output path set on a refused file: %s", fr.Output)
			}
		}
	}
	if _, err := os.Lstat(outRoot); err == nil {
		t.Fatal("output root created for a refused file")
	}
	if b, _ := os.ReadFile(src); string(b) != "x" {
		t.Fatal("source changed")
	}
	// A BLOCK result that also lacks probe info takes the SKIPPED path but
	// still ends BLOCK.
	rm, _ := newRemuxer(t, nil, mustProfile(t, "homelab"))
	rm.Force = true
	sc := mediaResult(src, report.Block)
	sc.Info = nil
	if fr := rm.RemuxScanned(context.Background(), sc, dir, outRoot); fr.Verdict != report.Block {
		t.Fatalf("verdict lowered to %s", fr.Verdict)
	}
}

func TestFailRefusedWithoutForce(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(src, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	outRoot := filepath.Join(t.TempDir(), "out")
	rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
	fr := rm.RemuxScanned(context.Background(), mediaResult(src, report.Fail), dir, outRoot)
	if fr.Verdict != report.Fail || !fr.Has(CodeRefused) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	if got := tr.all(); len(got) != 0 {
		t.Fatalf("tools ran: %v", got)
	}
	// With --force the file proceeds as far as the plan; DryRun stops it
	// before any tool.
	rm.Force = true
	rm.DryRun = true
	fr = rm.RemuxScanned(context.Background(), mediaResult(src, report.Fail), dir, outRoot)
	if fr.Has(CodeRefused) || !fr.Has(CodeDryRun) {
		t.Fatalf("forced: %v", codes(fr))
	}
	if fr.Verdict != report.Fail {
		t.Fatalf("--force lowered the scan verdict to %s", fr.Verdict)
	}
	if got := tr.all(); len(got) != 0 {
		t.Fatalf("dry run started tools: %v", got)
	}
}

// Guarantee 1: an existing destination is refused before any tool runs.
func TestOutputExistsBeforeAnyTool(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	src := filepath.Join(dir, "sub", "a.mp4")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("source"), 0o644); err != nil {
		t.Fatal(err)
	}
	outRoot := filepath.Join(t.TempDir(), "out")
	dest := filepath.Join(outRoot, "sub", "a.mkv")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dest, []byte("precious"), 0o644); err != nil {
		t.Fatal(err)
	}
	check := func(t *testing.T, rm *Remuxer, tr *trace, sc scan.Result, root string) {
		t.Helper()
		fr := rm.RemuxScanned(context.Background(), sc, dir, root)
		if fr.Verdict != report.Fail || !fr.Has(CodeOutputExists) {
			t.Errorf("%s %v", fr.Verdict, codes(fr))
		}
		if got := tr.all(); len(got) != 0 {
			t.Errorf("tools ran: %v", got)
		}
	}
	t.Run("file", func(t *testing.T) {
		rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
		check(t, rm, tr, mediaResult(src, report.Pass), outRoot)
		if b, _ := os.ReadFile(dest); string(b) != "precious" {
			t.Fatal("destination overwritten")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		if err := os.Remove(dest); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(filepath.Join(t.TempDir(), "elsewhere"), dest); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
		check(t, rm, tr, mediaResult(src, report.Pass), outRoot)
		if fi, err := os.Lstat(dest); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatal("dangling symlink at the destination was replaced")
		}
	})
	t.Run("directory", func(t *testing.T) {
		if err := os.Remove(dest); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
		check(t, rm, tr, mediaResult(src, report.Pass), outRoot)
	})
	t.Run("in place sibling", func(t *testing.T) {
		sibling := filepath.Join(dir, "sub", "a.mkv")
		if err := os.WriteFile(sibling, []byte("sibling"), 0o644); err != nil {
			t.Fatal(err)
		}
		rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
		rm.InPlace = true
		rm.VerifyTier = "quick"
		check(t, rm, tr, mediaResult(src, report.Pass), outRoot)
		if b, _ := os.ReadFile(sibling); string(b) != "sibling" {
			t.Fatal("sibling overwritten")
		}
		if b, _ := os.ReadFile(src); string(b) != "source" {
			t.Fatal("source changed")
		}
	})
}

func TestMkvmergeArgsGolden(t *testing.T) {
	rm, _ := newRemuxer(t, nil, mustProfile(t, "homelab"))
	in := filepath.Join(t.TempDir(), "a b;$(id) `x`.mkv")
	m := &probe.MediaInfo{Path: in, Container: "matroska"}
	streams := []probe.Stream{
		{Index: 0, MkvID: 0, Type: "video"},
		{Index: 1, MkvID: 1, Type: "audio", Language: "jpn"},
		{Index: 2, MkvID: 2, Type: "audio", Language: "eng", Title: "Commentary"},
		{Index: 3, MkvID: 3, Type: "audio", Language: "fre"},
		{Index: 4, MkvID: 4, Type: "subtitle", Language: "und"},
		{Index: 5, MkvID: 5, Type: "subtitle", Language: "ger"},
		{Index: 6, MkvID: -1, Type: "data"},
	}
	m.Streams = streams
	d := &policy.Decision{
		StripTitle: true, StripTags: true, StripProvenance: true, KeepChapters: false,
		Tracks: []policy.TrackAction{
			{Stream: streams[0], Keep: true},
			{Stream: streams[1], Keep: true, Default: true},
			{Stream: streams[2], Keep: true, ClearTitle: true},
			{Stream: streams[3], Keep: false},
			{Stream: streams[4], Keep: true, Forced: true, SetLanguage: "eng"},
			{Stream: streams[5], Keep: false},
			{Stream: streams[6], Keep: false},
		},
		Attachments: []policy.AttachmentAction{
			{Attachment: probe.Attachment{ID: 1, FileName: "font.ttf"}, Keep: true},
			{Attachment: probe.Attachment{ID: 2, FileName: "evil.exe"}, Keep: false, Block: true},
			{Attachment: probe.Attachment{ID: 3, FileName: "b.otf"}, Keep: true},
		},
	}
	out := filepath.Join(t.TempDir(), ".amuxify-out.mkv.tmp")
	want := []string{
		"-o", out, "--disable-track-statistics-tags", "--no-date",
		"--title", "",
		"--no-global-tags", "--no-track-tags",
		"--no-chapters",
		"--attachments", "1,3",
		"--video-tracks", "0",
		"--audio-tracks", "1,2",
		"--subtitle-tracks", "4",
		"--no-buttons",
		"--default-track-flag", "1:1", "--forced-display-flag", "1:0",
		"--default-track-flag", "2:0", "--forced-display-flag", "2:0", "--track-name", "2:",
		"--default-track-flag", "4:0", "--forced-display-flag", "4:1", "--language", "4:eng",
		"(", in, ")",
	}
	got := rm.mkvmergeArgs(out, m, d)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args\n got %q\nwant %q", got, want)
	}

	// Everything kept and nothing stripped: the negative forms appear and
	// no per-track edits are emitted beyond the flag pairs.
	d2 := &policy.Decision{KeepChapters: true,
		Tracks:      []policy.TrackAction{{Stream: streams[0], Keep: true}},
		Attachments: nil,
	}
	want2 := []string{"-o", out, "--disable-track-statistics-tags", "--no-date", "--no-attachments",
		"--video-tracks", "0", "--no-audio", "--no-subtitles", "--no-buttons", "(", in, ")"}
	if got := rm.mkvmergeArgs(out, m, d2); !reflect.DeepEqual(got, want2) {
		t.Fatalf("minimal args\n got %q\nwant %q", got, want2)
	}

	// A stream without a Matroska id falls back to its ffprobe index, and a
	// hostile language string is passed through as one argument, never
	// split into several.
	s := probe.Stream{Index: 7, MkvID: -1, Type: "audio"}
	d3 := &policy.Decision{Tracks: []policy.TrackAction{{Stream: streams[0], Keep: true}, {Stream: s, Keep: true, SetLanguage: "eng --attachments 2"}}}
	got3 := rm.mkvmergeArgs(out, m, d3)
	found := false
	for i, a := range got3 {
		if a == "--language" && i+1 < len(got3) && got3[i+1] == "7:eng --attachments 2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("language argument not passed verbatim: %q", got3)
	}
	// The input path is always the last argument inside parentheses, so a
	// path that looks like an option is still a path.
	m.Path = "--no-video"
	got4 := rm.mkvmergeArgs(out, m, d2)
	if n := len(got4); got4[n-3] != "(" || got4[n-2] != "--no-video" || got4[n-1] != ")" {
		t.Fatalf("input not bracketed: %q", got4)
	}
}

// Guarantee 2 and 1: output goes through a hidden temp name beside the
// destination and is placed with no clobber.
func TestRemuxWritesViaTempAndPlaces(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	src := testutil.Copy(t, "clean.mkv")
	root := filepath.Dir(src)
	before := fileSHA(t, src)
	outRoot := filepath.Join(t.TempDir(), "out")
	rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	fr := res[0]
	if fr.Verdict != report.Pass || !fr.Has(CodeHashOK) || !fr.Has(CodePlaced) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	dest := filepath.Join(outRoot, "clean.mkv")
	if fr.Output != dest {
		t.Fatalf("Output %q want %q", fr.Output, dest)
	}
	tmp := filepath.Join(outRoot, ".amuxify-clean.mkv.tmp")
	sawTmp := false
	for _, l := range tr.writes() {
		if strings.Contains(l, "-o "+tmp+" ") {
			sawTmp = true
		}
		if strings.Contains(l, "-o "+dest+" ") {
			t.Fatalf("mkvmerge wrote the destination directly: %s", l)
		}
	}
	if !sawTmp {
		t.Fatalf("no mkvmerge run targeting %s in %v", tmp, tr.writes())
	}
	if l := leftovers(t, outRoot, root); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
	if fi, err := os.Lstat(dest); err != nil || fi.Size() == 0 || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("destination: %v", err)
	}
	if fileSHA(t, src) != before {
		t.Fatal("source modified by a non in-place remux")
	}
	// The output really is a clean Matroska file: provenance gone, no tags.
	pr := &probe.Prober{Runner: r, Timeout: time.Minute}
	out, err := pr.Probe(context.Background(), dest)
	if err != nil {
		t.Fatal(err)
	}
	if out.MuxingApp != "" || out.WritingApp != "" || out.GlobalTagN != 0 || out.TrackTagN != 0 {
		t.Fatalf("output not clean: muxing=%q writing=%q tags=%d/%d", out.MuxingApp, out.WritingApp, out.GlobalTagN, out.TrackTagN)
	}
	// A second run refuses to clobber and leaves the first output alone.
	first := fileSHA(t, dest)
	res, err = rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Has(CodeOutputExists) || res[0].Verdict != report.Fail {
		t.Fatalf("second run: %v", codes(res[0]))
	}
	if fileSHA(t, dest) != first {
		t.Fatal("second run replaced the output")
	}
}

// Guarantee 5: a hash mismatch deletes the output and fails.
func TestHashMismatchDeletesOutput(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	src := testutil.Copy(t, "clean.mkv")
	srcBefore, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	shaBefore := fileSHA(t, src)
	outRoot := filepath.Join(t.TempDir(), "out")
	origStream, origDecoded := streamHash, decodedHash
	t.Cleanup(func() { streamHash, decodedHash = origStream, origDecoded })
	streamHash = func(ctx context.Context, v *verify.Verifier, path string, index int) (string, error) {
		if strings.HasPrefix(path, outRoot) {
			return "deadbeef", nil
		}
		return origStream(ctx, v, path, index)
	}
	decodedHash = func(ctx context.Context, v *verify.Verifier, path string, s probe.Stream) (string, error) {
		if strings.HasPrefix(path, outRoot) {
			return "deadbeef", nil
		}
		return origDecoded(ctx, v, path, s)
	}
	rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	res, err := rm.RemuxPath(context.Background(), filepath.Dir(src))
	if err != nil {
		t.Fatal(err)
	}
	fr := res[0]
	if fr.Verdict != report.Fail || !fr.Has(CodeHashMismatch) || fr.Has(CodePlaced) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	var files []string
	filepath.WalkDir(outRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 0 {
		t.Fatalf("output kept after mismatch: %v", files)
	}
	// The source is untouched: same size, same mtime, same bytes.
	srcAfter, err := os.Lstat(src)
	if err != nil {
		t.Fatal(err)
	}
	if srcAfter.Size() != srcBefore.Size() || !srcAfter.ModTime().Equal(srcBefore.ModTime()) {
		t.Fatalf("source size/mtime changed: %d/%v -> %d/%v", srcBefore.Size(), srcBefore.ModTime(), srcAfter.Size(), srcAfter.ModTime())
	}
	if fileSHA(t, src) != shaBefore {
		t.Fatal("source content changed after a hash mismatch")
	}
	if l := leftovers(t, outRoot, filepath.Dir(src)); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
}

// Guarantee 6: in-place keeps the file's identity.
func TestInPlacePreservesIdentity(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mode bits are not meaningful on Windows")
	}
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	src := testutil.Copy(t, "purchased.mp4")
	root := filepath.Dir(src)
	stamp := time.Date(2020, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := os.Chmod(src, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.InPlace = true
	rm.Force = true // purchased.mp4 is WARN only; --force is harmless here
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	fr := res[0]
	if fr.Verdict >= report.Fail || !fr.Has(CodePlaced) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	dest := filepath.Join(root, "purchased.mkv")
	if fr.Output != dest {
		t.Fatalf("Output %q", fr.Output)
	}
	fi, err := os.Lstat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o640 {
		t.Errorf("mode %o want 640", fi.Mode().Perm())
	}
	if !fi.ModTime().Truncate(time.Second).Equal(stamp) {
		t.Errorf("mtime %v want %v", fi.ModTime(), stamp)
	}
	if _, err := os.Lstat(src); err == nil {
		t.Error("the .mp4 is still present after an in-place remux")
	}
	if l := leftovers(t, root); len(l) != 0 {
		t.Errorf("temp files left: %v", l)
	}
	for _, l := range tr.writes() {
		if strings.Contains(l, "-o "+dest+" ") || strings.Contains(l, "-o "+src+" ") {
			t.Errorf("mkvmerge wrote the live path: %s", l)
		}
	}
	pr := &probe.Prober{Runner: r, Timeout: time.Minute}
	out, err := pr.Probe(context.Background(), dest)
	if err != nil {
		t.Fatal(err)
	}
	if !out.IsMatroska() || len(out.StreamsOf("video")) != 1 {
		t.Fatalf("output is not a Matroska file with video: %+v", out.Container)
	}
	// ffprobe reports the (now empty) writing application as an "encoder"
	// tag, so only tags with a value count.
	for k, v := range out.Tags {
		if v != "" && (strings.Contains(strings.ToLower(k), "purchase") || strings.Contains(strings.ToLower(k), "encoder")) {
			t.Errorf("tag %q=%q survived", k, v)
		}
	}
	if out.MuxingApp != "" || out.WritingApp != "" {
		t.Errorf("provenance survived: %q %q", out.MuxingApp, out.WritingApp)
	}
}

func TestHardlinkModes(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	setup := func(t *testing.T) (string, string, string) {
		t.Helper()
		src := testutil.Copy(t, "clean.mkv")
		link := filepath.Join(filepath.Dir(src), "twin.mkv")
		if err := os.Link(src, link); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		return filepath.Dir(src), src, link
	}
	t.Run("skip", func(t *testing.T) {
		root, src, link := setup(t)
		before := fileSHA(t, src)
		rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
		rm.InPlace = true
		rm.Hardlinks = "skip"
		res, err := rm.RemuxPath(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("want 2 results, got %d", len(res))
		}
		for _, fr := range res {
			if fr.Verdict != report.Warn || !fr.Has(CodeHardlinked) || fr.Has(CodePlaced) {
				t.Errorf("%s: %s %v", fr.Path, fr.Verdict, codes(fr))
			}
		}
		if len(tr.writes()) != 0 {
			t.Errorf("writes happened: %v", tr.writes())
		}
		if fileSHA(t, src) != before || fileSHA(t, link) != before {
			t.Error("files changed")
		}
		fi, _ := os.Lstat(src)
		if fsutil.Nlink(fi) != 2 {
			t.Errorf("nlink %d", fsutil.Nlink(fi))
		}
	})
	t.Run("copy", func(t *testing.T) {
		root, src, link := setup(t)
		before := fileSHA(t, src)
		rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
		rm.InPlace = true
		rm.Hardlinks = "copy"
		rm.OutputRoot = filepath.Join(t.TempDir(), "out")
		res, err := rm.RemuxPath(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("want 2 results, got %d", len(res))
		}
		for _, fr := range res {
			if fr.Verdict != report.Pass || !fr.Has(CodeHardlinked) || !fr.Has(CodePlaced) {
				t.Errorf("%s: %s %v", fr.Path, fr.Verdict, codes(fr))
			}
			if !strings.HasPrefix(fr.Output, rm.OutputRoot) {
				t.Errorf("%s: output %q outside %s", fr.Path, fr.Output, rm.OutputRoot)
			}
		}
		if fileSHA(t, src) != before || fileSHA(t, link) != before {
			t.Error("sources changed")
		}
		fi, _ := os.Lstat(src)
		if fsutil.Nlink(fi) != 2 {
			t.Errorf("link broken: nlink %d", fsutil.Nlink(fi))
		}
	})
	t.Run("break", func(t *testing.T) {
		root, src, link := setup(t)
		before := fileSHA(t, src)
		rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
		rm.InPlace = true
		rm.Hardlinks = "break"
		res, err := rm.RemuxPath(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("want 2 results, got %d", len(res))
		}
		// The first name is replaced with a warning, which breaks the link;
		// the second name is then an ordinary single-link file.
		warned := 0
		for _, fr := range res {
			if !fr.Has(CodePlaced) || fr.Verdict >= report.Fail {
				t.Errorf("%s: %s %v", fr.Path, fr.Verdict, codes(fr))
			}
			if fr.Verdict == report.Warn && fr.Has(CodeHardlinked) {
				warned++
			}
		}
		if warned != 1 {
			t.Errorf("%d files warned about the broken link, want 1", warned)
		}
		fa, _ := os.Lstat(src)
		fb, _ := os.Lstat(link)
		if fsutil.Nlink(fa) != 1 || fsutil.Nlink(fb) != 1 {
			t.Errorf("nlink %d/%d, the link was not broken", fsutil.Nlink(fa), fsutil.Nlink(fb))
		}
		if os.SameFile(fa, fb) {
			t.Error("both names still share one inode")
		}
		if fileSHA(t, src) == before && fileSHA(t, link) == before {
			t.Error("nothing was rewritten")
		}
		if l := leftovers(t, root); len(l) != 0 {
			t.Errorf("temp files left: %v", l)
		}
	})
}

// Guarantee 2 and 8: dry run touches nothing, and reports what it would do.
func TestDryRunWritesNothing(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVExtract)
	src := testutil.Copy(t, "multi.mkv")
	root := filepath.Dir(src)
	before := fileSHA(t, src)
	outRoot := filepath.Join(t.TempDir(), "out")
	for _, inPlace := range []bool{false, true} {
		rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
		rm.DryRun = true
		rm.InPlace = inPlace
		if !inPlace {
			rm.OutputRoot = outRoot
		}
		res, err := rm.RemuxPath(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		fr := res[0]
		if !fr.Has(CodeDryRun) || fr.Has(CodePlaced) || fr.Verdict >= report.Fail {
			t.Fatalf("inPlace=%v: %s %v", inPlace, fr.Verdict, codes(fr))
		}
		if len(tr.writes()) != 0 {
			t.Fatalf("inPlace=%v: writes: %v", inPlace, tr.writes())
		}
		if fileSHA(t, src) != before {
			t.Fatal("source changed")
		}
		entries, _ := os.ReadDir(root)
		if len(entries) != 1 {
			t.Fatalf("inPlace=%v: input dir now holds %d entries", inPlace, len(entries))
		}
	}
	if _, err := os.Lstat(outRoot); err == nil {
		t.Fatal("dry run created the output root")
	}
}

// A remux that fails midway leaves no temp file and no destination.
func TestFailedRemuxLeavesNothing(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVExtract)
	src := testutil.Copy(t, "clean.mkv")
	root := filepath.Dir(src)
	before := fileSHA(t, src)
	outRoot := filepath.Join(t.TempDir(), "out")
	rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	rm.Timeout = time.Nanosecond // mkvmerge is killed at once
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	fr := res[0]
	if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	var files []string
	filepath.WalkDir(outRoot, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if len(files) != 0 {
		t.Fatalf("files left after failure: %v", files)
	}
	if fileSHA(t, src) != before {
		t.Fatal("source changed")
	}
}

// Over the whole corpus with --force: BLOCK files are refused, no file is
// written in a dry run, and non-media files are only scanned.
func TestCorpusRefusalsWithForce(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVExtract)
	root := testutil.CopyTree(t)
	rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.Force = true
	rm.DryRun = true
	rm.OutputRoot = filepath.Join(t.TempDir(), "out")
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 {
		t.Fatal("no results")
	}
	for _, fr := range res {
		rel, _ := filepath.Rel(root, fr.Path)
		blocked := false
		for _, f := range fr.Findings {
			if f.Severity == report.Block && f.Code != CodeRefused {
				blocked = true
			}
		}
		if blocked && (fr.Verdict != report.Block || fr.Has(CodeDryRun) || fr.Has(CodePlaced)) {
			t.Errorf("%s: blocked file not refused: %v", rel, codes(fr))
		}
		if !scan.IsMedia(fr.Path) && fr.Has(CodeDryRun) {
			t.Errorf("%s: non-media file planned for remux", rel)
		}
	}
	if len(tr.writes()) != 0 {
		t.Fatalf("writes in a dry run: %v", tr.writes())
	}
	if _, err := os.Lstat(rm.OutputRoot); err == nil {
		t.Fatal("dry run created the output root")
	}
}

// A symlink planted inside the output tree must not redirect the placed
// file somewhere else.
func TestOutputRefusesSymlinkedSubdir(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	root := t.TempDir()
	src := filepath.Join(root, "sub", "clean.mkv")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(testutil.Copy(t, "clean.mkv"), src); err != nil {
		t.Fatal(err)
	}
	outRoot := filepath.Join(t.TempDir(), "out")
	elsewhere := t.TempDir()
	if err := os.MkdirAll(outRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(outRoot, "sub")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	before := fileSHA(t, src)
	rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, "clean.mkv")); err == nil {
		t.Fatalf("remux followed a symlinked directory inside the output root and placed the file outside it (%v)", codes(res[0]))
	}
	if len(leftovers(t, elsewhere)) != 0 || len(leftovers(t, outRoot)) != 0 {
		t.Fatalf("temp files left behind: %v %v", leftovers(t, elsewhere), leftovers(t, outRoot))
	}
	if len(res) != 1 || res[0].Verdict != report.Fail || !res[0].Has(CodeRemuxFail) {
		t.Fatalf("want FAIL REMUX_FAIL for the refused destination, got %v", codes(res[0]))
	}
	if fi, err := os.Lstat(filepath.Join(outRoot, "sub")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("planted symlink was replaced or removed: %v", err)
	}
	if fileSHA(t, src) != before {
		t.Fatal("source changed by a refused remux")
	}
	// Nothing was written inside elsewhere at all.
	entries, err := os.ReadDir(elsewhere)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("output tree escaped through the symlink: %v", entries)
	}
}

// shq quotes s for a POSIX shell.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// mkvmergeWrapper installs a shell script as AMUXIFY_MKVMERGE that runs the
// before snippet when mkvmerge is called with -o (a write), then the real
// mkvmerge with the original arguments, then the after snippet, and exits
// with mkvmerge's status. Identify calls (-J) pass straight through. The
// Runner strips the environment, so every path a snippet needs must be
// baked into it with shq. Tests use it to change the filesystem in the
// window between the remuxer's checks and its placement.
func mkvmergeWrapper(t *testing.T, r *exec.Runner, before, after string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the wrapper is a POSIX shell script")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}
	real, err := r.Path(exec.MKVMerge)
	if err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(t.TempDir(), "mkvmerge")
	body := "#!/bin/sh\nwrite=0\nfor a in \"$@\"; do [ \"$a\" = -o ] && write=1; done\n" +
		"if [ \"$write\" = 1 ]; then\n:\n" + before + "\nfi\n" +
		shq(real) + " \"$@\"\nrc=$?\n" +
		"if [ \"$write\" = 1 ]; then\n:\n" + after + "\nfi\nexit $rc\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_MKVMERGE", script)
}

// Guarantee 1 under in-place mode: when the source is not .mkv already, a
// file that appears at the .mkv name while mkvmerge runs is never replaced.
// The wrapper plants it after the up-front existence check has passed. The
// result is FAIL OUTPUT_EXISTS, the planted file is byte-identical, and the
// source is untouched.
func TestInPlacePlantedDestinationNotClobbered(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	src := testutil.Copy(t, "purchased.mp4")
	root := filepath.Dir(src)
	dest := filepath.Join(root, "purchased.mkv")
	before := fileSHA(t, src)
	mkvmergeWrapper(t, r, "printf 'PRECIOUS USER DATA' > "+shq(dest), "")
	rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
	rm.InPlace = true
	rm.Force = true // purchased.mp4 is WARN only
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	fr := res[0]
	if len(tr.writes()) == 0 {
		t.Fatalf("mkvmerge never wrote anything; the placement was not exercised: %v", codes(fr))
	}
	if fr.Verdict != report.Fail || !fr.Has(CodeOutputExists) || fr.Has(CodePlaced) || fr.Output != "" {
		t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
	}
	if b, err := os.ReadFile(dest); err != nil || string(b) != "PRECIOUS USER DATA" {
		t.Fatalf("planted destination was replaced: %q %v", b, err)
	}
	if fileSHA(t, src) != before {
		t.Fatal("source changed")
	}
	if fi, err := os.Lstat(src); err != nil || fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("source no longer a regular file: %v", err)
	}
	if l := leftovers(t, root); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
	said := false
	for _, f := range fr.Findings {
		if f.Code == CodeOutputExists && strings.Contains(f.Message, "source was left untouched") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the finding does not say what happened to the source: %v", fr.Findings)
	}
}

// caseInsensitiveDir reports whether dir resolves X.MKV and X.mkv to one
// entry, which is the default on macOS APFS and on Windows.
func caseInsensitiveDir(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, ".amuxify-case-probe.MKV")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(p)
	a, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.Lstat(filepath.Join(dir, ".amuxify-case-probe.mkv"))
	return err == nil && os.SameFile(a, b)
}

// An in-place remux of X.MKV must not report OUTPUT_EXISTS against itself on
// a case-insensitive filesystem, and must still refuse a distinct X.mkv on
// a case-sensitive one, even when that X.mkv is a hard link of the source.
func TestInPlaceUpperCaseExtension(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	setup := func(t *testing.T) (string, string) {
		t.Helper()
		src := testutil.Copy(t, "clean.mkv")
		upper := filepath.Join(filepath.Dir(src), "CLEAN.MKV")
		if err := os.Rename(src, upper); err != nil {
			t.Fatal(err)
		}
		return filepath.Dir(upper), upper
	}
	byName := func(res []report.FileResult, name string) (report.FileResult, bool) {
		for _, fr := range res {
			if filepath.Base(fr.Path) == name {
				return fr, true
			}
		}
		return report.FileResult{}, false
	}
	names := func(t *testing.T, dir string) []string {
		t.Helper()
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, e := range entries {
			out = append(out, e.Name())
		}
		return out
	}

	t.Run("same entry on a case-insensitive filesystem", func(t *testing.T) {
		root, src := setup(t)
		if !caseInsensitiveDir(t, root) {
			t.Skip("case-sensitive filesystem: CLEAN.MKV and CLEAN.mkv are distinct names here; the hard-link subtest covers that")
		}
		rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
		rm.InPlace = true
		res, err := rm.RemuxPath(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 {
			t.Fatalf("%d results", len(res))
		}
		fr := res[0]
		if fr.Has(CodeOutputExists) || fr.Verdict >= report.Fail || !fr.Has(CodePlaced) {
			t.Fatalf("%s %v", fr.Verdict, codes(fr))
		}
		want := filepath.Join(root, "CLEAN.mkv")
		if fr.Output != want {
			t.Fatalf("Output %q want %q", fr.Output, want)
		}
		if got := names(t, root); !reflect.DeepEqual(got, []string{"CLEAN.mkv"}) {
			t.Fatalf("directory holds %v, want exactly [CLEAN.mkv]", got)
		}
		if _, err := os.Lstat(src); err != nil {
			t.Fatalf("the entry vanished: %v", err)
		}
		pr := &probe.Prober{Runner: r, Timeout: time.Minute}
		out, err := pr.Probe(context.Background(), want)
		if err != nil || !out.IsMatroska() {
			t.Fatalf("output is not Matroska: %v", err)
		}
		if l := leftovers(t, root); len(l) != 0 {
			t.Fatalf("temp files left: %v", l)
		}
	})

	t.Run("hard-linked twin spelled with the lower-case extension", func(t *testing.T) {
		root, src := setup(t)
		twin := filepath.Join(root, "CLEAN.mkv")
		if err := os.Link(src, twin); err != nil {
			t.Skipf("cannot create CLEAN.mkv beside CLEAN.MKV (case-insensitive filesystem): %v", err)
		}
		before := fileSHA(t, src)
		rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
		rm.InPlace = true
		rm.Hardlinks = "break"
		res, err := rm.RemuxPath(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("want 2 results, got %d", len(res))
		}
		// CLEAN.MKV cannot take the name CLEAN.mkv: that is a distinct
		// file. Whichever of the two names was processed first, the file
		// behind CLEAN.MKV was never renamed over its twin.
		up, ok := byName(res, "CLEAN.MKV")
		if !ok {
			t.Fatalf("no result for CLEAN.MKV: %v", res)
		}
		if !up.Has(CodeOutputExists) || up.Verdict != report.Fail || up.Has(CodePlaced) {
			t.Fatalf("CLEAN.MKV: %s %v", up.Verdict, codes(up))
		}
		if fileSHA(t, src) != before {
			t.Fatal("CLEAN.MKV was rewritten although its .mkv name was taken")
		}
		if got := names(t, root); !reflect.DeepEqual(got, []string{"CLEAN.MKV", "CLEAN.mkv"}) {
			t.Fatalf("directory holds %v", got)
		}
		if l := leftovers(t, root); len(l) != 0 {
			t.Fatalf("temp files left: %v", l)
		}
	})

	t.Run("case-insensitive with a hard link under another name", func(t *testing.T) {
		root, src := setup(t)
		if !caseInsensitiveDir(t, root) {
			t.Skip("case-sensitive filesystem: the same-entry case does not arise here")
		}
		twin := filepath.Join(root, "twin.mkv")
		if err := os.Link(src, twin); err != nil {
			t.Skipf("hard links unavailable: %v", err)
		}
		rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
		rm.InPlace = true
		rm.Hardlinks = "break"
		res, err := rm.RemuxPath(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("want 2 results, got %d", len(res))
		}
		up, ok := byName(res, "CLEAN.MKV")
		if !ok {
			t.Fatalf("no result for CLEAN.MKV: %v", res)
		}
		if up.Has(CodeOutputExists) || up.Verdict >= report.Fail || !up.Has(CodePlaced) || up.Output != filepath.Join(root, "CLEAN.mkv") {
			t.Fatalf("CLEAN.MKV: %s %v output=%q", up.Verdict, codes(up), up.Output)
		}
		if got := names(t, root); !reflect.DeepEqual(got, []string{"CLEAN.mkv", "twin.mkv"}) {
			t.Fatalf("directory holds %v", got)
		}
		if l := leftovers(t, root); len(l) != 0 {
			t.Fatalf("temp files left: %v", l)
		}
	})
}

// Guarantee 4 under in-place mode: hardlinks=copy sends a hard-linked file
// to the mirrored output tree, and that tree is created without following
// symlinks, exactly as in output mode. A symlink planted at the mirrored
// subdirectory is refused and nothing is written through it, whether the
// output root is explicit or the derived <root>__remuxed.
func TestInPlaceCopyModeRefusesSymlinkedOutputSubdir(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, tc := range []struct {
		name     string
		explicit bool
	}{
		{"explicit output root", true},
		{"derived output root", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "in")
			src := filepath.Join(root, "sub", "a.mkv")
			if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(testutil.Copy(t, "clean.mkv"), src); err != nil {
				t.Fatal(err)
			}
			twin := filepath.Join(root, "sub", "b.mkv")
			if err := os.Link(src, twin); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
			outRoot := root + "__remuxed"
			if tc.explicit {
				outRoot = filepath.Join(t.TempDir(), "out")
			}
			elsewhere := t.TempDir()
			if err := os.MkdirAll(outRoot, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(elsewhere, filepath.Join(outRoot, "sub")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			before := fileSHA(t, src)
			rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
			rm.InPlace = true
			rm.Hardlinks = "copy"
			if tc.explicit {
				rm.OutputRoot = outRoot
			}
			res, err := rm.RemuxPath(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 2 {
				t.Fatalf("want 2 results, got %d", len(res))
			}
			for _, fr := range res {
				if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) || fr.Output != "" {
					t.Errorf("%s: %s %v output=%q", fr.Path, fr.Verdict, codes(fr), fr.Output)
				}
			}
			if w := tr.writes(); len(w) != 0 {
				t.Errorf("mkvmerge was started although the output directory was refused: %v", w)
			}
			entries, err := os.ReadDir(elsewhere)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("output escaped through the symlink: %v", entries)
			}
			if fi, err := os.Lstat(filepath.Join(outRoot, "sub")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("planted symlink was replaced or removed: %v", err)
			}
			if l := leftovers(t, elsewhere, outRoot, root); len(l) != 0 {
				t.Fatalf("temp files left: %v", l)
			}
			if fileSHA(t, src) != before || fileSHA(t, twin) != before {
				t.Fatal("sources changed")
			}
			fi, _ := os.Lstat(src)
			if fsutil.Nlink(fi) != 2 {
				t.Errorf("link broken: nlink %d", fsutil.Nlink(fi))
			}
		})
	}
}

// Guarantee 3 across the scan-to-remux window: a scanned regular file that
// is swapped for a symlink before the remuxer opens it is refused, no tool
// runs against the link, and the file behind the link is untouched. The
// victim is real media so a followed link would have succeeded.
func TestSourceSwappedForSymlinkRefused(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, tc := range []struct {
		name    string
		inPlace bool
	}{
		{"before mkvmerge in place", true},
		{"before mkvmerge to output tree", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "a.mkv")
			if err := os.Rename(testutil.Copy(t, "clean.mkv"), path); err != nil {
				t.Fatal(err)
			}
			victim := testutil.Copy(t, "clean.mkv")
			victimBefore := fileSHA(t, victim)
			outRoot := filepath.Join(t.TempDir(), "out")
			rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
			rm.InPlace = tc.inPlace
			rm.OutputRoot = outRoot
			sc := rm.Scanner.ScanFile(context.Background(), path, dir)
			if sc.Info == nil || sc.File.Verdict >= report.Fail {
				t.Fatalf("scan: %v", codes(sc.File))
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(victim, path); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			scanned := len(tr.all())
			fr := rm.RemuxScanned(context.Background(), sc, dir, outRoot)
			if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) || fr.Output != "" {
				t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
			}
			said := false
			for _, f := range fr.Findings {
				if f.Code == CodeRemuxFail && strings.Contains(f.Message, "symlink") {
					said = true
				}
			}
			if !said {
				t.Fatalf("the finding does not name the symlink: %v", fr.Findings)
			}
			if got := tr.all()[scanned:]; len(got) != 0 {
				t.Fatalf("tools ran against the swapped path: %v", got)
			}
			if fileSHA(t, victim) != victimBefore {
				t.Fatal("symlink target changed")
			}
			if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Fatalf("the planted symlink was replaced: %v", err)
			}
			if _, err := os.Lstat(outRoot); err == nil {
				t.Fatal("output root created for a refused file")
			}
			if l := leftovers(t, dir); len(l) != 0 {
				t.Fatalf("temp files left: %v", l)
			}
		})
	}

	// The swap can also land after mkvmerge has read the real file and
	// before the verified output replaces it. The wrapper performs the
	// swap as mkvmerge exits; the in-place placement must then refuse
	// rather than rename over the link or take the victim's identity.
	t.Run("after mkvmerge before placement in place", func(t *testing.T) {
		src := testutil.Copy(t, "clean.mkv")
		dir := filepath.Dir(src)
		victim := testutil.Copy(t, "clean.mkv")
		victimBefore := fileSHA(t, victim)
		mkvmergeWrapper(t, r, "", "rm -f "+shq(src)+" && ln -s "+shq(victim)+" "+shq(src))
		rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
		rm.InPlace = true
		res, err := rm.RemuxPath(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 {
			t.Fatalf("%d results", len(res))
		}
		fr := res[0]
		if len(tr.writes()) == 0 {
			t.Fatalf("mkvmerge never ran: %v", codes(fr))
		}
		if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) || fr.Output != "" {
			t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
		}
		if fileSHA(t, victim) != victimBefore {
			t.Fatal("symlink target changed")
		}
		if fi, err := os.Lstat(src); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("the planted symlink was replaced: %v", err)
		}
		if l := leftovers(t, dir); len(l) != 0 {
			t.Fatalf("temp files left: %v", l)
		}
		entries, _ := os.ReadDir(dir)
		if len(entries) != 1 {
			t.Fatalf("directory holds %d entries, want only the symlink", len(entries))
		}
	})
}

// A placement failure that is not a file at the destination must be
// reported as REMUX_FAIL, not as OUTPUT_EXISTS, which would tell the user a
// file is in the way when there is none. The wrapper takes write permission
// off the output directory once mkvmerge has written the temp file, so the
// link and the rename both fail with a permission error. The temp file
// cannot be removed from a directory that refuses writes, so leftovers are
// not asserted here; the directory is made writable again on cleanup.
func TestOutputPlacementErrorIsRemuxFail(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	src := testutil.Copy(t, "clean.mkv")
	root := filepath.Dir(src)
	before := fileSHA(t, src)
	outRoot := filepath.Join(t.TempDir(), "out")
	dest := filepath.Join(outRoot, "clean.mkv")
	t.Cleanup(func() { _ = os.Chmod(outRoot, 0o755) })
	mkvmergeWrapper(t, r, "", "chmod 0555 "+shq(outRoot))
	rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("%d results", len(res))
	}
	fr := res[0]
	if len(tr.writes()) == 0 {
		t.Fatalf("mkvmerge never ran: %v", codes(fr))
	}
	if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodeOutputExists) || fr.Has(CodePlaced) || fr.Output != "" {
		t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
	}
	said := false
	for _, f := range fr.Findings {
		if f.Code == CodeRemuxFail && strings.HasPrefix(f.Message, "place: ") {
			said = true
		}
	}
	if !said {
		t.Fatalf("the finding does not name the placement: %v", fr.Findings)
	}
	if _, err := os.Lstat(dest); err == nil {
		t.Fatal("output placed although placement failed")
	}
	if fileSHA(t, src) != before {
		t.Fatal("source changed")
	}
}
