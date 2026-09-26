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

// ffmpegWrapper installs a shell script as AMUXIFY_FFMPEG that runs the
// snippet before the real ffmpeg whenever ffmpeg is asked to decode (-f
// null) an input whose name carries the .amuxify- temp prefix, which is
// the verification pass over the rebuilt output. Every other call passes
// straight through. The Runner strips the environment, so paths a snippet
// needs must be baked into it with shq. Tests use it to change the source
// after the last time the remuxer reads it and before it places the output.
func ffmpegWrapper(t *testing.T, r *exec.Runner, onTempDecode string) {
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
	body := "#!/bin/sh\ntmp=0\nnull=0\nprev=\nfor a in \"$@\"; do\n" +
		"  case \"$prev:$a\" in -i:*/.amuxify-*) tmp=1;; -f:null) null=1;; esac\n" +
		"  prev=$a\ndone\n" +
		"if [ \"$tmp\" = 1 ] && [ \"$null\" = 1 ]; then\n:\n" + onTempDecode + "\nfi\n" +
		"exec " + shq(real) + " \"$@\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_FFMPEG", script)
}

// Guarantee 1 and 3 for the in-place window after the last read of the
// source: the stream hashes are taken from the source, then only the temp
// file is decoded and synced. A regular file renamed onto the source path in
// that window is not what was rebuilt and verified, so it must be neither
// renamed over (an .mkv source) nor deleted (any other source) once the
// output is placed. The wrapper performs the swap during the decode pass
// over the temp file. The swapped-in file must keep its bytes, nothing may
// be placed, and no temp file may remain.
func TestSourceSwappedForFileRefused(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, tc := range []struct {
		name    string
		fixture string
		swap    string
		force   bool
	}{
		{"mkv source replaced in place", "clean.mkv", "multi.mkv", false},
		{"mp4 source placed under a new name", "purchased.mp4", "sample.mov", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := testutil.Copy(t, tc.fixture)
			dir := filepath.Dir(src)
			swap := testutil.Copy(t, tc.swap)
			swapSum := fileSHA(t, swap)
			if swapSum == fileSHA(t, src) {
				t.Fatal("test setup: the swap must differ from the source")
			}
			dest := filepath.Join(dir, strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))+".mkv")
			ffmpegWrapper(t, r, "[ -e "+shq(swap)+" ] && mv -f "+shq(swap)+" "+shq(src))
			rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
			rm.InPlace = true
			rm.Force = tc.force
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
			if _, err := os.Lstat(swap); err == nil {
				t.Fatal("the wrapper never swapped the source; the placement was not exercised")
			}
			if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) || fr.Output != "" {
				t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
			}
			said := false
			for _, f := range fr.Findings {
				if f.Code == CodeRemuxFail && strings.Contains(f.Message, "was replaced") && strings.Contains(f.Message, "source was left untouched") {
					said = true
				}
			}
			if !said {
				t.Fatalf("the finding does not say what happened: %v", fr.Findings)
			}
			if fileSHA(t, src) != swapSum {
				t.Fatal("the swapped-in file was replaced or removed")
			}
			if dest != src {
				if _, err := os.Lstat(dest); err == nil {
					t.Fatalf("%s was placed although the source changed", dest)
				}
			}
			if l := leftovers(t, dir); len(l) != 0 {
				t.Fatalf("temp files left: %v", l)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 1 {
				t.Fatalf("directory holds %d entries, want only the swapped-in file", len(entries))
			}
		})
	}
}

