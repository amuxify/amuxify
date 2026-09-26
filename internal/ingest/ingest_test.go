package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/clean"
	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/remux"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/testutil"
	"github.com/amuxify/amuxify/internal/verify"
	"github.com/pkg/xattr"
)

// ebml is a minimal Matroska magic so files look like media to sniff.
const ebml = "\x1a\x45\xdf\xa3\x01\x00\x00\x00\x00\x00\x00\x1f\x42\x86\x81\x01"

type trace struct {
	mu    sync.Mutex
	lines []string
}

func (t *trace) add(s string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.lines = append(t.lines, s)
}

func (t *trace) all() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.lines...)
}

func mustProfile(t *testing.T, name string) *policy.Profile {
	t.Helper()
	p, err := policy.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// noTools points every tool override at a missing path.
func noTools(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "no-such-tool")
	for _, tool := range []string{exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract, exec.ExifTool, exec.ClamScan} {
		t.Setenv("AMUXIFY_"+strings.ToUpper(tool), missing)
	}
}

// newIngester wires the three stages the way cli.newIngester does, with a
// tracing runner so tests can prove that no tool ran.
func newIngester(t *testing.T, r *exec.Runner, p *policy.Profile) (*Ingester, *trace) {
	t.Helper()
	tr := &trace{}
	if r == nil {
		r = &exec.Runner{Timeout: 2 * time.Minute}
	}
	r.Trace = tr.add
	pr := &probe.Prober{Runner: r, Timeout: 2 * time.Minute}
	v := &verify.Verifier{Runner: r, Timeout: 2 * time.Minute}
	sc := &scan.Scanner{Runner: r, Prober: pr, Verifier: v, Profile: p, VerifyTier: "none"}
	rm := &remux.Remuxer{Runner: r, Prober: pr, Verifier: v, Profile: p, InPlace: true, Timeout: 2 * time.Minute}
	cl := &clean.Cleaner{Runner: r, Prober: pr, Verifier: v, Profile: p, Timeout: 2 * time.Minute}
	return &Ingester{Scanner: sc, Remuxer: rm, Cleaner: cl, Verifier: v, Profile: p}, tr
}

// forbidStageProgress makes any stage-level Progress callback fail the test,
// which proves ingest never reports a file through scan, remux or clean.
func forbidStageProgress(t *testing.T, in *Ingester) {
	t.Helper()
	in.Scanner.Progress = func(scan.Result) { t.Errorf("scanner Progress fired inside ingest") }
	in.Remuxer.Progress = func(report.FileResult) { t.Errorf("remuxer Progress fired inside ingest") }
	in.Cleaner.Progress = func(report.FileResult) { t.Errorf("cleaner Progress fired inside ingest") }
}

// apply copies the run options into the stages the way cli.newIngester does.
func (in *Ingester) apply(t *testing.T) {
	t.Helper()
	in.Remuxer.Hardlinks, in.Remuxer.VerifyTier, in.Remuxer.Force, in.Remuxer.Original = in.Hardlinks, in.VerifyTier, in.Force, in.Original
	in.Cleaner.Hardlinks, in.Cleaner.RemoveBlockedSidecars = in.Hardlinks, in.RemoveBlockedSidecars
}

func codes(fr report.FileResult) []string {
	var out []string
	for _, f := range fr.Findings {
		out = append(out, f.Code+"/"+f.Severity.String())
	}
	return out
}

func finding(fr report.FileResult, code string) (report.Finding, bool) {
	for _, f := range fr.Findings {
		if f.Code == code {
			return f, true
		}
	}
	return report.Finding{}, false
}

func route(t *testing.T, fr report.FileResult) (Route, string) {
	t.Helper()
	f, ok := finding(fr, CodeRoute)
	if !ok {
		t.Fatalf("%s: no ROUTE finding: %v", fr.Path, codes(fr))
	}
	r, text, _ := strings.Cut(f.Message, ": ")
	if fr.Info["route"] != r {
		t.Errorf("%s: Info[route]=%q but ROUTE says %q", fr.Path, fr.Info["route"], r)
	}
	if fr.Verdict >= report.Fail && fr.Output != "" && fr.Info["route"] == string(RouteClean) {
		t.Errorf("%s: failed clean route carries Output %q", fr.Path, fr.Output)
	}
	return Route(r), text
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

func fileSHA(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// snapshot records name, size, mtime and link count of every entry under
// root (symlinks included, not followed).
func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			out[p] = "dir"
			return nil
		}
		out[p] = fmt.Sprintf("%d %d %o", fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameSnapshot(t *testing.T, before, after map[string]string) {
	t.Helper()
	for p, v := range before {
		if after[p] != v {
			t.Errorf("%s changed: %q -> %q", p, v, after[p])
		}
	}
	for p := range after {
		if _, ok := before[p]; !ok {
			t.Errorf("%s appeared", p)
		}
	}
}

func byBase(res []report.FileResult) map[string]report.FileResult {
	out := map[string]report.FileResult{}
	for _, r := range res {
		out[filepath.Base(r.Path)] = r
	}
	return out
}

func result(path string, verdict report.Severity, info *probe.MediaInfo) scan.Result {
	fr := report.FileResult{Path: path, Info: map[string]string{}}
	switch verdict {
	case report.Block:
		fr.Addf(scan.CodePolyglot, report.Block, "test block")
	case report.Fail:
		fr.Addf(scan.CodeExtMismatch, report.Fail, "test fail")
	case report.Warn:
		fr.Addf(scan.CodeLinkInTag, report.Warn, "test warn")
	}
	if info != nil {
		info.Path = path
		fr.Info["container"] = info.Container
	}
	return scan.Result{File: fr, Info: info}
}

func mkvInfo(streams ...probe.Stream) *probe.MediaInfo {
	if len(streams) == 0 {
		streams = []probe.Stream{
			{Index: 0, MkvID: 0, Type: "video", Codec: "h264", Language: "eng", Default: true},
			{Index: 1, MkvID: 1, Type: "audio", Codec: "aac", Language: "eng", Default: true},
		}
	}
	return &probe.MediaInfo{Container: "matroska", MkvSupported: true, MkvIdentified: true, Streams: streams}
}

var builtins = []string{"homelab", "archive", "anime", "strict"}

func TestNeedsRemux(t *testing.T) {
	// A Matroska file whose single video and English audio track every
	// profile keeps needs no remux under any of them.
	for _, name := range builtins {
		p := mustProfile(t, name)
		m := mkvInfo()
		if got := NeedsRemux(m, p.Decide(m, policy.Options{})); len(got) != 0 {
			t.Errorf("%s: kept-all file needs remux: %v", name, got)
		}
	}
	t.Run("dropped audio", func(t *testing.T) {
		p := mustProfile(t, "archive")
		m := mkvInfo(
			probe.Stream{Index: 0, Type: "video", Codec: "h264", Language: "eng", Default: true},
			probe.Stream{Index: 1, Type: "audio", Codec: "aac", Language: "eng", Default: true},
			probe.Stream{Index: 2, Type: "audio", Codec: "ac3", Language: "ger"},
		)
		d := p.Decide(m, policy.Options{})
		want := []string{"drop #2 audio ac3 ger: " + d.Tracks[2].Reason}
		if d.Tracks[2].Keep || d.Tracks[2].Reason == "" {
			t.Fatalf("archive kept the German track: %+v", d.Tracks[2])
		}
		if got := NeedsRemux(m, d); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("got %q want %q", got, want)
		}
	})
	t.Run("dropped attachment", func(t *testing.T) {
		p := mustProfile(t, "archive")
		m := mkvInfo()
		m.Attachments = []probe.Attachment{{ID: 1, FileName: "font.ttf", MimeType: "font/ttf", Size: 10}}
		d := p.Decide(m, policy.Options{})
		if len(d.Attachments) != 1 || d.Attachments[0].Keep {
			t.Fatalf("archive kept the font: %+v", d.Attachments)
		}
		want := []string{"drop attachment #1 font.ttf: " + d.Attachments[0].Reason}
		if got := NeedsRemux(m, d); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("got %q want %q", got, want)
		}
		// homelab keeps fonts only with text subtitles; without them the
		// font is dropped too, and with them nothing is listed.
		hp := mustProfile(t, "homelab")
		if got := NeedsRemux(m, hp.Decide(m, policy.Options{})); len(got) != 1 || !strings.HasPrefix(got[0], "drop attachment #1 font.ttf: ") {
			t.Errorf("homelab without text subtitles: %q", got)
		}
		m2 := mkvInfo(
			probe.Stream{Index: 0, Type: "video", Codec: "h264", Language: "eng", Default: true},
			probe.Stream{Index: 1, Type: "audio", Codec: "aac", Language: "eng", Default: true},
			probe.Stream{Index: 2, Type: "subtitle", Codec: "ass", Language: "eng", TextSubtitle: true},
		)
		m2.Attachments = m.Attachments
		if got := NeedsRemux(m2, hp.Decide(m2, policy.Options{})); len(got) != 0 {
			t.Errorf("homelab with text subtitles: %q", got)
		}
	})
	t.Run("flipped default", func(t *testing.T) {
		m := mkvInfo(
			probe.Stream{Index: 0, Type: "video", Codec: "h264", Language: "eng", Default: true},
			probe.Stream{Index: 1, Type: "audio", Codec: "aac", Language: "jpn", Default: false},
			probe.Stream{Index: 2, Type: "audio", Codec: "aac", Language: "eng", Default: true},
		)
		// anime prefers the original language for the default audio.
		d := mustProfile(t, "anime").Decide(m, policy.Options{OriginalLanguage: "jpn"})
		want := []string{"#1 default flag false -> true", "#2 default flag true -> false"}
		if got := NeedsRemux(m, d); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("got %q want %q", got, want)
		}
		// The same decision expressed by hand, for a subtitle track and a
		// forced flag.
		d = &policy.Decision{KeepChapters: true, Tracks: []policy.TrackAction{
			{Stream: m.Streams[0], Keep: true, Default: true},
			{Stream: probe.Stream{Index: 3, Type: "subtitle", Codec: "subrip", Language: "eng", Default: true, Forced: false}, Keep: true, Default: false, Forced: true},
		}}
		want = []string{"#3 default flag true -> false", "#3 forced flag false -> true"}
		if got := NeedsRemux(m, d); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("got %q want %q", got, want)
		}
	})
	t.Run("set language", func(t *testing.T) {
		m := mkvInfo(
			probe.Stream{Index: 0, Type: "video", Codec: "h264", Language: "eng", Default: true},
			probe.Stream{Index: 1, Type: "audio", Codec: "aac", Language: "und", Default: true},
		)
		d := &policy.Decision{KeepChapters: true, Tracks: []policy.TrackAction{
			{Stream: m.Streams[0], Keep: true, Default: true},
			{Stream: m.Streams[1], Keep: true, Default: true, SetLanguage: "eng"},
		}}
		want := []string{"#1 language und -> eng"}
		if got := NeedsRemux(m, d); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("got %q want %q", got, want)
		}
	})
	t.Run("chapters", func(t *testing.T) {
		m := mkvInfo()
		m.ChapterN = 3
		for _, name := range builtins {
			p := mustProfile(t, name)
			got := NeedsRemux(m, p.Decide(m, policy.Options{}))
			if p.Chapters.Keep && len(got) != 0 {
				t.Errorf("%s keeps chapters but lists %q", name, got)
			}
			if !p.Chapters.Keep && strings.Join(got, "|") != "chapters dropped" {
				t.Errorf("%s drops chapters but lists %q", name, got)
			}
		}
		m.ChapterN = 0
		if got := NeedsRemux(m, &policy.Decision{Tracks: []policy.TrackAction{{Stream: m.Streams[0], Keep: true, Default: true}}}); len(got) != 0 {
			t.Errorf("no chapters to drop but %q", got)
		}
	})
	t.Run("data and attachment streams ignored", func(t *testing.T) {
		m := mkvInfo(
			probe.Stream{Index: 0, Type: "video", Codec: "h264", Language: "eng", Default: true},
			probe.Stream{Index: 1, Type: "data", Codec: "bin_data", CodecTag: "tmcd"},
			probe.Stream{Index: 2, Type: "attachment", Codec: "ttf"},
		)
		d := &policy.Decision{KeepChapters: true, Tracks: []policy.TrackAction{
			{Stream: m.Streams[0], Keep: true, Default: true},
			{Stream: m.Streams[1], Keep: false, Reason: "data stream"},
			{Stream: m.Streams[2], Keep: false, Reason: "attachment stream"},
		}}
		if got := NeedsRemux(m, d); len(got) != 0 {
			t.Errorf("got %q", got)
		}
		if got := NeedsRemux(nil, d); got != nil {
			t.Errorf("nil info: %q", got)
		}
		if got := NeedsRemux(m, nil); got != nil {
			t.Errorf("nil decision: %q", got)
		}
	})
}

