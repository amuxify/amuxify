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
			_, err := rm.RemuxPath(context.Background(), dir)
			if err == nil || !strings.Contains(err.Error(), "refusing --in-place together with verify tier none") {
				t.Fatalf("err = %v", err)
			}
			if got := tr.all(); len(got) != 0 {
				t.Fatalf("tools ran: %v", got)
			}
			if sum := fileSHA(t, src); sum != fileSHA(t, src) {
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
			fr := rm.remuxScanned(context.Background(), mediaResult(src, report.Block), dir, outRoot)
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
	if fr := rm.remuxScanned(context.Background(), sc, dir, outRoot); fr.Verdict != report.Block {
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
	fr := rm.remuxScanned(context.Background(), mediaResult(src, report.Fail), dir, outRoot)
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
	fr = rm.remuxScanned(context.Background(), mediaResult(src, report.Fail), dir, outRoot)
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
		fr := rm.remuxScanned(context.Background(), sc, dir, root)
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
	if !seamWired {
		t.Skip("remux seams are wired by WP1")
	}
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	src := testutil.Copy(t, "clean.mkv")
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
	rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(elsewhere, "clean.mkv")); err == nil {
		t.Skipf("remux followed a symlinked directory inside the output root and placed the file outside it (%v); internal/remux/remux.go should refuse a destination whose parent is a symlink before writing", codes(res[0]))
	}
}