// Guarantee 1 and 3 for the temp file: a symlink planted at the
// .amuxify-<name>.tmp path must never be verified as this run's output
// nor placed. The wrapper swaps the -o target for a link to a victim, once
// before the real mkvmerge opens the name and once after it has written.
// In the first window mkvmerge itself follows the link and rewrites the
// victim, which no check inside amuxify can prevent once the name has been
// handed over; what amuxify guarantees is that the result is refused, that
// nothing is placed, that the source is untouched and that only the planted
// link is removed. In the second window the victim keeps its bytes as well.
// The victim is real media identical to the source, so a followed link
// would have verified and been placed.
func TestPlantedTempSymlinkRefused(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, tc := range []struct {
		name    string
		inPlace bool
		before  bool
	}{
		{"before mkvmerge to output tree", false, true},
		{"after mkvmerge to output tree", false, false},
		{"before mkvmerge in place", true, true},
		{"after mkvmerge in place", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := testutil.Copy(t, "clean.mkv")
			dir := filepath.Dir(src)
			srcBefore := fileSHA(t, src)
			victim := testutil.Copy(t, "clean.mkv")
			victimBefore := fileSHA(t, victim)
			outRoot := filepath.Join(t.TempDir(), "out")
			dest := filepath.Join(outRoot, "clean.mkv")
			if tc.inPlace {
				dest = src
			}
			tmp := fsutil.TempName(dest)
			plant := "rm -f " + shq(tmp) + " && ln -s " + shq(victim) + " " + shq(tmp)
			if tc.before {
				mkvmergeWrapper(t, r, plant, "")
			} else {
				mkvmergeWrapper(t, r, "", plant)
			}
			rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
			rm.InPlace = tc.inPlace
			if !tc.inPlace {
				rm.OutputRoot = outRoot
			}
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
			if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) || fr.Has(CodeHashOK) || fr.Output != "" {
				t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
			}
			said := false
			for _, f := range fr.Findings {
				if f.Code == CodeRemuxFail && strings.Contains(f.Message, "symlink") && strings.Contains(f.Message, "nothing was placed") {
					said = true
				}
			}
			if !said {
				t.Fatalf("the finding does not name the symlink: %v", fr.Findings)
			}
			if fileSHA(t, src) != srcBefore {
				t.Fatal("source changed")
			}
			if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() || fsutil.Nlink(fi) != 1 {
				t.Fatalf("source is no longer a plain regular file: %v", err)
			}
			if !tc.inPlace {
				if _, err := os.Lstat(dest); err == nil {
					t.Fatalf("%s was placed through the planted link", dest)
				}
			}
			if _, err := os.Lstat(tmp); err == nil {
				t.Fatal("the planted symlink is still there")
			}
			if fi, err := os.Lstat(victim); err != nil || fi.Mode()&os.ModeSymlink != 0 || fsutil.Nlink(fi) != 1 {
				t.Fatalf("victim is no longer a plain regular file: %v", err)
			}
			if !tc.before && fileSHA(t, victim) != victimBefore {
				t.Fatal("victim rewritten although mkvmerge had already finished")
			}
			if l := leftovers(t, dir, outRoot); len(l) != 0 {
				t.Fatalf("temp files left: %v", l)
			}
		})
	}
}

