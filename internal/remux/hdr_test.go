package remux

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// filesUnder lists every regular file below root.
func filesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	return files
}

// hdrFinding returns the HDR_LOST finding of fr.
func hdrFinding(t *testing.T, fr report.FileResult) report.Finding {
	t.Helper()
	f, ok := finding(fr, CodeHDRLost)
	if !ok {
		t.Fatalf("no HDR_LOST finding: %s %v", fr.Verdict, codes(fr))
	}
	return f
}

// A remux of the HDR fixtures keeps every colour and HDR value: the output
// probes to the same normalised signalling as the source, and the file
// passes with the hashes verified.
func TestRemuxKeepsHDRProperties(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	for _, name := range []string{"hdr10.mkv", "hlg.mkv"} {
		t.Run(name, func(t *testing.T) {
			src := testutil.Copy(t, name)
			outRoot := filepath.Join(t.TempDir(), "out")
			rm, _ := newRemuxer(t, r, mustProfile(t, "homelab"))
			rm.OutputRoot = outRoot
			res, err := rm.RemuxPath(context.Background(), filepath.Dir(src))
			if err != nil {
				t.Fatal(err)
			}
			fr := res[0]
			if fr.Verdict != report.Pass || !fr.Has(CodeHashOK) || !fr.Has(CodePlaced) || fr.Has(CodeHDRLost) {
				t.Fatalf("%s %v", fr.Verdict, codes(fr))
			}
			pr := &probe.Prober{Runner: r, Timeout: time.Minute}
			in, err := pr.Probe(context.Background(), src)
			if err != nil {
				t.Fatal(err)
			}
			out, err := pr.Probe(context.Background(), fr.Output)
			if err != nil {
				t.Fatal(err)
			}
			sv, ov := in.StreamsOf("video")[0], out.StreamsOf("video")[0]
			if sv.Color.Empty() || len(sv.HDR) == 0 {
				t.Fatalf("the fixture carries no HDR signalling: %v %q", sv.HDR, sv.Color.String())
			}
			if diffs := hdrDiff(sv, ov); len(diffs) != 0 {
				t.Fatalf("output differs from source: %v\nsource %s\noutput %s", diffs, sv.Color.String(), ov.Color.String())
			}
			if name == "hdr10.mkv" && (ov.Color.Mastering == nil || ov.Color.Light == nil) {
				t.Fatalf("output lost static metadata: %s", ov.Color.String())
			}
		})
	}
}