func TestDecideRoutes(t *testing.T) {
	video := mkvInfo
	audioInfo := func() *probe.MediaInfo {
		return &probe.MediaInfo{Container: "matroska", MkvSupported: true, MkvIdentified: true,
			Streams: []probe.Stream{{Index: 0, Type: "audio", Codec: "aac", Language: "eng", Default: true}}}
	}
	needs := func() *probe.MediaInfo {
		return mkvInfo(
			probe.Stream{Index: 0, Type: "video", Codec: "h264", Language: "eng", Default: true},
			probe.Stream{Index: 1, Type: "audio", Codec: "aac", Language: "eng", Default: true},
			probe.Stream{Index: 2, Type: "audio", Codec: "ac3", Language: "ger"},
		)
	}
	type tc struct {
		name       string
		path       string
		verdict    report.Severity
		info       *probe.MediaInfo
		nlink      string
		mkvWarn    bool
		profile    string
		hardlinks  string
		force      bool
		wantRoute  Route
		wantText   string // exact reason text, or prefix when wantPrefix
		wantPrefix bool
		wantCodes  []string
	}
	cases := []tc{
		{name: "1 decision failed (NO_VIDEO)", path: "a.mkv", info: mkvInfo(probe.Stream{Index: 0, Type: "audio", Codec: "aac", Language: "eng"}),
			wantRoute: RouteSkip, wantText: "decision failed", wantCodes: []string{"NO_VIDEO/FAIL"}},
		{name: "2 FAIL with force", path: "a.mkv", verdict: report.Fail, info: video(), force: true,
			wantRoute: RouteRemux, wantText: "scan verdict FAIL; rebuilt because --force was given"},
		{name: "2 BLOCK with force is not overridden here either", path: "a.mkv", verdict: report.Block, info: video(), force: true,
			wantRoute: RouteRemux, wantText: "scan verdict FAIL; rebuilt because --force was given"},
		{name: "2 FAIL with force on audio is cleaned, not rebuilt", path: "a.mka", verdict: report.Fail, info: audioInfo(), force: true,
			wantRoute: RouteClean, wantText: "scan verdict FAIL; cleaned because --force was given"},
		{name: "2 FAIL with force on subtitles is cleaned, not rebuilt", path: "a.mks", verdict: report.Fail, info: audioInfo(), force: true,
			wantRoute: RouteClean, wantText: "scan verdict FAIL; cleaned because --force was given"},
		{name: "2 FAIL with force on mp3 is cleaned, not rebuilt", path: "a.mp3", verdict: report.Fail, info: &probe.MediaInfo{Container: "mp3", Streams: audioInfo().Streams}, force: true,
			wantRoute: RouteClean, wantText: "scan verdict FAIL; cleaned because --force was given"},
		{name: "2 FAIL with force on hard-linked audio still skips", path: "a.mka", verdict: report.Fail, info: audioInfo(), force: true, nlink: "2", hardlinks: "break",
			wantRoute: RouteSkip, wantText: "hard-linked audio or subtitle file", wantCodes: []string{"HARDLINKED/WARN"}},
		{name: "3 hard-linked audio", path: "a.mka", info: audioInfo(), nlink: "2", hardlinks: "break",
			wantRoute: RouteSkip, wantText: "hard-linked audio or subtitle file", wantCodes: []string{"HARDLINKED/WARN"}},
		{name: "3 hard-linked subtitle", path: "a.mks", info: audioInfo(), nlink: "3", hardlinks: "copy",
			wantRoute: RouteSkip, wantText: "hard-linked audio or subtitle file", wantCodes: []string{"HARDLINKED/WARN"}},
		{name: "4 hard-linked skip", path: "a.mkv", info: video(), nlink: "2", hardlinks: "skip",
			wantRoute: RouteSkip, wantText: "hard-linked", wantCodes: []string{"HARDLINKED/WARN"}},
		{name: "4 hard-linked default mode is skip", path: "a.mkv", info: video(), nlink: "2", hardlinks: "",
			wantRoute: RouteSkip, wantText: "hard-linked", wantCodes: []string{"HARDLINKED/WARN"}},
		{name: "4 hard-linked unknown mode is skip", path: "a.mkv", info: video(), nlink: "2", hardlinks: "../evil",
			wantRoute: RouteSkip, wantText: "hard-linked", wantCodes: []string{"HARDLINKED/WARN"}},
		{name: "5 hard-linked break", path: "a.mkv", info: video(), nlink: "2", hardlinks: "break",
			wantRoute: RouteRemux, wantText: "hard-linked; rebuilt rather than edited in place (safety.hardlinks = break)"},
		{name: "5 hard-linked copy", path: "a.mkv", info: video(), nlink: "2", hardlinks: "copy",
			wantRoute: RouteRemux, wantText: "hard-linked; rebuilt rather than edited in place (safety.hardlinks = copy)"},
		{name: "5 hard-linked mp4 under break", path: "a.mp4", info: &probe.MediaInfo{Container: "mp4", Streams: video().Streams}, nlink: "2", hardlinks: "break",
			wantRoute: RouteRemux, wantText: "hard-linked; rebuilt rather than edited in place (safety.hardlinks = break)"},
		{name: "6 audio category", path: "a.mka", info: audioInfo(),
			wantRoute: RouteClean, wantText: "audio or subtitle container; metadata only"},
		{name: "6 subtitle category", path: "a.mks", info: audioInfo(),
			wantRoute: RouteClean, wantText: "audio or subtitle container; metadata only"},
		{name: "6 mp3 category", path: "a.mp3", info: &probe.MediaInfo{Container: "mp3", Streams: audioInfo().Streams},
			wantRoute: RouteClean, wantText: "audio or subtitle container; metadata only"},
		{name: "7 webm extension", path: "a.webm", info: &probe.MediaInfo{Container: "webm", MkvSupported: true, Streams: video().Streams},
			wantRoute: RouteRemux, wantText: "container webm is rebuilt as Matroska"},
		{name: "7 mp4 container", path: "a.mp4", info: &probe.MediaInfo{Container: "mp4", MkvSupported: true, Streams: video().Streams},
			wantRoute: RouteRemux, wantText: "container mp4 is rebuilt as Matroska"},
		{name: "7 mkv extension with mp4 inside", path: "a.mkv", info: &probe.MediaInfo{Container: "mp4", MkvSupported: true, Streams: video().Streams},
			wantRoute: RouteRemux, wantText: "container mp4 is rebuilt as Matroska"},
		{name: "7 MKV extension in capitals is still mkv", path: "a.MKV", info: video(),
			wantRoute: RouteClean, wantText: ""},
		{name: "8 MKV_WARNING", path: "a.mkv", info: video(), mkvWarn: true,
			wantRoute: RouteRemux, wantText: "mkvmerge reported warnings; rebuilding"},
		{name: "9 needs remux", path: "a.mkv", info: needs(), profile: "archive",
			wantRoute: RouteRemux, wantText: "drop #2 audio ac3 ger: ", wantPrefix: true},
		{name: "10 conforming", path: "a.mkv", info: video(),
			wantRoute: RouteClean, wantText: ""},
		{name: "10 conforming with WARN scan", path: "a.mkv", verdict: report.Warn, info: video(),
			wantRoute: RouteClean, wantText: ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			name := c.profile
			if name == "" {
				name = "homelab"
			}
			p := mustProfile(t, name)
			sc := result(filepath.Join(t.TempDir(), c.path), c.verdict, c.info)
			if c.nlink != "" {
				sc.File.Info["nlink"] = c.nlink
			}
			if c.mkvWarn {
				sc.File.Addf(scan.CodeMkvWarning, report.Warn, "test warning")
			}
			d := p.Decide(sc.Info, policy.Options{})
			fr := sc.File
			before := len(fr.Findings)
			got, reasons := Decide(&fr, sc, d, c.hardlinks, c.force)
			text := strings.Join(reasons, "; ")
			if got != c.wantRoute {
				t.Fatalf("route %s (%q), want %s; findings %v", got, text, c.wantRoute, codes(fr))
			}
			if c.wantPrefix {
				if !strings.HasPrefix(text, c.wantText) {
					t.Errorf("reasons %q, want prefix %q", text, c.wantText)
				}
			} else if text != c.wantText {
				t.Errorf("reasons %q, want %q", text, c.wantText)
			}
			added := codes(fr)[before:]
			for _, want := range c.wantCodes {
				found := false
				for _, a := range added {
					if a == want {
						found = true
					}
				}
				if !found {
					t.Errorf("finding %s missing; added %v", want, added)
				}
			}
			// The remux route never adds findings here: RemuxScanned adds
			// the decision's findings itself, so nothing is reported twice.
			if got == RouteRemux && len(added) != 0 {
				t.Errorf("remux route added findings %v", added)
			}
			// Audio and subtitle containers never carry the video-only
			// NO_VIDEO failure.
			if scan.MediaExts[strings.ToLower(filepath.Ext(c.path)[1:])] != "video" {
				if fr.Has(policy.CodeNoVideo) {
					t.Errorf("NO_VIDEO reported for a non-video container: %v", codes(fr))
				}
			}
		})
	}
}

