package ingest

import (
	"fmt"
	"strconv"

	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/remux"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
)

// Route is where a scanned file goes next.
type Route string

const (
	RouteRemux Route = "remux"
	RouteClean Route = "clean"
	RouteSkip  Route = "skip"
)

// CodeRoute is the PASS finding that records the route and its reasons.
const CodeRoute = "ROUTE"

// nlinkOf reads the hard-link count scan recorded in Info["nlink"]. A missing
// or malformed value counts as one link.
func nlinkOf(fr *report.FileResult) uint64 {
	if fr.Info == nil {
		return 1
	}
	n, err := strconv.ParseUint(fr.Info["nlink"], 10, 64)
	if err != nil || n == 0 {
		return 1
	}
	return n
}

func addAll(fr *report.FileResult, fs []report.Finding) {
	for _, f := range fs {
		fr.Add(f)
	}
}

// Decide is the pure routing function. It appends REFUSED, HARDLINKED and,
// for every route except remux, the decision's findings to fr (RemuxScanned
// appends them itself on the remux route). It returns the route and reasons.
func Decide(fr *report.FileResult, sc scan.Result, d *policy.Decision, hardlinks string, force bool) (Route, []string) {
	if d == nil {
		d = &policy.Decision{}
	}
	category := scan.MediaExts[fsutil.Ext(fr.Path)]
	isAudioOrSubs := category == "audio" || category == "subtitle"
	if isAudioOrSubs {
		// The policy decides for video files; an audio or subtitle container
		// has no video stream by design, so that finding does not apply.
		d = withoutNoVideo(d)
	}
	if !force {
		for _, f := range d.Findings {
			if f.Severity >= report.Fail {
				addAll(fr, d.Findings)
				return RouteSkip, []string{"decision failed"}
			}
		}
	}
	// A forced FAIL video file is rebuilt. An audio or subtitle container
	// cannot be rebuilt (remux handles video containers only), so it takes
	// the clean route below and keeps its FAIL verdict (review C9).
	if sc.File.Verdict >= report.Fail && force && !isAudioOrSubs {
		return RouteRemux, []string{"scan verdict FAIL; rebuilt because --force was given"}
	}
	if n := nlinkOf(fr); n > 1 {
		switch {
		case isAudioOrSubs:
			fr.Addf(remux.CodeHardlinked, report.Warn, "file has %d hard links; audio and subtitle files are edited in place, which would change every link", n)
			addAll(fr, d.Findings)
			return RouteSkip, []string{"hard-linked audio or subtitle file"}
		case hardlinks == "break", hardlinks == "copy":
			return RouteRemux, []string{fmt.Sprintf("hard-linked; rebuilt rather than edited in place (safety.hardlinks = %s)", hardlinks)}
		default:
			fr.Addf(remux.CodeHardlinked, report.Warn, "file has %d hard links; skipped (safety.hardlinks = skip)", n)
			addAll(fr, d.Findings)
			return RouteSkip, []string{"hard-linked"}
		}
	}
	if isAudioOrSubs {
		addAll(fr, d.Findings)
		if sc.File.Verdict >= report.Fail && force {
			return RouteClean, []string{"scan verdict FAIL; cleaned because --force was given"}
		}
		return RouteClean, []string{"audio or subtitle container; metadata only"}
	}
	if fsutil.Ext(fr.Path) != "mkv" || sc.Info == nil || !sc.Info.IsMatroska() {
		name := "unknown"
		if sc.Info != nil && sc.Info.Container != "" {
			name = sc.Info.Container
		}
		return RouteRemux, []string{fmt.Sprintf("container %s is rebuilt as Matroska", name)}
	}
	if fr.Has(scan.CodeMkvWarning) {
		return RouteRemux, []string{"mkvmerge reported warnings; rebuilding"}
	}
	if reasons := NeedsRemux(sc.Info, d); len(reasons) > 0 {
		return RouteRemux, reasons
	}
	addAll(fr, d.Findings)
	return RouteClean, nil
}

// NeedsRemux lists why a Matroska file does not match the decision. Empty
// means metadata cleaning is enough.
func NeedsRemux(m *probe.MediaInfo, d *policy.Decision) []string {
	if m == nil || d == nil {
		return nil
	}
	var out []string
	for _, t := range d.Tracks {
		switch t.Stream.Type {
		case "video", "audio", "subtitle":
		default:
			continue
		}
		if !t.Keep {
			out = append(out, fmt.Sprintf("drop #%d %s %s %s: %s", t.Stream.Index, t.Stream.Type, t.Stream.Codec, orUnd(t.Stream.Language), t.Reason))
			continue
		}
		if t.Stream.Type == "audio" || t.Stream.Type == "subtitle" {
			if t.Default != t.Stream.Default {
				out = append(out, fmt.Sprintf("#%d default flag %t -> %t", t.Stream.Index, t.Stream.Default, t.Default))
			}
			if t.Forced != t.Stream.Forced {
				out = append(out, fmt.Sprintf("#%d forced flag %t -> %t", t.Stream.Index, t.Stream.Forced, t.Forced))
			}
		}
		if t.SetLanguage != "" {
			out = append(out, fmt.Sprintf("#%d language und -> %s", t.Stream.Index, t.SetLanguage))
		}
	}
	for _, a := range d.Attachments {
		if !a.Keep {
			out = append(out, fmt.Sprintf("drop attachment #%d %s: %s", a.Attachment.ID, a.Attachment.FileName, a.Reason))
		}
	}
	if !d.KeepChapters && m.ChapterN > 0 {
		out = append(out, "chapters dropped")
	}
	return out
}

// withoutNoVideo returns a shallow copy of d whose findings omit NO_VIDEO.
func withoutNoVideo(d *policy.Decision) *policy.Decision {
	c := *d
	c.Findings = nil
	for _, f := range d.Findings {
		if f.Code != policy.CodeNoVideo {
			c.Findings = append(c.Findings, f)
		}
	}
	return &c
}

func orUnd(lang string) string {
	if lang == "" {
		return "und"
	}
	return lang
}
