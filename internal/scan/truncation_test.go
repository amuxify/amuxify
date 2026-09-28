package scan

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/testutil"
)

// stubTool installs a POSIX shell script under the named tool's override.
// The script runs with whatever arguments amuxify passes and ignores them
// unless the body looks at them.
func stubTool(t *testing.T, tool, body string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub tool is a POSIX shell script")
	}
	p := filepath.Join(t.TempDir(), tool)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_"+strings.ToUpper(tool), p)
}

// srtFlood is a stub ffmpeg body that prints an SRT track of about 180 KiB
// of harmless cues and then one cue carrying a URL, so a runner that keeps
// less than that never sees the link.
const srtFlood = `i=0; while [ $i -lt 3000 ]; do printf '%d\n00:00:01,000 --> 00:00:02,000\nharmless subtitle text line\n\n' $i; i=$((i+1)); done; printf '9999\n00:00:03,000 --> 00:00:04,000\nvisit https://evil.example/payload\n\n'`

// Guarantee 4, from the other side: the runner's bound on what it keeps of
// a tool's output must never turn into a silent pass. A text subtitle
// track longer than the runner keeps is reported LINK_IN_SUBS as not fully
// checked, at the severity the profile gives a link, even though the URL
// sits past the cut and was never seen. With a bound the track fits under,
// the same track is reported for the link itself. The track comes from a
// stub ffmpeg so its size is under the test's control; ffprobe and mkvmerge
// are real, on the subtitle-only fixture, so the stream is a genuine text
// subtitle and the scan reaches the extraction.
func TestSubtitleTrackLongerThanRunnerKeeps(t *testing.T) {
	r := testutil.Need(t, exec.FFprobe, exec.MKVMerge)
	stubTool(t, exec.FFmpeg, srtFlood)
	for _, profile := range []string{"homelab", "strict"} {
		sev := report.Warn
		if profile == "strict" {
			sev = report.Fail
		}
		r.MaxOutput = 64 << 10
		s := newScanner(t, mustProfile(t, profile), r)
		fr := scanOne(t, s, testutil.Copy(t, "subs.mks"))
		expect(t, fr, sev, CodeLinkInSubs)
		f := finding(fr, CodeLinkInSubs)
		if f.Severity != sev || !strings.Contains(f.Message, "longer than the runner keeps and was not fully checked for links") {
			t.Fatalf("%s: %s %q", profile, f.Severity, f.Message)
		}
		if strings.Contains(f.Message, "evil.example") {
			t.Fatalf("%s: the link past the cut was seen: %q", profile, f.Message)
		}
		// With room for the whole track the link itself is reported and
		// nothing says the track was cut.
		r.MaxOutput = 1 << 20
		s = newScanner(t, mustProfile(t, profile), r)
		fr = scanOne(t, s, testutil.Copy(t, "subs.mks"))
		expect(t, fr, sev, CodeLinkInSubs)
		var cut, link bool
		for _, f := range fr.Findings {
			if f.Code != CodeLinkInSubs {
				continue
			}
			cut = cut || strings.Contains(f.Message, "not fully checked")
			link = link || strings.Contains(f.Message, "evil.example")
		}
		if cut || !link {
			t.Fatalf("%s with room for the track: cut=%v link=%v %v", profile, cut, link, codes(fr))
		}
	}
}

// A probe whose output is longer than the runner keeps is reported as
// exactly that, not as a parse error of the part that was kept: ffprobe
// makes the file UNPARSEABLE with the reason named, and mkvmerge -J on a
// Matroska file is an MKV_ERROR with the reason named. Neither path can
// pass a file on a partial document.
func TestProbeOutputLongerThanRunnerKeeps(t *testing.T) {
	noTools(t)
	const media = `{"format":{"format_name":"matroska,webm","duration":"10.0"},"streams":[{"index":0,"codec_type":"video","codec_name":"h264"},{"index":1,"codec_type":"audio","codec_name":"aac"}]}`
	pad := `i=0; while [ $i -lt 2000 ]; do printf '{"padding":"%s"}\n' "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"; i=$((i+1)); done`
	dir := t.TempDir()
	path := write(t, filepath.Join(dir, "a.mkv"), ebml, 0o644)

	stubTool(t, exec.FFprobe, `printf '%s' '{"streams":['; `+pad+`; printf ']}'`)
	r := &exec.Runner{MaxOutput: 64 << 10}
	fr := scanOne(t, newScanner(t, mustProfile(t, "homelab"), r), path)
	expect(t, fr, report.Fail, CodeUnparseable)
	f := finding(fr, CodeUnparseable)
	if !strings.Contains(f.Message, "ffprobe: output was longer than the runner keeps") {
		t.Fatalf("ffprobe message %q", f.Message)
	}

	stubTool(t, exec.FFprobe, `printf '%s' '`+media+`'`)
	stubTool(t, exec.MKVMerge, `printf '%s' '{"container":{"recognized":true,"supported":true},"tracks":['; `+pad+`; printf ']}'`)
	r = &exec.Runner{MaxOutput: 64 << 10}
	fr = scanOne(t, newScanner(t, mustProfile(t, "homelab"), r), path)
	if fr.Verdict < report.Fail || !fr.Has(CodeMkvError) {
		t.Fatalf("mkvmerge: %s %v", fr.Verdict, codes(fr))
	}
	f = finding(fr, CodeMkvError)
	if !strings.Contains(f.Message, "mkvmerge -J output was longer than the runner keeps") {
		t.Fatalf("mkvmerge message %q", f.Message)
	}
	// The same stubs with room for their output parse and pass the probe.
	stubTool(t, exec.MKVMerge, `printf '%s' '{"container":{"recognized":true,"supported":true,"properties":{}},"tracks":[]}'`)
	r = &exec.Runner{MaxOutput: 1 << 20}
	fr = scanOne(t, newScanner(t, mustProfile(t, "homelab"), r), path)
	if fr.Has(CodeUnparseable) || fr.Has(CodeMkvError) {
		t.Fatalf("with room for the output: %v", codes(fr))
	}
}