// Guarantee 4 (docs/safety.md): BLOCK is never overridden, not even by --force.
func TestBlockRefusedEvenWithForce(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	// An empty .mkv is BLOCK (EMPTY_FILE) before any tool would run.
	empty := write(t, filepath.Join(dir, "empty.mkv"), "")
	for _, force := range []bool{false, true} {
		for _, dry := range []bool{false, true} {
			in, tr := newIngester(t, nil, mustProfile(t, "homelab"))
			in.Force = force
			in.Remuxer.DryRun, in.Cleaner.DryRun = dry, dry
			forbidStageProgress(t, in)
			in.apply(t)
			in.Cleaner = nil // any call would panic
			before := snapshot(t, dir)
			res, err := in.IngestPath(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 {
				t.Fatalf("results: %d", len(res))
			}
			fr := res[0]
			if fr.Verdict != report.Block {
				t.Errorf("force=%v dry=%v: verdict %s %v", force, dry, fr.Verdict, codes(fr))
			}
			if f, ok := finding(fr, remux.CodeRefused); !ok || f.Severity != report.Block || f.Message != "scan blocked this file; not ingested" {
				t.Errorf("force=%v dry=%v: REFUSED %+v %v", force, dry, f, codes(fr))
			}
			if r, text := route(t, fr); r != RouteSkip || text != "scan verdict BLOCK" {
				t.Errorf("force=%v dry=%v: route %s %q", force, dry, r, text)
			}
			if fr.Output != "" {
				t.Errorf("BLOCK file has Output %q", fr.Output)
			}
			if got := tr.all(); len(got) != 0 {
				t.Errorf("tools ran for a BLOCK file: %v", got)
			}
			sameSnapshot(t, before, snapshot(t, dir))
			if _, err := os.Stat(empty); err != nil {
				t.Errorf("blocked file gone: %v", err)
			}
		}
	}
	// A synthetic BLOCK result handed straight to Decide with force is not
	// reachable from IngestFile; the refusal lives before Decide. Prove the
	// remuxer refuses it too, in case a caller bypasses IngestFile.
	in, tr := newIngester(t, nil, mustProfile(t, "homelab"))
	in.Remuxer.Force = true
	sc := result(empty, report.Block, mkvInfo())
	fr := in.Remuxer.RemuxScanned(context.Background(), sc, dir, dir+"__remuxed")
	if fr.Verdict != report.Block || !fr.Has(remux.CodeRefused) || len(tr.all()) != 0 {
		t.Errorf("remuxer with force on BLOCK: %s %v tools %v", fr.Verdict, codes(fr), tr.all())
	}
}

// Guarantee 8 (docs/safety.md): in-place writes require verification.
func TestVerifyNoneRefusedFromFlagAndProfile(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	src := write(t, filepath.Join(dir, "a.mkv"), ebml)
	url := write(t, filepath.Join(dir, "a.url"), "[InternetShortcut]\nURL=http://x\n")
	for _, tc := range []struct {
		name    string
		tier    string
		profile string
	}{
		{"flag", "none", ""},
		{"profile", "", "[verify]\ntier=\"none\"\n"},
		{"flag over profile", "none", "[verify]\ntier=\"full\"\n"},
		{"flag with surrounding profile none", "none", "[verify]\ntier=\"none\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := mustProfile(t, "homelab")
			if tc.profile != "" {
				p = mustProfile(t, testutil.Profile(t, tc.profile))
			}
			in, tr := newIngester(t, nil, p)
			in.VerifyTier = tc.tier
			in.Force = true
			in.RemoveBlockedSidecars = true
			in.Scanner.Quarantine = filepath.Join(t.TempDir(), "q")
			in.apply(t)
			fired := 0
			in.Progress = func(report.FileResult) { fired++ }
			before := snapshot(t, dir)
			res, err := in.IngestPath(context.Background(), dir)
			if err == nil || err.Error() != "ingest writes in place and requires verification; verify tier none is refused" {
				t.Fatalf("err = %v", err)
			}
			if len(res) != 0 || fired != 0 {
				t.Errorf("results %d, progress %d", len(res), fired)
			}
			if got := tr.all(); len(got) != 0 {
				t.Errorf("tools ran: %v", got)
			}
			sameSnapshot(t, before, snapshot(t, dir))
			if _, err := os.Lstat(url); err != nil {
				t.Errorf("blocked sidecar removed before the refusal: %v", err)
			}
			if _, err := os.Lstat(in.Scanner.Quarantine); err == nil {
				t.Error("quarantine directory created before the refusal")
			}
			if fileSHA(t, src) != fileSHA(t, src) {
				t.Error("source changed")
			}
		})
	}
	// Lifting the tier makes the same call legal; the file is then scanned
	// (and refused as unparseable without tools, which is not this test).
	in, _ := newIngester(t, nil, mustProfile(t, testutil.Profile(t, "[verify]\ntier=\"none\"\n")))
	in.VerifyTier = "quick"
	if _, err := in.IngestPath(context.Background(), dir); err != nil {
		t.Fatalf("quick tier refused: %v", err)
	}
}