// Guarantee 5 for the signalling: an output whose colour or HDR properties
// were stripped or changed after mkvmerge wrote it is HDR_LOST, the
// finding names the property, the output is deleted and the source is
// untouched. The mkvmerge wrapper edits the temp file with mkvpropedit
// right after the real mkvmerge returns, which is where a muxer that drops
// a value would leave the file.
func TestHDRPropertyStrippedDeletesOutput(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	propedit, err := r.Path(exec.MKVPropedit)
	if err != nil {
		t.Fatal(err)
	}
	// tamper runs mkvpropedit on the file mkvmerge just wrote, found as
	// the argument after -o.
	tamper := func(edit string) string {
		return "out=; prev=; for a in \"$@\"; do [ \"$prev\" = -o ] && out=$a; prev=$a; done\n" +
			shq(propedit) + " -q \"$out\" --edit track:v1 " + edit + " || exit 2\n"
	}
	cases := []struct {
		name    string
		fixture string
		inPlace bool
		edit    string
		names   []string // phrases the message must carry
	}{
		{"content light deleted", "hdr10.mkv", false, "--delete max-content-light --delete max-frame-light", []string{"content light level lost"}},
		{"max luminance changed", "hdr10.mkv", false, "--set max-luminance=4000", []string{"max_luminance changed from 1000 to 4000"}},
		{"min luminance changed", "hdr10.mkv", false, "--set min-luminance=0.005", []string{"min_luminance changed from 0.0001 to 0.005"}},
		{"chromaticity changed", "hdr10.mkv", false, "--set chromaticity-coordinates-red-x=0.64", []string{"red_x changed from 0.708 to 0.64"}},
		{"white point changed", "hdr10.mkv", false, "--set white-coordinates-y=0.3", []string{"white_y changed from 0.329 to 0.3"}},
		{"transfer changed to bt709", "hdr10.mkv", false, "--set colour-transfer-characteristics=1", []string{"source video is hdr10, output is none", "transfer changed from smpte2084 to bt709"}},
		{"primaries changed", "hdr10.mkv", false, "--set colour-primaries=1", []string{"primaries changed from bt2020 to bt709"}},
		{"matrix deleted", "hdr10.mkv", false, "--delete colour-matrix-coefficients", []string{"matrix lost (was bt2020nc)"}},
		{"range changed", "hdr10.mkv", false, "--set colour-range=2", []string{"range changed from tv to pc"}},
		{"hlg transfer changed", "hlg.mkv", false, "--set colour-transfer-characteristics=16", []string{"source video is hlg, output is hdr10", "transfer changed from arib-std-b67 to smpte2084"}},
		{"content light deleted in place", "hdr10.mkv", true, "--delete max-content-light --delete max-frame-light", []string{"content light level lost"}},
		{"max luminance changed in place", "hdr10.mkv", true, "--set max-luminance=4000", []string{"max_luminance changed from 1000 to 4000"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := testutil.Copy(t, tc.fixture)
			root := filepath.Dir(src)
			srcBefore, err := os.Lstat(src)
			if err != nil {
				t.Fatal(err)
			}
			shaBefore := fileSHA(t, src)
			outRoot := filepath.Join(t.TempDir(), "out")
			mkvmergeWrapper(t, r, "", tamper(tc.edit))
			rm, tr := newRemuxer(t, nil, mustProfile(t, "homelab"))
			rm.OutputRoot = outRoot
			rm.InPlace = tc.inPlace
			res, err := rm.RemuxPath(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			fr := res[0]
			if fr.Verdict != report.Fail || fr.Has(CodePlaced) || fr.Has(CodeHashOK) || fr.Output != "" {
				t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
			}
			f := hdrFinding(t, fr)
			if f.Severity != report.Fail {
				t.Fatalf("HDR_LOST severity %s", f.Severity)
			}
			if !strings.HasPrefix(f.Message, "stream #0: ") {
				t.Fatalf("message does not name the stream: %q", f.Message)
			}
			for _, want := range tc.names {
				if !strings.Contains(f.Message, want) {
					t.Fatalf("message %q does not say %q", f.Message, want)
				}
			}
			// The tamper really ran, so the failure is the verifier's doing.
			if len(tr.writes()) == 0 {
				t.Fatal("mkvmerge never ran")
			}
			if files := filesUnder(t, outRoot); len(files) != 0 {
				t.Fatalf("output kept after HDR_LOST: %v", files)
			}
			if l := leftovers(t, outRoot, root); len(l) != 0 {
				t.Fatalf("temp files left: %v", l)
			}
			srcAfter, err := os.Lstat(src)
			if err != nil {
				t.Fatal(err)
			}
			if srcAfter.Size() != srcBefore.Size() || !srcAfter.ModTime().Equal(srcBefore.ModTime()) || fileSHA(t, src) != shaBefore {
				t.Fatal("source changed after HDR_LOST")
			}
			if entries, _ := os.ReadDir(root); len(entries) != 1 {
				t.Fatalf("source directory holds %d entries after the failure", len(entries))
			}
		})
	}
}

// An SDR source whose output came back carrying HDR signalling is HDR_LOST
// as well: gained values are as much a change as lost ones, and the output
// is discarded.
func TestSDRGainedHDRFails(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	propedit, err := r.Path(exec.MKVPropedit)
	if err != nil {
		t.Fatal(err)
	}
	src := testutil.Copy(t, "clean.mkv")
	root := filepath.Dir(src)
	shaBefore := fileSHA(t, src)
	outRoot := filepath.Join(t.TempDir(), "out")
	gain := "out=; prev=; for a in \"$@\"; do [ \"$prev\" = -o ] && out=$a; prev=$a; done\n" +
		shq(propedit) + " -q \"$out\" --edit track:v1 --set colour-primaries=9 --set colour-transfer-characteristics=16 " +
		"--set colour-matrix-coefficients=9 --set max-luminance=1000 --set min-luminance=0.0001 --set max-content-light=1000 --set max-frame-light=400 || exit 2\n"
	mkvmergeWrapper(t, r, "", gain)
	rm, _ := newRemuxer(t, nil, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	fr := res[0]
	if fr.Verdict != report.Fail || fr.Has(CodePlaced) || fr.Output != "" {
		t.Fatalf("%s %v output=%q", fr.Verdict, codes(fr), fr.Output)
	}
	f := hdrFinding(t, fr)
	for _, want := range []string{"source video is none, output is hdr10", "mastering display metadata gained", "content light level gained", "transfer gained (now smpte2084)"} {
		if !strings.Contains(f.Message, want) {
			t.Fatalf("message %q does not say %q", f.Message, want)
		}
	}
	if files := filesUnder(t, outRoot); len(files) != 0 {
		t.Fatalf("output kept: %v", files)
	}
	if l := leftovers(t, outRoot, root); len(l) != 0 {
		t.Fatalf("temp files left: %v", l)
	}
	if fileSHA(t, src) != shaBefore {
		t.Fatal("source changed")
	}
}

// A dry run of an HDR file plans the output and touches no tool that
// writes, so the new assertion cannot run and cannot fail it.
func TestHDRDryRunUntouched(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge, exec.MKVPropedit, exec.MKVExtract)
	src := testutil.Copy(t, "hdr10.mkv")
	root := filepath.Dir(src)
	shaBefore := fileSHA(t, src)
	outRoot := filepath.Join(t.TempDir(), "out")
	rm, tr := newRemuxer(t, r, mustProfile(t, "homelab"))
	rm.OutputRoot = outRoot
	rm.DryRun = true
	res, err := rm.RemuxPath(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	fr := res[0]
	if fr.Verdict != report.Pass || !fr.Has(CodeDryRun) || fr.Has(CodeHDRLost) || fr.Has(CodePlaced) {
		t.Fatalf("%s %v", fr.Verdict, codes(fr))
	}
	if fr.Output != filepath.Join(outRoot, "hdr10.mkv") {
		t.Fatalf("planned output %q", fr.Output)
	}
	if w := tr.writes(); len(w) != 0 {
		t.Fatalf("a dry run ran a writing tool: %v", w)
	}
	if _, err := os.Lstat(outRoot); !os.IsNotExist(err) {
		t.Fatalf("a dry run created the output tree: %v", err)
	}
	if fileSHA(t, src) != shaBefore {
		t.Fatal("source changed")
	}
}

// Dolby Vision cannot be built with the fixture tools, so the comparison
// the verifier runs is exercised on streams as the probe would build them
// from ffprobe's output: a configuration record that is lost or altered is
// reported by name, and so is an HDR10+ family label that went missing.
func TestHdrDiffDolbyVision(t *testing.T) {
	dv := func(profile, level int, rpu, el, bl bool, compat int) probe.Stream {
		return probe.Stream{Index: 0, Type: "video", HDR: []string{"hdr10", "dovi"}, Color: probe.Color{
			Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", Range: "tv",
			Light:       &probe.ContentLight{MaxCLL: 1000, MaxFALL: 400},
			DolbyVision: &probe.DolbyVision{Profile: profile, Level: level, RPU: rpu, EL: el, BL: bl, BLSignalCompatibilityID: compat},
		}}
	}
	hdr := dv(8, 6, true, false, true, 1)
	sdr := probe.Stream{Index: 0, Type: "video"}
	cases := []struct {
		name     string
		src, out probe.Stream
		want     []string
	}{
		{"identical", hdr, dv(8, 6, true, false, true, 1), nil},
		{"record lost", hdr, func() probe.Stream {
			s := dv(8, 6, true, false, true, 1)
			s.HDR = []string{"hdr10"}
			s.Color.DolbyVision = nil
			return s
		}(), []string{"source video is hdr10+dovi, output is hdr10", "dolby vision configuration lost"}},
		{"profile changed", hdr, dv(5, 6, true, false, true, 1), []string{"dv_profile changed from 8 to 5"}},
		{"level changed", hdr, dv(8, 9, true, false, true, 1), []string{"dv_level changed from 6 to 9"}},
		{"rpu flag cleared", hdr, dv(8, 6, false, false, true, 1), []string{"dv_rpu changed from 1 to 0"}},
		{"el flag set", hdr, dv(8, 6, true, true, true, 1), []string{"dv_el changed from 0 to 1"}},
		{"bl flag cleared", hdr, dv(8, 6, true, false, false, 1), []string{"dv_bl changed from 1 to 0"}},
		{"compatibility id changed", hdr, dv(8, 6, true, false, true, 4), []string{"dv_bl_signal_compatibility_id changed from 1 to 4"}},
		{"record gained on sdr", sdr, hdr, []string{"source video is none, output is hdr10+dovi", "content light level gained", "dolby vision configuration gained",
			"primaries gained (now bt2020)", "transfer gained (now smpte2084)", "matrix gained (now bt2020nc)", "range gained (now tv)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hdrDiff(tc.src, tc.out)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("diff\n got %q\nwant %q", got, tc.want)
			}
		})
	}
	// An HDR10+ label that went missing is reported even when every static
	// value survived, because the dynamic metadata lives in the packets and
	// only the label says the demuxer still sees it.
	plus := probe.Stream{Index: 0, Type: "video", HDR: []string{"hdr10", "hdr10plus"}, Color: probe.Color{Transfer: "smpte2084"}}
	lost := probe.Stream{Index: 0, Type: "video", HDR: []string{"hdr10"}, Color: probe.Color{Transfer: "smpte2084"}}
	if got := hdrDiff(plus, lost); !reflect.DeepEqual(got, []string{"source video is hdr10+hdr10plus, output is hdr10"}) {
		t.Fatalf("hdr10plus loss: %q", got)
	}
}