// Guarantee 1 and 3 for the window tempUnchanged cannot cover: the temp
// name is checked, and then the placement primitive uses that name again.
// A symlink swapped onto the name in between is followed by link(2) on
// macOS, which would leave a hard link to the victim at the destination, and
// is moved as a link by the rename fallback. The seam performs the swap in
// exactly that window, after the last check and before the placement. In
// output mode and for a non-.mkv source in place the foreign entry must be
// discarded, nothing may be reported as placed, the source and the victim
// must keep their bytes and their single link, and no temp file may remain.
// For an .mkv source the rename over the source cannot be checked first, so
// the run must notice afterwards, say that the source is gone, and leave the
// entry alone rather than follow or remove it; the victim keeps its bytes
// and its single link there as well.
func TestTempSwappedBeforePlacementRefused(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, tc := range []struct {
		name    string
		fixture string
		inPlace bool
		force   bool
		rename  bool // the .mkv source path, where tmp is renamed over the source
	}{
		{"output tree", "clean.mkv", false, false, false},
		{"in place under a new name", "purchased.mp4", true, true, false},
		{"in place over the source", "clean.mkv", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := testutil.Copy(t, tc.fixture)
			dir := filepath.Dir(src)
			srcBefore := fileSHA(t, src)
			victim := testutil.Copy(t, "clean.mkv")
			victimBefore := fileSHA(t, victim)
			outRoot := filepath.Join(t.TempDir(), "out")
			dest := filepath.Join(outRoot, "clean.mkv")
			if tc.inPlace {
				dest = filepath.Join(dir, strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))+".mkv")
			}
			swapped := false
			t.Cleanup(func() { beforePlace = nil })
			beforePlace = func(tmp, _ string) {
				if err := os.Remove(tmp); err != nil {
					t.Fatalf("swap: %v", err)
				}
				if err := os.Symlink(victim, tmp); err != nil {
					t.Fatalf("swap: %v", err)
				}
				swapped = true
			}
			rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
			rm.InPlace = tc.inPlace
			rm.Force = tc.force
			if !tc.inPlace {
				rm.OutputRoot = outRoot
			}
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
			if !swapped {
				t.Fatalf("the seam never ran; the placement was not exercised: %v", codes(fr))
			}
			if fr.Verdict != report.Fail || !fr.Has(CodeRemuxFail) || fr.Has(CodePlaced) || fr.Output != "" {
				t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
			}
			want := []string{"swapped before placement", "was discarded"}
			if tc.rename {
				want = []string{"not the file this run built", "is gone"}
			}
			said := false
			for _, f := range fr.Findings {
				if f.Code == CodeRemuxFail && strings.Contains(f.Message, want[0]) && strings.Contains(f.Message, want[1]) {
					said = true
				}
			}
			if !said {
				t.Fatalf("the finding does not say what happened: %v", fr.Findings)
			}
			if fi, err := os.Lstat(victim); err != nil || !fi.Mode().IsRegular() || fsutil.Nlink(fi) != 1 {
				t.Fatalf("victim is no longer a plain regular file with one name: %v", err)
			}
			if fileSHA(t, victim) != victimBefore {
				t.Fatal("victim rewritten")
			}
			if tc.rename {
				// The source was renamed over before the swap could be
				// seen; the entry that took its place is the planted link
				// and it must have been left exactly as it was found.
				fi, err := os.Lstat(src)
				if err != nil || fi.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("the planted link at %s was removed or replaced: %v", src, err)
				}
				if target, err := os.Readlink(src); err != nil || target != victim {
					t.Fatalf("the entry at %s is not the planted link: %q %v", src, target, err)
				}
			} else {
				if fileSHA(t, src) != srcBefore {
					t.Fatal("source changed")
				}
				if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() || fsutil.Nlink(fi) != 1 {
					t.Fatalf("source is no longer a plain regular file: %v", err)
				}
				if _, err := os.Lstat(dest); err == nil {
					t.Fatalf("%s still holds the swapped entry", dest)
				}
			}
			if l := leftovers(t, dir, outRoot); len(l) != 0 {
				t.Fatalf("temp files left: %v", l)
			}
		})
	}
}