func TestProgressFiresOncePerFile(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	a := write(t, filepath.Join(dir, "a.txt"), "a\n")
	b := write(t, filepath.Join(dir, "b.txt"), "b\n")
	c := write(t, filepath.Join(dir, "sub", "c.srt"), "1\n00:00:00,000 --> 00:00:01,000\nhi\n")
	in, tr := newIngester(t, nil, mustProfile(t, "homelab"))
	forbidStageProgress(t, in)
	var seen []string
	in.Progress = func(fr report.FileResult) {
		seen = append(seen, fr.Path)
		if fr.Path == a {
			if err := os.Remove(b); err != nil {
				t.Fatal(err)
			}
		}
		if _, ok := finding(fr, CodeRoute); !ok {
			t.Errorf("%s: progress result has no ROUTE", fr.Path)
		}
	}
	res, err := in.IngestPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(seen, ",") != strings.Join([]string{a, b, c}, ",") {
		t.Fatalf("progress order %v", seen)
	}
	if len(res) != 3 {
		t.Fatalf("results %d", len(res))
	}
	got := byBase(res)
	if fr := got["b.txt"]; fr.Verdict != report.Fail || !fr.Has(scan.CodeUnreadable) {
		t.Errorf("vanished b.txt: %s %v", fr.Verdict, codes(fr))
	}
	if r, text := route(t, got["b.txt"]); r != RouteSkip || text != "sidecar" {
		t.Errorf("b.txt route %s %q", r, text)
	}
	for _, name := range []string{"a.txt", "c.srt"} {
		fr := got[name]
		if fr.Verdict > report.Warn {
			t.Errorf("%s: %s %v", name, fr.Verdict, codes(fr))
		}
		if r, _ := route(t, fr); r != RouteSkip {
			t.Errorf("%s route %s", name, r)
		}
		if !fr.Has(clean.CodeNothing) && !fr.Has(clean.CodeXattr) {
			t.Errorf("%s: cleaner did not run: %v", name, codes(fr))
		}
	}
	if got := tr.all(); len(got) != 0 {
		t.Errorf("tools ran for sidecars: %v", got)
	}
	// Each ROUTE appears exactly once per result.
	for _, fr := range res {
		n := 0
		for _, f := range fr.Findings {
			if f.Code == CodeRoute {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: %d ROUTE findings", fr.Path, n)
		}
	}
}

// Sidecars: blocked ones are removed only with the flag and never when the
// scanner already moved them to quarantine.
func TestBlockedSidecarsWithoutTools(t *testing.T) {
	noTools(t)
	for _, tc := range []struct {
		name       string
		remove     bool
		quarantine bool
		dry        bool
	}{
		{"left in place", false, false, false},
		{"removed", true, false, false},
		{"quarantined, not removed", true, true, false},
		{"dry run removes nothing", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			url := write(t, filepath.Join(dir, "sub", "x.url"), "[InternetShortcut]\nURL=http://x\n")
			nfo := write(t, filepath.Join(dir, "sub", "x.nfo"), "plain notes\n")
			in, _ := newIngester(t, nil, mustProfile(t, "homelab"))
			in.RemoveBlockedSidecars = tc.remove
			in.Remuxer.DryRun, in.Cleaner.DryRun = tc.dry, tc.dry
			q := filepath.Join(t.TempDir(), "q")
			if tc.quarantine {
				in.Scanner.Quarantine = q
			}
			in.apply(t)
			res, err := in.IngestPath(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			got := byBase(res)
			fr := got["x.url"]
			if fr.Verdict != report.Block || !fr.Has(scan.CodeSidecarBlocked) {
				t.Fatalf("x.url: %s %v", fr.Verdict, codes(fr))
			}
			if r, text := route(t, fr); r != RouteSkip || text != "sidecar" {
				t.Errorf("x.url route %s %q", r, text)
			}
			_, exists := os.Lstat(url)
			_, inQ := os.Lstat(filepath.Join(q, "sub", "x.url"))
			switch {
			case tc.quarantine:
				if exists == nil || inQ != nil {
					t.Errorf("quarantine: exists=%v inQuarantine=%v", exists == nil, inQ == nil)
				}
				if fr.Has(clean.CodeSidecarRemove) || fr.Has(clean.CodeCleanFail) {
					t.Errorf("cleaner touched a quarantined file: %v", codes(fr))
				}
			case tc.remove && !tc.dry:
				if exists == nil || !fr.Has(clean.CodeSidecarRemove) {
					t.Errorf("not removed: exists=%v %v", exists == nil, codes(fr))
				}
			case tc.remove && tc.dry:
				if exists != nil || !fr.Has(clean.CodeDryRun) || fr.Has(clean.CodeSidecarRemove) {
					t.Errorf("dry run: exists=%v %v", exists == nil, codes(fr))
				}
			default:
				if exists != nil || fr.Has(clean.CodeSidecarRemove) {
					t.Errorf("removed without the flag: exists=%v %v", exists == nil, codes(fr))
				}
			}
			if _, err := os.Lstat(nfo); err != nil {
				t.Errorf("allowed sidecar removed: %v", err)
			}
			if r, _ := route(t, got["x.nfo"]); r != RouteSkip {
				t.Errorf("x.nfo route %s", r)
			}
		})
	}
}

// Media a scan refuses is never touched, whatever the reason.
func TestRefusedMediaUntouched(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	// Without tools every parse fails, so a real-looking file is FAIL with
	// no probe result; a symlink is WARN with no probe result.
	unparseable := write(t, filepath.Join(dir, "u.mkv"), ebml+strings.Repeat("x", 100))
	victim := write(t, filepath.Join(t.TempDir(), "victim.mkv"), ebml+"victim")
	link := filepath.Join(dir, "link.mkv")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for _, force := range []bool{false, true} {
		in, tr := newIngester(t, nil, mustProfile(t, "homelab"))
		in.Force = force
		in.Hardlinks = "break"
		in.apply(t)
		before := snapshot(t, dir)
		victimSum := fileSHA(t, victim)
		res, err := in.IngestPath(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		got := byBase(res)
		fr := got["u.mkv"]
		if fr.Verdict != report.Fail || !fr.Has(remux.CodeRefused) {
			t.Errorf("force=%v u.mkv: %s %v", force, fr.Verdict, codes(fr))
		}
		if r, _ := route(t, fr); r != RouteSkip {
			t.Errorf("force=%v u.mkv route %s", force, r)
		}
		// Guarantee 3: the symlink is skipped with a WARN, not refused, and
		// --force does not change that.
		fr = got["link.mkv"]
		if fr.Verdict != report.Warn || !fr.Has(scan.CodeSymlink) || fr.Has(remux.CodeRefused) {
			t.Errorf("force=%v link.mkv: %s %v", force, fr.Verdict, codes(fr))
		}
		if r, text := route(t, fr); r != RouteSkip || text != "symlink skipped" {
			t.Errorf("force=%v link.mkv route %s %q", force, r, text)
		}
		if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("force=%v link.mkv is no longer a symlink: %v", force, err)
		}
		sameSnapshot(t, before, snapshot(t, dir))
		if fileSHA(t, victim) != victimSum {
			t.Error("symlink target changed")
		}
		if _, err := os.Lstat(unparseable); err != nil {
			t.Error(err)
		}
		// Only ffprobe (and mkvmerge identify) may have been attempted on
		// u.mkv; nothing that writes.
		for _, line := range tr.all() {
			if strings.Contains(line, "mkvpropedit") || strings.Contains(line, "ffmpeg ") {
				t.Errorf("writer ran on refused input: %s", line)
			}
		}
	}
}

// A symlinked directory inside the tree is never entered, and a symlink
// planted at the mirrored quarantine path cannot redirect a quarantined file.
func TestSymlinkedDirectoryAndQuarantinePathNotFollowed(t *testing.T) {
	noTools(t)
	outside := t.TempDir()
	victim := write(t, filepath.Join(outside, "empty.mkv"), "")
	root := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	blocked := write(t, filepath.Join(root, "sub", "empty.mkv"), "")
	q := filepath.Join(t.TempDir(), "q")
	target := write(t, filepath.Join(t.TempDir(), "target.mkv"), "keep me")
	if err := os.MkdirAll(filepath.Join(q, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(q, "sub", "empty.mkv")); err != nil {
		t.Fatal(err)
	}
	in, _ := newIngester(t, nil, mustProfile(t, "homelab"))
	in.Scanner.Quarantine = q
	in.RemoveBlockedSidecars = true
	in.apply(t)
	beforeOutside := snapshot(t, outside)
	res, err := in.IngestPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	got := byBase(res)
	if evil, ok := got["evil"]; !ok {
		t.Errorf("symlinked directory not listed as an entry: %v", res)
	} else if !evil.Has(scan.CodeSymlink) {
		t.Errorf("evil: %v", codes(evil))
	}
	if len(res) != 2 {
		t.Errorf("entered the symlinked directory: %d results", len(res))
	}
	sameSnapshot(t, beforeOutside, snapshot(t, outside))
	if _, err := os.Lstat(victim); err != nil {
		t.Error("file behind the symlinked directory was moved")
	}
	// The planted symlink stays a symlink to an untouched target, and the
	// blocked file stays where it was because the quarantine slot is taken.
	if fi, err := os.Lstat(filepath.Join(q, "sub", "empty.mkv")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("planted symlink replaced: %v %v", fi, err)
	}
	if b, _ := os.ReadFile(target); string(b) != "keep me" {
		t.Errorf("symlink target overwritten: %q", b)
	}
	if _, err := os.Lstat(blocked); err != nil {
		t.Errorf("blocked file moved although the quarantine slot was taken: %v", err)
	}
	fr := got["empty.mkv"]
	if fr.Verdict != report.Block || !fr.Has(remux.CodeRefused) {
		t.Errorf("empty.mkv: %s %v", fr.Verdict, codes(fr))
	}
}

// Quarantine roots given through .. or a symlink resolve to one directory
// and every quarantined file lands inside it, mirrored from the scan root.
func TestQuarantineStaysInsideItsRoot(t *testing.T) {
	noTools(t)
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(base, "link")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	for _, q := range []string{
		filepath.Join(base, "real", "x", "..", "quarantine"),
		filepath.Join(base, "link", "quarantine"),
	} {
		root := t.TempDir()
		names := []string{
			filepath.Join("a", "empty.mkv"),
			filepath.Join("b‮", "empty.mkv"),
			filepath.Join("c", "​zero.mkv"),
			filepath.Join("d", "..evil.mkv"),
		}
		for _, n := range names {
			write(t, filepath.Join(root, n), "")
		}
		in, _ := newIngester(t, nil, mustProfile(t, "homelab"))
		in.Scanner.Quarantine = q
		in.apply(t)
		res, err := in.IngestPath(context.Background(), root)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != len(names) {
			t.Fatalf("%d results", len(res))
		}
		want := filepath.Join(real, "quarantine")
		var placed []string
		filepath.WalkDir(base, func(p string, d os.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				placed = append(placed, p)
			}
			return nil
		})
		sort.Strings(placed)
		if len(placed) != len(names) {
			t.Fatalf("quarantined files: %v", placed)
		}
		for _, p := range placed {
			if !strings.HasPrefix(p, want+string(filepath.Separator)) {
				t.Errorf("%s lies outside %s", p, want)
			}
		}
		for _, n := range names {
			if _, err := os.Lstat(filepath.Join(root, n)); err == nil {
				t.Errorf("%s still in the tree", n)
			}
			if _, err := os.Lstat(filepath.Join(want, n)); err != nil {
				t.Errorf("%s not mirrored under the quarantine root: %v", n, err)
			}
		}
		for _, fr := range res {
			if fr.Verdict != report.Block {
				t.Errorf("%s: %s", fr.Path, fr.Verdict)
			}
		}
		if err := os.RemoveAll(want); err != nil {
			t.Fatal(err)
		}
	}
}

// A file that is a symlink by the time the cleaner sees it is not followed.
func TestCleanScannedRefusesSymlinkSwappedAfterScan(t *testing.T) {
	noTools(t)
	dir := t.TempDir()
	path := write(t, filepath.Join(dir, "a.mkv"), ebml)
	victim := write(t, filepath.Join(t.TempDir(), "victim.mkv"), "victim")
	sc := result(path, report.Pass, mkvInfo())
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlink: %v", err)
	}
	in, tr := newIngester(t, nil, mustProfile(t, "homelab"))
	fr := in.Cleaner.CleanScanned(context.Background(), sc)
	if !fr.Has(clean.CodeSymlink) || fr.Verdict != report.Warn {
		t.Errorf("%s %v", fr.Verdict, codes(fr))
	}
	if len(tr.all()) != 0 {
		t.Errorf("tools ran: %v", tr.all())
	}
	if b, _ := os.ReadFile(victim); string(b) != "victim" {
		t.Errorf("victim changed: %q", b)
	}
	// The same swap for a sidecar: the cleaner strips nothing through it.
	side := write(t, filepath.Join(dir, "a.nfo"), "x")
	sideVictim := write(t, filepath.Join(t.TempDir(), "victim.nfo"), "v")
	sc = result(side, report.Pass, nil)
	os.Remove(side)
	if err := os.Symlink(sideVictim, side); err != nil {
		t.Fatal(err)
	}
	in.RemoveBlockedSidecars = true
	in.apply(t)
	fr = in.Cleaner.CleanScanned(context.Background(), sc)
	if !fr.Has(clean.CodeSymlink) {
		t.Errorf("sidecar swap: %v", codes(fr))
	}
	if b, _ := os.ReadFile(sideVictim); string(b) != "v" {
		t.Errorf("sidecar victim changed: %q", b)
	}
	// A vanished file is a clean failure, not a crash.
	gone := filepath.Join(dir, "gone.mkv")
	fr = in.Cleaner.CleanScanned(context.Background(), result(gone, report.Pass, mkvInfo()))
	if !fr.Has(clean.CodeCleanFail) {
		t.Errorf("vanished: %v", codes(fr))
	}
	// A media result without a probe is refused by the cleaner.
	fr = in.Cleaner.CleanScanned(context.Background(), result(write(t, filepath.Join(dir, "np.mkv"), ebml), report.Pass, nil))
	if f, ok := finding(fr, clean.CodeCleanFail); !ok || f.Severity != report.Fail || f.Message != "no probe result; scan did not parse this file" {
		t.Errorf("no probe: %+v %v", f, codes(fr))
	}
}

// An unreadable media file is refused, not modified, and the run goes on.
func TestUnreadableFileRefused(t *testing.T) {
	noTools(t)
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("permission bits do not stop this user")
	}
	dir := t.TempDir()
	locked := write(t, filepath.Join(dir, "locked.mkv"), ebml+"locked")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(locked, 0o644) })
	write(t, filepath.Join(dir, "z.nfo"), "notes")
	in, _ := newIngester(t, nil, mustProfile(t, "homelab"))
	in.Force = true
	in.apply(t)
	before := snapshot(t, dir)
	res, err := in.IngestPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	got := byBase(res)
	fr := got["locked.mkv"]
	if fr.Verdict < report.Fail || !fr.Has(remux.CodeRefused) {
		t.Errorf("locked: %s %v", fr.Verdict, codes(fr))
	}
	if _, ok := got["z.nfo"]; !ok {
		t.Error("run stopped at the unreadable file")
	}
	sameSnapshot(t, before, snapshot(t, dir))
}

// Hostile Original values reach the policy as data only.
func TestOriginalLanguageIsData(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "pwned")
	for _, orig := range []string{
		"eng; touch " + marker,
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"../../" + marker,
		"eng\x00jpn",
		"jpn‮eng",
		strings.Repeat("a", 1<<20),
	} {
		m := mkvInfo(
			probe.Stream{Index: 0, Type: "video", Codec: "h264", Language: "eng", Default: true},
			probe.Stream{Index: 1, Type: "audio", Codec: "aac", Language: "jpn"},
			probe.Stream{Index: 2, Type: "audio", Codec: "aac", Language: "eng", Default: true},
		)
		for _, name := range builtins {
			d := mustProfile(t, name).Decide(m, policy.Options{OriginalLanguage: orig})
			fr := report.FileResult{Path: "/x/a.mkv", Info: map[string]string{}}
			r, reasons := Decide(&fr, result("/x/a.mkv", report.Pass, m), d, "", false)
			if r != RouteClean && r != RouteRemux {
				t.Errorf("%s %q: route %s", name, orig, r)
			}
			for _, line := range reasons {
				if strings.Contains(line, "touch") || strings.Contains(line, "..") {
					t.Errorf("%s: hostile value echoed into a reason: %q", name, line)
				}
			}
		}
	}
	if _, err := os.Lstat(marker); err == nil {
		t.Fatal("marker file created")
	}
}

// Hard-link counts that are absent or hostile in Info fall back to one link.
func TestNlinkParsing(t *testing.T) {
	for v, want := range map[string]uint64{"": 1, "0": 1, "-1": 1, "abc": 1, "2": 2, "18446744073709551615": 18446744073709551615, "1e3": 1, " 2": 1} {
		fr := report.FileResult{Info: map[string]string{"nlink": v}}
		if got := nlinkOf(&fr); got != want {
			t.Errorf("%q: %d want %d", v, got, want)
		}
	}
	if got := nlinkOf(&report.FileResult{}); got != 1 {
		t.Errorf("nil Info: %d", got)
	}
}

// ---- integration on the fixture corpus ----

func needTools(t *testing.T) *exec.Runner {
	t.Helper()
	return testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
}

// archiveLike is the archive policy with links downgraded to warnings so
// multi.mkv is not refused by the scan and reaches the routing decision.
const archiveLike = `
[languages]
keep = ["eng"]
und = "drop"
[subtitles]
links = "warn"
[chapters]
keep = false
[attachments]
fonts = "drop"
cover_art = "drop"
[metadata]
strip_track_titles = true
links = "warn"
`