// placedOwn is the check behind the test above. It is driven here with
// every entry an attacker could leave at the destination: the run's own
// file passes; a symlink is removed without being followed; a hard link to
// a victim is removed and the victim keeps its bytes and its single name; a
// foreign regular file with no other name is left in place and reported as
// such; a directory is left alone.
func TestPlacedOwn(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "own")
	if err := os.WriteFile(own, []byte("own"), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err := os.Lstat(own)
	if err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("victim"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := placedOwn(own, created); err != nil {
		t.Fatalf("own file refused: %v", err)
	}
	if _, err := os.Lstat(own); err != nil {
		t.Fatalf("own file removed: %v", err)
	}

	link := filepath.Join(dir, "link")
	if err := os.Symlink(victim, link); err != nil {
		t.Fatal(err)
	}
	err = placedOwn(link, created)
	if err == nil || !strings.Contains(err.Error(), "swapped before placement") || !strings.Contains(err.Error(), "was discarded") {
		t.Fatalf("symlink: %v", err)
	}
	if _, err := os.Lstat(link); err == nil {
		t.Fatal("symlink left at the destination")
	}

	hard := filepath.Join(dir, "hard")
	if err := os.Link(victim, hard); err != nil {
		t.Fatal(err)
	}
	err = placedOwn(hard, created)
	if err == nil || !strings.Contains(err.Error(), "was discarded") {
		t.Fatalf("hard link: %v", err)
	}
	if _, err := os.Lstat(hard); err == nil {
		t.Fatal("hard link left at the destination")
	}
	if fi, err := os.Lstat(victim); err != nil || fsutil.Nlink(fi) != 1 {
		t.Fatalf("victim lost its file or kept an extra name: %v", err)
	}
	if b, _ := os.ReadFile(victim); string(b) != "victim" {
		t.Fatalf("victim rewritten: %q", b)
	}

	foreign := filepath.Join(dir, "foreign")
	if err := os.WriteFile(foreign, []byte("foreign"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = placedOwn(foreign, created)
	if err == nil || !strings.Contains(err.Error(), "left in place") {
		t.Fatalf("foreign file: %v", err)
	}
	if b, err := os.ReadFile(foreign); err != nil || string(b) != "foreign" {
		t.Fatalf("the only name of a foreign file was removed: %q %v", b, err)
	}

	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := placedOwn(sub, created); err == nil {
		t.Fatal("directory accepted as the placed output")
	}
	if _, err := os.Lstat(sub); err != nil {
		t.Fatalf("directory removed: %v", err)
	}

	err = placedOwn(filepath.Join(dir, "missing"), created)
	if err == nil || !strings.Contains(err.Error(), "could not be examined") {
		t.Fatalf("missing: %v", err)
	}
}

// replacedOwn never removes anything, whatever sits at the path, because
// the source it would have protected is already gone.
func TestReplacedOwnLeavesTheEntry(t *testing.T) {
	dir := t.TempDir()
	own := filepath.Join(dir, "own")
	if err := os.WriteFile(own, []byte("own"), 0o644); err != nil {
		t.Fatal(err)
	}
	created, err := os.Lstat(own)
	if err != nil {
		t.Fatal(err)
	}
	if err := replacedOwn(own, created); err != nil {
		t.Fatalf("own file refused: %v", err)
	}
	victim := filepath.Join(dir, "victim")
	if err := os.WriteFile(victim, []byte("victim"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, plant := range map[string]func(p string) error{
		"symlink":   func(p string) error { return os.Symlink(victim, p) },
		"hard link": func(p string) error { return os.Link(victim, p) },
		"file":      func(p string) error { return os.WriteFile(p, []byte("foreign"), 0o644) },
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		if err := plant(p); err != nil {
			t.Fatal(err)
		}
		err := replacedOwn(p, created)
		if err == nil || !strings.Contains(err.Error(), "not the file this run built") || !strings.Contains(err.Error(), "is gone") {
			t.Fatalf("%s: %v", name, err)
		}
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s: the entry was removed: %v", name, err)
		}
	}
	if b, _ := os.ReadFile(victim); string(b) != "victim" {
		t.Fatalf("victim rewritten: %q", b)
	}
	if err := replacedOwn(filepath.Join(dir, "missing"), created); err == nil || !strings.Contains(err.Error(), "is gone") {
		t.Fatalf("missing: %v", err)
	}
}

// When the source could not be examined when the remux began, the check
// says so instead of claiming the file was replaced, and a file that really
// was replaced keeps its own message.
func TestSourceUnchangedNilInfoMessage(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.mkv")
	if err := os.WriteFile(p, []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := sourceUnchanged(p, nil)
	if err == nil {
		t.Fatal("a nil FileInfo was accepted")
	}
	if want := p + " could not be examined when the remux began; nothing was placed"; err.Error() != want {
		t.Fatalf("message %q, want %q", err.Error(), want)
	}
	was, err := os.Lstat(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := sourceUnchanged(p, was); err != nil {
		t.Fatalf("unchanged file refused: %v", err)
	}
	if err := os.WriteFile(p, []byte("ab"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = sourceUnchanged(p, was)
	if err == nil || !strings.Contains(err.Error(), "was replaced while it was being rebuilt") || strings.Contains(err.Error(), "could not be examined") {
		t.Fatalf("replaced file: %v", err)
	}
}

// finding returns the first finding of fr with the given code.
func finding(fr report.FileResult, code string) (report.Finding, bool) {
	for _, f := range fr.Findings {
		if f.Code == code {
			return f, true
		}
	}
	return report.Finding{}, false
}

// byBase indexes results by the base name of their path.
func byBase(res []report.FileResult) map[string]report.FileResult {
	out := map[string]report.FileResult{}
	for _, fr := range res {
		out[filepath.Base(fr.Path)] = fr
	}
	return out
}

// collisionPair puts two fixtures into a fresh directory under the names
// first and second, which share a stem so both rebuild to <stem>.mkv.
func collisionPair(t *testing.T, first, second string) string {
	t.Helper()
	dir := t.TempDir()
	for name, as := range map[string]string{"sample.mov": first, "sample.webm": second} {
		src := testutil.Copy(t, name)
		if err := os.Rename(src, filepath.Join(dir, as)); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// A dry run of two files that rebuild to one destination, ep.mov and
// ep.webm, reports the second with the OUTPUT_EXISTS the live run gives,
// for --output and for --in-place, and writes nothing. The plan lives in
// the Remuxer for the whole run, so the collision is also found when the
// two files arrive as separate positional paths, which is how the CLI
// calls RemuxPath; a fresh Remuxer starts a fresh plan.
func TestDryRunPredictsDestinationCollision(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, inPlace := range []bool{false, true} {
		mode := "output"
		if inPlace {
			mode = "in place"
		}
		t.Run(mode, func(t *testing.T) {
			outRoot := filepath.Join(t.TempDir(), "out")
			mk := func(t *testing.T, dry bool) *Remuxer {
				rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
				rm.DryRun, rm.InPlace = dry, inPlace
				if !inPlace {
					rm.OutputRoot = outRoot
				}
				return rm
			}
			destOf := func(dir string) string {
				if inPlace {
					return filepath.Join(dir, "ep.mkv")
				}
				return filepath.Join(outRoot, "ep.mkv")
			}
			check := func(t *testing.T, res []report.FileResult, dest string) report.FileResult {
				t.Helper()
				if len(res) != 2 {
					t.Fatalf("results %d", len(res))
				}
				mov, webm := byBase(res)["ep.mov"], byBase(res)["ep.webm"]
				if !mov.Has(CodeDryRun) || mov.Output != dest || mov.Has(CodeOutputExists) {
					t.Errorf("ep.mov: %v %q", codes(mov), mov.Output)
				}
				f, ok := finding(webm, CodeOutputExists)
				if !ok || f.Severity != report.Fail || f.Message != dest+" already exists" {
					t.Errorf("ep.webm: %v %q", codes(webm), f.Message)
				}
				if webm.Verdict != report.Fail || webm.Has(CodeDryRun) || webm.Output != "" {
					t.Errorf("ep.webm: %s %v %q", webm.Verdict, codes(webm), webm.Output)
				}
				return webm
			}

			dir := collisionPair(t, "ep.mov", "ep.webm")
			before := map[string]string{"ep.mov": fileSHA(t, filepath.Join(dir, "ep.mov")), "ep.webm": fileSHA(t, filepath.Join(dir, "ep.webm"))}
			rm := mk(t, true)
			res, err := rm.RemuxPath(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			webm := check(t, res, destOf(dir))

			// The same Remuxer keeps its plan across RemuxPath calls: the
			// same two files as positional paths collide as well.
			rm = mk(t, true)
			var split []report.FileResult
			for _, name := range []string{"ep.mov", "ep.webm"} {
				one, err := rm.RemuxPath(context.Background(), filepath.Join(dir, name))
				if err != nil {
					t.Fatal(err)
				}
				split = append(split, one...)
			}
			check(t, split, destOf(dir))

			// A fresh Remuxer is a fresh run, so the first file plans again
			// instead of colliding with the earlier dry run.
			again, err := mk(t, true).RemuxPath(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			check(t, again, destOf(dir))

			for name, sum := range before {
				if fileSHA(t, filepath.Join(dir, name)) != sum {
					t.Errorf("%s changed", name)
				}
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 2 {
				t.Errorf("input dir now holds %d entries", len(entries))
			}
			if _, err := os.Lstat(outRoot); err == nil {
				t.Error("dry run created the output root")
			}
			if l := leftovers(t, dir); len(l) != 0 {
				t.Errorf("temp files left: %v", l)
			}

			// The live run gives the second file the same findings.
			liveDir := collisionPair(t, "ep.mov", "ep.webm")
			live, err := mk(t, false).RemuxPath(context.Background(), liveDir)
			if err != nil {
				t.Fatal(err)
			}
			liveWebm := byBase(live)["ep.webm"]
			if f, ok := finding(liveWebm, CodeOutputExists); !ok || f.Message != destOf(liveDir)+" already exists" {
				t.Fatalf("live ep.webm: %v", codes(liveWebm))
			}
			if strings.Join(codes(liveWebm), ",") != strings.Join(codes(webm), ",") {
				t.Errorf("collision findings differ: dry %v, live %v", codes(webm), codes(liveWebm))
			}
			if _, err := os.Lstat(destOf(liveDir)); err != nil {
				t.Errorf("live run did not place %s: %v", destOf(liveDir), err)
			}
		})
	}
}

// On a case-insensitive filesystem Ep.mov and ep.webm rebuild to one entry,
// so the dry run must report the second with OUTPUT_EXISTS just as the
// live run does, although the two planned names differ in case. That
// holds for an output root the dry run has not created, whose filesystem
// is the one of its nearest existing ancestor.
func TestDryRunCollisionFoldsCase(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	if !caseInsensitiveDir(t, t.TempDir()) {
		t.Skip("the temp directory's filesystem is case-sensitive; see TestDryRunCollisionKeepsCase")
	}
	for _, inPlace := range []bool{false, true} {
		mode := "output"
		if inPlace {
			mode = "in place"
		}
		t.Run(mode, func(t *testing.T) {
			outRoot := filepath.Join(t.TempDir(), "out")
			mk := func(t *testing.T, dry bool) *Remuxer {
				rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
				rm.DryRun, rm.InPlace = dry, inPlace
				if !inPlace {
					rm.OutputRoot = outRoot
				}
				return rm
			}
			destDir := func(dir string) string {
				if inPlace {
					return dir
				}
				return outRoot
			}
			dir := collisionPair(t, "Ep.mov", "ep.webm")
			res, err := mk(t, true).RemuxPath(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 2 {
				t.Fatalf("results %d", len(res))
			}
			mov, webm := byBase(res)["Ep.mov"], byBase(res)["ep.webm"]
			if !mov.Has(CodeDryRun) || mov.Output != filepath.Join(destDir(dir), "Ep.mkv") {
				t.Errorf("Ep.mov: %v %q", codes(mov), mov.Output)
			}
			want := filepath.Join(destDir(dir), "ep.mkv") + " already exists"
			if f, ok := finding(webm, CodeOutputExists); !ok || f.Severity != report.Fail || f.Message != want {
				t.Errorf("ep.webm: %v %q", codes(webm), f.Message)
			}
			if webm.Has(CodeDryRun) || webm.Output != "" || webm.Verdict != report.Fail {
				t.Errorf("ep.webm: %s %v %q", webm.Verdict, codes(webm), webm.Output)
			}
			if _, err := os.Lstat(outRoot); err == nil {
				t.Error("dry run created the output root")
			}
			if entries, _ := os.ReadDir(dir); len(entries) != 2 {
				t.Errorf("input dir now holds %d entries", len(entries))
			}

			liveDir := collisionPair(t, "Ep.mov", "ep.webm")
			live, err := mk(t, false).RemuxPath(context.Background(), liveDir)
			if err != nil {
				t.Fatal(err)
			}
			liveWebm := byBase(live)["ep.webm"]
			if f, ok := finding(liveWebm, CodeOutputExists); !ok || f.Message != filepath.Join(destDir(liveDir), "ep.mkv")+" already exists" {
				t.Fatalf("live ep.webm: %v", codes(liveWebm))
			}
			if strings.Join(codes(liveWebm), ",") != strings.Join(codes(webm), ",") {
				t.Errorf("collision findings differ: dry %v, live %v", codes(webm), codes(liveWebm))
			}
		})
	}
}

// On a case-sensitive filesystem Ep.mkv and ep.mkv are two entries, so
// Ep.mov and ep.webm are two distinct plans and neither collides.
func TestDryRunCollisionKeepsCase(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVExtract)
	if caseInsensitiveDir(t, t.TempDir()) {
		t.Skip("the temp directory's filesystem folds case; see TestDryRunCollisionFoldsCase")
	}
	for _, inPlace := range []bool{false, true} {
		rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
		rm.DryRun, rm.InPlace = true, inPlace
		if !inPlace {
			rm.OutputRoot = filepath.Join(t.TempDir(), "out")
		}
		dir := collisionPair(t, "Ep.mov", "ep.webm")
		res, err := rm.RemuxPath(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("results %d", len(res))
		}
		for _, fr := range res {
			if !fr.Has(CodeDryRun) || fr.Has(CodeOutputExists) || fr.Output == "" {
				t.Errorf("inPlace=%v %s: %v %q", inPlace, filepath.Base(fr.Path), codes(fr), fr.Output)
			}
		}
		if res[0].Output == res[1].Output {
			t.Errorf("inPlace=%v: both plan %s", inPlace, res[0].Output)
		}
	}
}

// The planned-name comparison itself, with the per-directory case answer
// seeded so both branches run whatever filesystem hosts the tests: names
// that differ only in case collide when the directory folds case and are
// distinct when it does not, other directories never collide, and
// RemuxPath does not clear the plan. Nothing here touches the disk.
func TestPlannedCollisionComparesPerDirectory(t *testing.T) {
	fold := filepath.Join(string(filepath.Separator), "fold")
	exact := filepath.Join(string(filepath.Separator), "exact")
	rm := &Remuxer{caseFold: map[string]bool{fold: true, exact: false}}
	for _, dir := range []string{fold, exact} {
		if rm.plannedCollision(filepath.Join(dir, "ep.mkv")) {
			t.Errorf("%s: collision before anything was planned", dir)
		}
		rm.plan(filepath.Join(dir, "Ep.mkv"))
		if !rm.plannedCollision(filepath.Join(dir, "Ep.mkv")) {
			t.Errorf("%s: the exact name does not collide", dir)
		}
		if rm.plannedCollision(filepath.Join(dir, "other.mkv")) {
			t.Errorf("%s: a different name collides", dir)
		}
	}
	if !rm.plannedCollision(filepath.Join(fold, "ep.mkv")) || !rm.plannedCollision(filepath.Join(fold, "EP.MKV")) {
		t.Error("a folding directory does not fold case")
	}
	if rm.plannedCollision(filepath.Join(exact, "ep.mkv")) || rm.plannedCollision(filepath.Join(exact, "EP.MKV")) {
		t.Error("an exact directory folds case")
	}
	if rm.plannedCollision(filepath.Join(fold, "sub", "Ep.mkv")) {
		t.Error("a plan leaks into a subdirectory")
	}
	// A dry run over a missing path fails before it could reset the plan,
	// and the plan of the earlier call is still there afterwards.
	rm.DryRun = true
	if _, err := rm.RemuxPath(context.Background(), filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("RemuxPath of a missing path succeeded")
	}
	if !rm.plannedCollision(filepath.Join(fold, "ep.mkv")) {
		t.Error("RemuxPath cleared the plan")
	}
}

// foldsCase agrees with a probe that writes a file, for an existing
// directory and for a directory that does not exist yet, and answers
// case-sensitive at the filesystem root where nothing can be probed.
func TestFoldsCaseDetection(t *testing.T) {
	dir := t.TempDir()
	want := caseInsensitiveDir(t, dir)
	if got := foldsCase(dir); got != want {
		t.Errorf("foldsCase(%s) = %v, probe says %v", dir, got, want)
	}
	missing := filepath.Join(dir, "out", "Season 1")
	if got := foldsCase(missing); got != want {
		t.Errorf("foldsCase(%s) = %v, probe says %v", missing, got, want)
	}
	if foldsCase(string(filepath.Separator)) {
		t.Error("the root folds case")
	}
	rm := &Remuxer{}
	if a, b := rm.foldsCase(dir), rm.foldsCase(dir); a != want || b != want || rm.caseFold[dir] != want {
		t.Errorf("cached answer %v %v %v, want %v", a, b, rm.caseFold[dir], want)
	}
	for in, out := range map[string]string{"Ep.mkv": "eP.MKV", "123": "", "": "", "üï": "", "ünï": "üNï"} {
		if got := flipCase(in); got != out {
			t.Errorf("flipCase(%q) = %q, want %q", in, got, out)
		}
	}
}