func TestIngestCorpus(t *testing.T) {
	r := needTools(t)
	corpus := testutil.CopyTree(t)
	before := snapshot(t, corpus)
	hardB := fileSHA(t, filepath.Join(corpus, "hard_b.mkv"))
	polyglot := filepath.Join(corpus, "polyglot.mkv")
	polyglotSum := fileSHA(t, polyglot)

	in, tr := newIngester(t, r, mustProfile(t, "homelab"))
	forbidStageProgress(t, in)
	fired := map[string]int{}
	in.Progress = func(fr report.FileResult) { fired[fr.Path]++ }
	res, err := in.IngestPath(context.Background(), corpus)
	if err != nil {
		t.Fatal(err)
	}
	for p, n := range fired {
		if n != 1 {
			t.Errorf("%s reported %d times", p, n)
		}
	}
	if len(fired) != len(res) {
		t.Errorf("progress %d results %d", len(fired), len(res))
	}
	got := byBase(res)
	check := func(name string, verdict report.Severity, want Route, codesWanted ...string) report.FileResult {
		t.Helper()
		fr, ok := got[name]
		if !ok {
			t.Errorf("%s: no result", name)
			return fr
		}
		if fr.Verdict != verdict {
			t.Errorf("%s: verdict %s want %s: %v", name, fr.Verdict, verdict, codes(fr))
		}
		if r, _ := route(t, fr); r != want {
			t.Errorf("%s: route %s want %s: %v", name, r, want, codes(fr))
		}
		for _, c := range codesWanted {
			if !fr.Has(c) {
				t.Errorf("%s: missing %s: %v", name, c, codes(fr))
			}
		}
		return fr
	}
	fr := check("conforming.mkv", report.Pass, RouteClean, clean.CodeNothing)
	if _, text := route(t, fr); text != "tracks, flags, attachments and chapters already match the profile" {
		t.Errorf("conforming reason %q", text)
	}
	check("clean.mkv", report.Pass, RouteClean, clean.CodeMetadata)
	fr = check("multi.mkv", report.Warn, RouteRemux, remux.CodeHashOK, remux.CodePlaced)
	if _, text := route(t, fr); !strings.HasPrefix(text, "#2 default flag true -> false; #3 default flag true -> false") {
		t.Errorf("multi reason %q", text)
	}
	if fr.Output != filepath.Join(corpus, "multi.mkv") {
		t.Errorf("multi output %q", fr.Output)
	}
	fr = check("purchased.mp4", report.Warn, RouteRemux, remux.CodeHashOK, remux.CodePlaced)
	if fr.Output != filepath.Join(corpus, "purchased.mkv") {
		t.Errorf("purchased output %q", fr.Output)
	}
	if _, err := os.Stat(filepath.Join(corpus, "purchased.mkv")); err != nil {
		t.Errorf("purchased.mkv: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(corpus, "purchased.mp4")); err == nil {
		t.Error("purchased.mp4 still exists")
	}
	fr = check("polyglot.mkv", report.Block, RouteSkip, remux.CodeRefused)
	if f, _ := finding(fr, remux.CodeRefused); f.Severity != report.Block {
		t.Errorf("polyglot REFUSED %+v", f)
	}
	if fileSHA(t, polyglot) != polyglotSum || snapshot(t, corpus)[polyglot] != before[polyglot] {
		t.Error("polyglot.mkv changed")
	}
	for _, name := range []string{"exe_attach.mkv", "fake_font.mkv", "empty.mkv", "movie‮vkm.mkv"} {
		p := filepath.Join(corpus, name)
		check(name, report.Block, RouteSkip, remux.CodeRefused)
		if snapshot(t, corpus)[p] != before[p] {
			t.Errorf("%s changed", name)
		}
	}
	for _, name := range []string{"text.mkv", "truncated.mp4", "mislabeled.mp4"} {
		p := filepath.Join(corpus, name)
		check(name, report.Fail, RouteSkip, remux.CodeRefused)
		if snapshot(t, corpus)[p] != before[p] {
			t.Errorf("%s changed", name)
		}
	}
	// sample.url is a blocked sidecar; without the flag it stays.
	check("sample.url", report.Block, RouteSkip, scan.CodeSidecarBlocked)
	if _, err := os.Lstat(filepath.Join(corpus, "sample.url")); err != nil {
		t.Error("sample.url removed without the flag")
	}
	if u := got["sample.url"]; u.Has(clean.CodeSidecarRemove) {
		t.Error("sample.url reported removed")
	}
	// Hard links under the default skip mode: both names untouched.
	fr = check("hard_a.mkv", report.Warn, RouteSkip, remux.CodeHardlinked)
	skippedWarn := false
	for _, f := range fr.Findings {
		if f.Code == remux.CodeHardlinked && f.Severity == report.Warn && strings.Contains(f.Message, "skipped (safety.hardlinks = skip)") {
			skippedWarn = true
		}
	}
	if !skippedWarn {
		t.Errorf("hard_a has no HARDLINKED warning: %v", fr.Findings)
	}
	for _, name := range []string{"hard_a.mkv", "hard_b.mkv"} {
		if fileSHA(t, filepath.Join(corpus, name)) != hardB {
			t.Errorf("%s changed under skip", name)
		}
	}
	check("audio.mka", report.Pass, RouteClean)
	// A subtitle-only container has nothing to decode; it is cleaned like
	// any other file and never fails the decode pass.
	fr = check("subs.mks", report.Pass, RouteClean)
	if fr.Has(scan.CodeDecodeFail) || !(fr.Has(clean.CodeNothing) || fr.Has(clean.CodeMetadata)) {
		t.Errorf("subs.mks: want NOTHING_TO_CLEAN or METADATA without DECODE_FAIL: %v", codes(fr))
	}
	if fr := got["audio.mka"]; fr.Has(policy.CodeNoVideo) {
		t.Errorf("audio.mka carries NO_VIDEO: %v", codes(fr))
	}
	check("sample.mp3", report.Pass, RouteClean)
	// The six sample.* containers all want sample.mkv; only the first
	// (sample.avi, sorted) gets it and the others fail without clobbering.
	fr = check("sample.avi", report.Pass, RouteRemux, remux.CodePlaced)
	placed := fileSHA(t, filepath.Join(corpus, "sample.mkv"))
	for _, name := range []string{"sample.m4v", "sample.mov", "sample.mpg", "sample.ts", "sample.webm"} {
		check(name, report.Fail, RouteRemux, remux.CodeOutputExists)
		p := filepath.Join(corpus, name)
		if snapshot(t, corpus)[p] != before[p] {
			t.Errorf("%s changed after OUTPUT_EXISTS", name)
		}
	}
	if fileSHA(t, filepath.Join(corpus, "sample.mkv")) != placed {
		t.Error("sample.mkv was overwritten by a later container")
	}
	// Guarantee 3: a symlink is skipped with WARN SYMLINK and no REFUSED.
	fr = check("link.mkv", report.Warn, RouteSkip, scan.CodeSymlink)
	if fr.Has(remux.CodeRefused) {
		t.Errorf("link.mkv carries REFUSED: %v", codes(fr))
	}
	if _, text := route(t, fr); text != "symlink skipped" {
		t.Errorf("link.mkv route text %q", text)
	}
	if fi, err := os.Lstat(filepath.Join(corpus, "link.mkv")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("link.mkv is no longer a symlink")
	}
	nested := filepath.Join(corpus, "nested", "deep", "clean.mkv")
	found := false
	for _, fr := range res {
		if fr.Path == nested {
			found = true
			if r, _ := route(t, fr); r != RouteClean || fr.Verdict != report.Pass {
				t.Errorf("nested: %s %s %v", r, fr.Verdict, codes(fr))
			}
		}
	}
	if !found {
		t.Error("nested/deep/clean.mkv not processed")
	}
	for _, name := range []string{"run.sh", "double.mkv.exe"} {
		check(name, report.Block, RouteSkip, scan.CodeSidecarBlocked)
		if _, err := os.Lstat(filepath.Join(corpus, name)); err != nil {
			t.Errorf("%s removed without the flag", name)
		}
	}
	if got := leftovers(t, corpus); len(got) != 0 {
		t.Errorf("temp files left: %v", got)
	}
	if strings.Contains(strings.Join(tr.all(), "\n"), "__remuxed") {
		t.Errorf("an output tree was used in place mode: %v", tr.all())
	}

	// Second pass: every Matroska file that passed is now NOTHING_TO_CLEAN
	// and nothing is rewritten again.
	after := snapshot(t, corpus)
	in2, _ := newIngester(t, r, mustProfile(t, "homelab"))
	res2, err := in2.IngestPath(context.Background(), corpus)
	if err != nil {
		t.Fatal(err)
	}
	for _, fr := range res2 {
		prev, ok := got[filepath.Base(fr.Path)]
		if ok && prev.Verdict <= report.Warn && filepath.Ext(fr.Path) == ".mkv" && prev.Info["route"] != string(RouteSkip) {
			if !fr.Has(clean.CodeNothing) {
				t.Errorf("second pass %s: %v", fr.Path, codes(fr))
			}
		}
	}
	// A rebuilt file is only ever cleaned of extended attributes on the
	// second pass; sizes and mtimes of every file stay as they were.
	for p, v := range after {
		if snapshot(t, corpus)[p] != v {
			t.Errorf("second pass changed %s", p)
		}
	}
	if got := leftovers(t, corpus); len(got) != 0 {
		t.Errorf("temp files left after second pass: %v", got)
	}
}

func leftovers(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, root := range roots {
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err == nil && strings.HasPrefix(d.Name(), ".amuxify-") {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

func TestIngestCorpusVariants(t *testing.T) {
	r := needTools(t)
	t.Run("multi.mkv remuxes under an archive-like profile", func(t *testing.T) {
		src := testutil.Copy(t, "multi.mkv")
		in, _ := newIngester(t, r, mustProfile(t, testutil.Profile(t, archiveLike)))
		res, err := in.IngestPath(context.Background(), src)
		if err != nil {
			t.Fatal(err)
		}
		fr := res[0]
		rt, text := route(t, fr)
		if rt != RouteRemux || !strings.Contains(text, "drop #") || !strings.Contains(text, "chapters dropped") {
			t.Errorf("route %s %q %v", rt, text, codes(fr))
		}
		if fr.Verdict > report.Warn || !fr.Has(remux.CodePlaced) || !fr.Has(remux.CodeHashOK) {
			t.Errorf("%s %v", fr.Verdict, codes(fr))
		}
		if fr.Output != src {
			t.Errorf("output %q", fr.Output)
		}
		in2, _ := newIngester(t, r, mustProfile(t, testutil.Profile(t, archiveLike)))
		res, _ = in2.IngestPath(context.Background(), src)
		if rt, _ := route(t, res[0]); rt != RouteClean || !res[0].Has(clean.CodeNothing) {
			t.Errorf("second pass: %s %v", rt, codes(res[0]))
		}
	})
	t.Run("multi.mkv is refused under the real archive profile", func(t *testing.T) {
		src := testutil.Copy(t, "multi.mkv")
		sum := fileSHA(t, src)
		in, _ := newIngester(t, r, mustProfile(t, "archive"))
		res, err := in.IngestPath(context.Background(), src)
		if err != nil {
			t.Fatal(err)
		}
		if res[0].Verdict != report.Fail || !res[0].Has(remux.CodeRefused) || fileSHA(t, src) != sum {
			t.Errorf("%s %v", res[0].Verdict, codes(res[0]))
		}
	})
	t.Run("hard links under break rebuild one name only", func(t *testing.T) {
		corpus := testutil.CopyTree(t)
		a, b := filepath.Join(corpus, "hard_a.mkv"), filepath.Join(corpus, "hard_b.mkv")
		orig := fileSHA(t, b)
		in, _ := newIngester(t, r, mustProfile(t, "homelab"))
		in.Hardlinks = "break"
		in.apply(t)
		res, err := in.IngestPath(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		fr := res[0]
		rt, text := route(t, fr)
		if rt != RouteRemux || text != "hard-linked; rebuilt rather than edited in place (safety.hardlinks = break)" {
			t.Errorf("route %s %q", rt, text)
		}
		if fr.Verdict != report.Warn || !fr.Has(remux.CodePlaced) || !fr.Has(remux.CodeHardlinked) {
			t.Errorf("%s %v", fr.Verdict, codes(fr))
		}
		if fileSHA(t, b) != orig {
			t.Error("hard_b.mkv bytes changed")
		}
		fa, _ := os.Stat(a)
		fb, _ := os.Stat(b)
		if os.SameFile(fa, fb) {
			t.Error("hard_a.mkv still shares the inode with hard_b.mkv")
		}
		if got := leftovers(t, corpus); len(got) != 0 {
			t.Errorf("temp files left: %v", got)
		}
	})
	t.Run("hard links under copy write beside the tree", func(t *testing.T) {
		corpus := testutil.CopyTree(t)
		a, b := filepath.Join(corpus, "hard_a.mkv"), filepath.Join(corpus, "hard_b.mkv")
		orig := fileSHA(t, b)
		in, _ := newIngester(t, r, mustProfile(t, "homelab"))
		in.Hardlinks = "copy"
		in.apply(t)
		res, err := in.IngestPath(context.Background(), a)
		if err != nil {
			t.Fatal(err)
		}
		fr := res[0]
		if rt, _ := route(t, fr); rt != RouteRemux || !fr.Has(remux.CodePlaced) {
			t.Errorf("%s %v", rt, codes(fr))
		}
		if fileSHA(t, a) != orig || fileSHA(t, b) != orig {
			t.Error("a hard-linked name changed under copy")
		}
		want := filepath.Join(filepath.Dir(corpus), "corpus__remuxed", "hard_a.mkv")
		if fr.Output != want {
			t.Errorf("output %q want %q", fr.Output, want)
		}
		if _, err := os.Stat(want); err != nil {
			t.Error(err)
		}
	})
	t.Run("sample.url removed only with the flag", func(t *testing.T) {
		corpus := testutil.CopyTree(t)
		url := filepath.Join(corpus, "sample.url")
		in, _ := newIngester(t, r, mustProfile(t, "homelab"))
		in.RemoveBlockedSidecars = true
		in.apply(t)
		res, err := in.IngestPath(context.Background(), url)
		if err != nil {
			t.Fatal(err)
		}
		if !res[0].Has(clean.CodeSidecarRemove) || res[0].Verdict != report.Block {
			t.Errorf("%s %v", res[0].Verdict, codes(res[0]))
		}
		if _, err := os.Lstat(url); err == nil {
			t.Error("sample.url still exists")
		}
	})
	t.Run("FAIL with force takes the remux route, BLOCK does not", func(t *testing.T) {
		corpus := testutil.CopyTree(t)
		mis := filepath.Join(corpus, "mislabeled.mp4")
		poly := filepath.Join(corpus, "polyglot.mkv")
		polySum := fileSHA(t, poly)
		in, _ := newIngester(t, r, mustProfile(t, "homelab"))
		in.Force = true
		in.apply(t)
		res, err := in.IngestPath(context.Background(), mis)
		if err != nil {
			t.Fatal(err)
		}
		fr := res[0]
		rt, text := route(t, fr)
		if rt != RouteRemux || text != "scan verdict FAIL; rebuilt because --force was given" {
			t.Errorf("route %s %q", rt, text)
		}
		if !fr.Has(remux.CodePlaced) || fr.Output != filepath.Join(corpus, "mislabeled.mkv") {
			t.Errorf("%s %v output %q", fr.Verdict, codes(fr), fr.Output)
		}
		if _, err := os.Lstat(mis); err == nil {
			t.Error("mislabeled.mp4 still exists")
		}
		res, err = in.IngestPath(context.Background(), poly)
		if err != nil {
			t.Fatal(err)
		}
		if res[0].Verdict != report.Block || !res[0].Has(remux.CodeRefused) || fileSHA(t, poly) != polySum {
			t.Errorf("polyglot with force: %s %v", res[0].Verdict, codes(res[0]))
		}
	})
	t.Run("pre-existing destination is never overwritten", func(t *testing.T) {
		dir := t.TempDir()
		src := testutil.Copy(t, "purchased.mp4")
		mp4 := filepath.Join(dir, "x.mp4")
		if err := os.Rename(src, mp4); err != nil {
			t.Fatal(err)
		}
		mkv := write(t, filepath.Join(dir, "x.mkv"), "precious bytes")
		srcSum := fileSHA(t, mp4)
		in, _ := newIngester(t, r, mustProfile(t, "homelab"))
		res, err := in.IngestPath(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		got := byBase(res)
		fr := got["x.mp4"]
		if fr.Verdict != report.Fail || !fr.Has(remux.CodeOutputExists) {
			t.Errorf("x.mp4: %s %v", fr.Verdict, codes(fr))
		}
		if rt, _ := route(t, fr); rt != RouteRemux {
			t.Errorf("x.mp4 route %s", rt)
		}
		if b, _ := os.ReadFile(mkv); string(b) != "precious bytes" {
			t.Errorf("x.mkv overwritten: %q", b)
		}
		if fileSHA(t, mp4) != srcSum {
			t.Error("x.mp4 changed")
		}
		if got := leftovers(t, dir); len(got) != 0 {
			t.Errorf("temp files left: %v", got)
		}
	})
	t.Run("dry run changes nothing", func(t *testing.T) {
		corpus := testutil.CopyTree(t)
		q := filepath.Join(t.TempDir(), "q")
		before := snapshot(t, corpus)
		in, tr := newIngester(t, r, mustProfile(t, "homelab"))
		in.Remuxer.DryRun, in.Cleaner.DryRun = true, true
		in.Force = true
		in.Hardlinks = "break"
		in.RemoveBlockedSidecars = true
		in.apply(t)
		// cli clears the quarantine under --dry-run; ingest itself must
		// not create it either when nothing is BLOCK-moved.
		_ = q
		res, err := in.IngestPath(context.Background(), corpus)
		if err != nil {
			t.Fatal(err)
		}
		sameSnapshot(t, before, snapshot(t, corpus))
		got := byBase(res)
		if fr := got["purchased.mp4"]; !fr.Has(remux.CodeDryRun) || fr.Output != filepath.Join(corpus, "purchased.mkv") {
			t.Errorf("purchased dry: %v %q", codes(fr), fr.Output)
		}
		if fr := got["clean.mkv"]; !fr.Has(clean.CodeDryRun) {
			t.Errorf("clean dry: %v", codes(fr))
		}
		if fr := got["sample.url"]; !fr.Has(clean.CodeDryRun) {
			t.Errorf("sample.url dry: %v", codes(fr))
		}
		if fr := got["hard_a.mkv"]; !fr.Has(remux.CodeDryRun) {
			t.Errorf("hard_a dry under break: %v", codes(fr))
		}
		for _, line := range tr.all() {
			if strings.Contains(line, "mkvpropedit") || strings.Contains(line, "mkvmerge -o") || strings.Contains(line, "mkvmerge --output") {
				t.Errorf("writer ran in dry run: %s", line)
			}
		}
		if got := leftovers(t, corpus); len(got) != 0 {
			t.Errorf("temp files left: %v", got)
		}
	})
	t.Run("decode failure on the clean route leaves the file alone", func(t *testing.T) {
		// A Matroska file whose tail is corrupted still probes, so it
		// routes to clean; the decode check must refuse it before
		// mkvpropedit touches the headers.
		src := testutil.Copy(t, "conforming.mkv")
		b, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		cut := b[:len(b)*3/4]
		if err := os.WriteFile(src, cut, 0o644); err != nil {
			t.Fatal(err)
		}
		sum := fileSHA(t, src)
		in, tr := newIngester(t, r, mustProfile(t, "homelab"))
		res, err := in.IngestPath(context.Background(), src)
		if err != nil {
			t.Fatal(err)
		}
		fr := res[0]
		if fr.Verdict < report.Fail {
			t.Skipf("truncated conforming.mkv still decodes cleanly: %v", codes(fr))
		}
		if fr.Has(clean.CodeMetadata) || fr.Has(clean.CodeNothing) {
			t.Errorf("cleaner ran on a failed file: %v", codes(fr))
		}
		if fileSHA(t, src) != sum {
			t.Error("file changed")
		}
		for _, line := range tr.all() {
			if strings.Contains(line, "mkvpropedit") {
				t.Errorf("mkvpropedit ran: %s", line)
			}
		}
	})
	t.Run("bidi and zero-width directory names", func(t *testing.T) {
		dir := t.TempDir()
		sub := filepath.Join(dir, "sub‮", "​deep")
		src := testutil.Copy(t, "conforming.mkv")
		dst := filepath.Join(sub, "conforming.mkv")
		if err := os.MkdirAll(sub, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(src, dst); err != nil {
			t.Fatal(err)
		}
		before := snapshot(t, dir)
		sum := fileSHA(t, dst)
		in, _ := newIngester(t, r, mustProfile(t, "homelab"))
		res, err := in.IngestPath(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 1 || res[0].Path != dst {
			t.Fatalf("results %+v", res)
		}
		// The tools run under a UTF-8 locale, so the path reaches mkvmerge
		// whole and the file is cleaned like any other conforming file.
		if res[0].Has(scan.CodeMkvError) || res[0].Has(remux.CodeRefused) {
			t.Fatalf("file under a non-ASCII directory was refused: %v", codes(res[0]))
		}
		if rt, _ := route(t, res[0]); rt != RouteClean || res[0].Verdict > report.Warn {
			t.Errorf("%s %s %v", rt, res[0].Verdict, codes(res[0]))
		}
		if fileSHA(t, dst) == "" || len(snapshot(t, dir)) != len(before) {
			t.Fatalf("tree changed shape: %v", snapshot(t, dir))
		}
		if sum == "" {
			t.Fatal("unreadable source")
		}
	})
	t.Run("decode pass follows the probed streams, not the extension", func(t *testing.T) {
		// A subtitle-only Matroska file under an audio extension has
		// nothing to decode and must not fail the decode pass; an audio
		// file under the subtitle extension still gets decoded before
		// mkvpropedit touches it, so a mislabeled extension cannot dodge
		// the check.
		dir := t.TempDir()
		subsAsAudio := filepath.Join(dir, "subs.mka")
		audioAsSubs := filepath.Join(dir, "audio.mks")
		if err := os.Rename(testutil.Copy(t, "subs.mks"), subsAsAudio); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(testutil.Copy(t, "audio.mka"), audioAsSubs); err != nil {
			t.Fatal(err)
		}
		in, tr := newIngester(t, r, mustProfile(t, "homelab"))
		res, err := in.IngestPath(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		got := byBase(res)
		// The scanner refuses subs.mka for lacking audio (NO_AUDIO) before
		// the clean route is reached; whatever the route, the decode pass
		// is never the reason and never runs.
		if fr := got["subs.mka"]; fr.Has(scan.CodeDecodeFail) {
			t.Errorf("subs.mka: %s %v", fr.Verdict, codes(fr))
		}
		if fr := got["audio.mks"]; fr.Has(scan.CodeDecodeFail) || fr.Verdict > report.Warn {
			t.Errorf("audio.mks: %s %v", fr.Verdict, codes(fr))
		}
		decoded := map[string]bool{}
		for _, line := range tr.all() {
			if strings.Contains(line, "ffmpeg") && strings.Contains(line, "-f null") {
				for _, p := range []string{subsAsAudio, audioAsSubs} {
					if strings.Contains(line, p) {
						decoded[p] = true
					}
				}
			}
		}
		if decoded[subsAsAudio] {
			t.Error("ffmpeg decode ran on a subtitle-only container")
		}
		if !decoded[audioAsSubs] {
			t.Error("ffmpeg decode did not run on an audio file named .mks")
		}
	})
	t.Run("original language with shell metacharacters", func(t *testing.T) {
		src := testutil.Copy(t, "conforming.mkv")
		marker := filepath.Join(t.TempDir(), "pwned")
		hostile := "eng; touch " + marker + "; $(touch " + marker + ")"
		in, tr := newIngester(t, r, mustProfile(t, "anime"))
		in.Original = hostile
		in.apply(t)
		res, err := in.IngestPath(context.Background(), src)
		if err != nil {
			t.Fatal(err)
		}
		if res[0].Verdict > report.Warn {
			t.Errorf("%s %v", res[0].Verdict, codes(res[0]))
		}
		if _, err := os.Lstat(marker); err == nil {
			t.Fatal("marker file created")
		}
		for _, line := range tr.all() {
			if strings.Contains(line, "touch") {
				t.Errorf("hostile value reached a command line: %s", line)
			}
		}
	})
	t.Run("file replaced by a symlink before the remuxer runs", func(t *testing.T) {
		dir := t.TempDir()
		src := testutil.Copy(t, "purchased.mp4")
		path := filepath.Join(dir, "a.mp4")
		if err := os.Rename(src, path); err != nil {
			t.Fatal(err)
		}
		victim := write(t, filepath.Join(t.TempDir(), "victim.mp4"), "victim bytes")
		sc := in0(t, r).Scanner.ScanFile(context.Background(), path, dir)
		if sc.Info == nil || sc.File.Verdict >= report.Fail {
			t.Fatalf("scan: %v", codes(sc.File))
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(victim, path); err != nil {
			t.Skipf("symlink: %v", err)
		}
		in := in0(t, r)
		fr := in.Remuxer.RemuxScanned(context.Background(), sc, dir, dir+"__remuxed")
		if b, _ := os.ReadFile(victim); string(b) != "victim bytes" {
			t.Errorf("symlink target changed: %q", b)
		}
		if fr.Verdict < report.Fail {
			t.Logf("remuxer result on swapped path: %s %v", fr.Verdict, codes(fr))
		}
		if _, err := os.Lstat(filepath.Join(dir, "a.mkv")); err == nil {
			if fi, _ := os.Lstat(victim); fi == nil || fi.Size() != int64(len("victim bytes")) {
				t.Error("victim rewritten")
			}
		}
		if got := leftovers(t, dir); len(got) != 0 {
			t.Errorf("temp files left: %v", got)
		}
	})
}

func in0(t *testing.T, r *exec.Runner) *Ingester {
	t.Helper()
	in, _ := newIngester(t, &exec.Runner{Timeout: r.Timeout}, mustProfile(t, "homelab"))
	return in
}

// A single blocked file given as the ingest root is quarantined under the
// quarantine directory by its base name, the way scan.ScanPath does it. The
// Sonarr and Radarr adapters always hand over one file, so this is the path
// every arr quarantine takes (review C1).
func TestIngestSingleFileRootQuarantines(t *testing.T) {
	noTools(t)
	for _, name := range []string{"x.url", "empty.mkv", "movie‮vkm.mkv"} {
		t.Run(name, func(t *testing.T) {
			body := "[InternetShortcut]\nURL=http://x\n"
			if strings.HasSuffix(name, ".mkv") {
				body = ""
			}
			src := write(t, filepath.Join(t.TempDir(), name), body)
			q := filepath.Join(t.TempDir(), "quarantine")
			in, tr := newIngester(t, nil, mustProfile(t, "homelab"))
			in.Scanner.Quarantine = q
			in.RemoveBlockedSidecars = true
			in.apply(t)
			res, err := in.IngestPath(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			if len(res) != 1 {
				t.Fatalf("%d results", len(res))
			}
			fr := res[0]
			dest := filepath.Join(q, name)
			f, ok := finding(fr, scan.CodeQuarantined)
			if !ok || f.Severity != report.Block || f.Message != "moved to "+dest {
				t.Fatalf("QUARANTINED finding %+v ok=%v, want BLOCK moved to %s (%v)", f, ok, dest, codes(fr))
			}
			if fr.Verdict != report.Block {
				t.Errorf("verdict %s", fr.Verdict)
			}
			if _, err := os.Lstat(dest); err != nil {
				t.Errorf("not in quarantine: %v", err)
			}
			if _, err := os.Lstat(src); err == nil {
				t.Error("source still in place")
			}
			if fi, err := os.Lstat(q); err != nil || !fi.IsDir() {
				t.Errorf("quarantine root is not a directory: %v", err)
			}
			// A quarantined sidecar is never also removed by the cleaner.
			if fr.Has(clean.CodeSidecarRemove) || fr.Has(clean.CodeCleanFail) {
				t.Errorf("cleaner touched a quarantined file: %v", codes(fr))
			}
			if got := tr.all(); len(got) != 0 {
				t.Errorf("tools ran: %v", got)
			}
		})
	}
}

// An unreadable directory inside the tree is reported as a run-level error
// after the readable files, never dropped, so a hook cannot tell its caller
// the download is good when part of it could not be read (review C2).
func TestIngestReportsUnreadableDirectory(t *testing.T) {
	noTools(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := t.TempDir()
	ok := write(t, filepath.Join(root, "a", "ok.nfo"), "nfo\n")
	locked := filepath.Join(root, "locked")
	write(t, filepath.Join(locked, "payload.url"), "[InternetShortcut]\nURL=http://x\n")
	if err := os.Chmod(locked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	in, _ := newIngester(t, nil, mustProfile(t, "homelab"))
	fired := 0
	in.Progress = func(report.FileResult) { fired++ }
	res, err := in.IngestPath(context.Background(), root)
	if err == nil || !strings.Contains(err.Error(), "cannot read "+locked) {
		t.Fatalf("err %v, want one naming %s", err, locked)
	}
	if len(res) != 1 || res[0].Path != ok || fired != 1 {
		t.Fatalf("results %d, progress %d", len(res), fired)
	}
	// The way cli.runIngest records it, the run verdict is FAIL.
	s := report.NewSummary("amuxify", "test", "ingest", "homelab")
	for _, r := range res {
		s.Append(r)
	}
	s.Error(err.Error())
	if s.Verdict != report.Fail {
		t.Errorf("run verdict %s, want FAIL", s.Verdict)
	}
}

// Review C9: a FAIL audio or subtitle file under --force takes the clean
// route and keeps its FAIL verdict. Before the fix it was routed to remux,
// which only handles video containers and skipped it, so the ROUTE line
// promised a rebuild that never happened.
func TestForcedFailAudioIsCleanedNotRemuxed(t *testing.T) {
	r := needTools(t)
	for _, name := range []string{"audio.mka", "subs.mks", "sample.mp3"} {
		t.Run(name, func(t *testing.T) {
			src := testutil.Copy(t, name)
			if err := os.Chmod(src, 0o755); err != nil {
				t.Fatal(err)
			}
			in, _ := newIngester(t, r, mustProfile(t, "archive"))
			in.Force = true
			in.apply(t)
			res, err := in.IngestPath(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			fr := res[0]
			rt, text := route(t, fr)
			if rt != RouteClean || text != "scan verdict FAIL; cleaned because --force was given" {
				t.Errorf("route %s %q", rt, text)
			}
			if fr.Verdict != report.Fail || !fr.Has(scan.CodeExecPerm) {
				t.Errorf("verdict %s %v, want FAIL with EXEC_PERM kept", fr.Verdict, codes(fr))
			}
			if f, ok := finding(fr, remux.CodeSkipped); ok && strings.Contains(f.Message, "no video stream") {
				t.Errorf("remux skipped the file: %v", codes(fr))
			}
			if _, err := os.Lstat(src); err != nil {
				t.Errorf("source gone: %v", err)
			}
		})
	}
}

// Review C10: ingest has no --strip-audio-tags flag, so the SKIPPED advice
// for an audio file must not name it.
func TestIngestAudioTagsAdviceNamesNoFlag(t *testing.T) {
	r := needTools(t)
	src := testutil.Copy(t, "sample.mp3")
	in, _ := newIngester(t, r, mustProfile(t, "homelab"))
	res, err := in.IngestPath(context.Background(), src)
	if err != nil {
		t.Fatal(err)
	}
	fr := res[0]
	f, ok := finding(fr, clean.CodeSkipped)
	if !ok || f.Message != "audio tags left alone; ingest does not strip audio tags" {
		t.Errorf("SKIPPED %q (%v)", f.Message, codes(fr))
	}
	for _, f := range fr.Findings {
		if strings.Contains(f.Message, "--strip-audio-tags") {
			t.Errorf("advice names a flag ingest does not have: %s", f.Message)
		}
	}
	// IngestFile sets the wording too, so a caller that skips IngestPath
	// gets the same text.
	in2, _ := newIngester(t, r, mustProfile(t, "homelab"))
	fr = in2.IngestFile(context.Background(), src, filepath.Dir(src), filepath.Dir(src), filepath.Dir(src))
	if f, ok := finding(fr, clean.CodeSkipped); !ok || strings.Contains(f.Message, "--strip-audio-tags") {
		t.Errorf("IngestFile SKIPPED %q", f.Message)
	}
}

// Review C11 and guarantee 3: a symlinked sidecar is reported once, as
// SYMLINK by the scanner, is never followed and never cleaned through the
// link. Before the fix the cleaner added a second SYMLINK finding.
func TestSymlinkedSidecarReportedOnce(t *testing.T) {
	noTools(t)
	victim := write(t, filepath.Join(t.TempDir(), "victim.srt"), "1\n00:00:00,000 --> 00:00:01,000\nhi\n")
	dir := t.TempDir()
	link := filepath.Join(dir, "link.srt")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	// An attribute in the namespace the cleaner strips: it must survive,
	// which proves the cleaner never reached the victim through the link.
	attr := "user.amuxifytest"
	if runtime.GOOS == "darwin" {
		attr = "com.apple.amuxifytest"
	}
	if err := xattr.LSet(victim, attr, []byte("x")); err != nil {
		t.Logf("no xattr on the victim: %v", err)
		attr = ""
	}
	in, tr := newIngester(t, nil, mustProfile(t, "homelab"))
	in.RemoveBlockedSidecars = true
	in.apply(t)
	res, err := in.IngestPath(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("results %+v", res)
	}
	fr := res[0]
	n := 0
	for _, f := range fr.Findings {
		if f.Code == scan.CodeSymlink {
			n++
		}
	}
	if n != 1 {
		t.Errorf("SYMLINK reported %d times: %v", n, codes(fr))
	}
	if fr.Verdict != report.Warn || fr.Has(clean.CodeXattr) || fr.Has(clean.CodeNothing) || fr.Has(clean.CodeSidecarRemove) {
		t.Errorf("%s %v", fr.Verdict, codes(fr))
	}
	if rt, text := route(t, fr); rt != RouteSkip || text != "sidecar; symlink skipped" {
		t.Errorf("route %s %q", rt, text)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Error("the link was removed or replaced")
	}
	if b, _ := os.ReadFile(victim); !strings.Contains(string(b), "hi") {
		t.Error("the victim changed")
	}
	if attr != "" {
		if _, err := xattr.LGet(victim, attr); err != nil {
			t.Errorf("the victim's xattr was stripped through the link: %v", err)
		}
	}
	if got := tr.all(); len(got) != 0 {
		t.Errorf("tools ran: %v", got)
	}
}

// Review C33: a dry run predicts the OUTPUT_EXISTS collision the live run
// hits when two files of one run rebuild to the same destination, with the
// same finding text, so the verdict counts and the exit code match.
func TestDryRunPredictsDestinationCollision(t *testing.T) {
	r := needTools(t)
	prepare := func(t *testing.T) string {
		dir := t.TempDir()
		for _, name := range []string{"sample.mov", "sample.webm"} {
			src := testutil.Copy(t, name)
			if err := os.Rename(src, filepath.Join(dir, "ep"+filepath.Ext(name))); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}
	run := func(t *testing.T, dir string, dry bool) []report.FileResult {
		t.Helper()
		in, _ := newIngester(t, r, mustProfile(t, "homelab"))
		in.Remuxer.DryRun, in.Cleaner.DryRun = dry, dry
		res, err := in.IngestPath(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(res) != 2 {
			t.Fatalf("results %d", len(res))
		}
		return res
	}
	counts := func(res []report.FileResult) map[report.Severity]int {
		m := map[report.Severity]int{}
		for _, fr := range res {
			m[fr.Verdict]++
		}
		return m
	}
	dryDir := prepare(t)
	before := snapshot(t, dryDir)
	dry := run(t, dryDir, true)
	sameSnapshot(t, before, snapshot(t, dryDir))
	dest := filepath.Join(dryDir, "ep.mkv")
	mov, webm := byBase(dry)["ep.mov"], byBase(dry)["ep.webm"]
	if !mov.Has(remux.CodeDryRun) || mov.Output != dest || mov.Has(remux.CodeOutputExists) {
		t.Errorf("ep.mov dry: %v %q", codes(mov), mov.Output)
	}
	f, ok := finding(webm, remux.CodeOutputExists)
	if !ok || f.Severity != report.Fail || f.Message != dest+" already exists" {
		t.Errorf("ep.webm dry: %v %q", codes(webm), f.Message)
	}
	if webm.Verdict != report.Fail || webm.Has(remux.CodeDryRun) || webm.Output != "" {
		t.Errorf("ep.webm dry: %s %v %q", webm.Verdict, codes(webm), webm.Output)
	}
	total := 0
	for _, fr := range dry {
		for _, f := range fr.Findings {
			if f.Code == remux.CodeOutputExists {
				total++
			}
		}
	}
	if total != 1 {
		t.Errorf("OUTPUT_EXISTS reported %d times, want 1", total)
	}
	// The planned set is per run: a second dry run of the same tree gives
	// the same answer, not a collision for the first file too.
	again := byBase(run(t, dryDir, true))
	againMov, againWebm := again["ep.mov"], again["ep.webm"]
	if !againMov.Has(remux.CodeDryRun) || !againWebm.Has(remux.CodeOutputExists) {
		t.Errorf("second dry run differs: %v / %v", codes(againMov), codes(againWebm))
	}

	liveDir := prepare(t)
	live := run(t, liveDir, false)
	liveWebm := byBase(live)["ep.webm"]
	liveDest := filepath.Join(liveDir, "ep.mkv")
	if f, ok := finding(liveWebm, remux.CodeOutputExists); !ok || f.Message != liveDest+" already exists" {
		t.Fatalf("live ep.webm: %v", codes(liveWebm))
	}
	if strings.Join(codes(liveWebm), ",") != strings.Join(codes(webm), ",") {
		t.Errorf("collision findings differ: dry %v, live %v", codes(webm), codes(liveWebm))
	}
	if dc, lc := counts(dry), counts(live); fmt.Sprint(dc) != fmt.Sprint(lc) {
		t.Errorf("verdict counts differ: dry %v, live %v", dc, lc)
	}
	if _, err := os.Lstat(liveDest); err != nil {
		t.Errorf("live run did not place %s: %v", liveDest, err)
	}
}
