package policy

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/nxame/amuxify/internal/probe"
	"github.com/nxame/amuxify/internal/report"
)

// Finding codes emitted by the decision engine.
const (
	CodeAudioFallback = "AUDIO_FALLBACK"
	CodeNoVideo       = "NO_VIDEO"
	CodeVideoExtra    = "VIDEO_EXTRA"
	CodeUndTrack      = "UND_TRACK"
)

// commentaryRe is word-bounded so "Director's Cut" or "Cast" in a title
// never matches; only the word commentary/commentaries does.
var commentaryRe = regexp.MustCompile(`(?i)\bcommentar(?:y|ies)\b`)

// TrackAction is the decision for one stream.
type TrackAction struct {
	Stream      probe.Stream `json:"stream"`
	Keep        bool         `json:"keep"`
	Reason      string       `json:"reason"`
	Default     bool         `json:"default"`
	Forced      bool         `json:"forced"`
	SetLanguage string       `json:"set_language,omitempty"`
	ClearTitle  bool         `json:"clear_title"`
}

// AttachmentAction is the decision for one attachment.
type AttachmentAction struct {
	Attachment probe.Attachment `json:"attachment"`
	Keep       bool             `json:"keep"`
	Block      bool             `json:"block"`
	Kind       string           `json:"kind"` // font image other
	Reason     string           `json:"reason"`
}

// Decision is the complete plan for one file.
type Decision struct {
	Tracks          []TrackAction      `json:"tracks"`
	Attachments     []AttachmentAction `json:"attachments"`
	KeepChapters    bool               `json:"keep_chapters"`
	StripTitle      bool               `json:"strip_title"`
	StripTags       bool               `json:"strip_tags"`
	StripProvenance bool               `json:"strip_provenance"`
	Findings        []report.Finding   `json:"findings,omitempty"`
}

// Options carry per-run context, for example the original language supplied
// by a Sonarr or Radarr hook.
type Options struct {
	OriginalLanguage string
}

// KeptOf returns kept actions of one stream type.
func (d *Decision) KeptOf(t string) []TrackAction {
	var out []TrackAction
	for _, a := range d.Tracks {
		if a.Keep && a.Stream.Type == t {
			out = append(out, a)
		}
	}
	return out
}

// Decide applies the profile to a probed file.
func (p *Profile) Decide(m *probe.MediaInfo, opt Options) *Decision {
	d := &Decision{
		KeepChapters:    p.Chapters.Keep,
		StripTitle:      p.Metadata.StripTitle,
		StripTags:       p.Metadata.StripTags,
		StripProvenance: p.Metadata.StripProvenance,
	}
	orig := ""
	if p.Languages.PreferOriginal && opt.OriginalLanguage != "" {
		orig = Canonical(opt.OriginalLanguage)
	}

	videoSeen := false
	var audio, subs []int
	for _, s := range m.Streams {
		a := TrackAction{Stream: s, ClearTitle: p.Metadata.StripTrackTitles}
		switch s.Type {
		case "video":
			switch {
			case s.AttachedPic:
				a.Reason = "cover art stream"
			case videoSeen:
				a.Reason = "secondary video stream"
				d.Findings = append(d.Findings, report.Finding{Code: CodeVideoExtra, Severity: report.Warn,
					Message: fmt.Sprintf("stream #%d: extra video stream dropped", s.Index)})
			default:
				a.Keep, a.Reason, videoSeen = true, "primary video", true
				a.Default = true
			}
		case "audio":
			audio = append(audio, len(d.Tracks))
			a.Keep, a.Reason, a.SetLanguage = p.wantTrack(s)
			if a.Keep && p.Audio.DropCommentary && (s.Commentary || commentaryRe.MatchString(s.Title)) {
				a.Keep, a.Reason = false, "commentary"
			}
			a.Forced = s.Forced
		case "subtitle":
			subs = append(subs, len(d.Tracks))
			a.Keep, a.Reason, a.SetLanguage = p.wantTrack(s)
			if !a.Keep && p.Subtitles.KeepForced && s.Forced {
				a.Keep, a.Reason = true, "forced subtitle"
			}
			if a.Keep && p.Audio.DropCommentary && (s.Commentary || commentaryRe.MatchString(s.Title)) {
				a.Keep, a.Reason = false, "commentary"
			}
			a.Forced = s.Forced
			a.Default = s.Default
		case "attachment":
			a.Reason = "attachment stream handled by attachment policy"
		default:
			a.Reason = "data stream"
		}
		d.Tracks = append(d.Tracks, a)
	}
	if !videoSeen {
		d.Findings = append(d.Findings, report.Finding{Code: CodeNoVideo, Severity: report.Fail, Message: "no video stream"})
	}

	// Audio: never produce a silent file. If policy dropped everything, keep
	// the original set and say so.
	if len(audio) > 0 && len(d.KeptOf("audio")) == 0 {
		for _, i := range audio {
			d.Tracks[i].Keep = true
			d.Tracks[i].Reason = "kept: policy would have removed every audio track"
		}
		d.Findings = append(d.Findings, report.Finding{Code: CodeAudioFallback, Severity: report.Warn,
			Message: "no audio track matched the language policy; all audio kept"})
	}
	if p.Audio.KeepFirst {
		seen := false
		for _, i := range audio {
			if d.Tracks[i].Keep {
				if seen {
					d.Tracks[i].Keep, d.Tracks[i].Reason = false, "keep_first"
				}
				seen = true
			}
		}
	}
	p.assignAudioDefault(d, audio, orig)

	if p.Subtitles.DropSDHDuplicates {
		langs := map[string]bool{}
		for _, i := range subs {
			t := &d.Tracks[i]
			if t.Keep && !t.Stream.HearingImp {
				langs[Canonical(t.Stream.Language)] = true
			}
		}
		for _, i := range subs {
			t := &d.Tracks[i]
			if t.Keep && t.Stream.HearingImp && langs[Canonical(t.Stream.Language)] {
				t.Keep, t.Reason = false, "SDH duplicate"
			}
		}
	}

	for _, at := range m.Attachments {
		d.Attachments = append(d.Attachments, p.attachmentAction(at, m.HasTextSubtitles()))
	}
	for _, t := range d.Tracks {
		if t.Keep && (t.Stream.Type == "audio" || t.Stream.Type == "subtitle") && IsUnd(t.Stream.Language) && t.SetLanguage == "" {
			d.Findings = append(d.Findings, report.Finding{Code: CodeUndTrack, Severity: report.Pass,
				Message: fmt.Sprintf("stream #%d (%s) has no language tag; kept as und (languages.und = keep)", t.Stream.Index, t.Stream.Type)})
		}
	}
	return d
}

// wantTrack applies the language and und rules.
func (p *Profile) wantTrack(s probe.Stream) (keep bool, reason, setLang string) {
	if IsUnd(s.Language) && IsUnd(s.LanguageIETF) {
		switch {
		case p.Languages.Und == "drop":
			return false, "untagged language (und = drop)", ""
		case strings.HasPrefix(p.Languages.Und, "assume:"):
			lang := Canonical(strings.TrimPrefix(p.Languages.Und, "assume:"))
			if p.LanguageMatches(lang, "") {
				return true, "untagged language assumed " + lang, lang
			}
			return false, "untagged language assumed " + lang + ", not kept", ""
		default:
			return true, "untagged language kept", ""
		}
	}
	if p.LanguageMatches(s.Language, s.LanguageIETF) {
		return true, "language " + Canonical(s.Language), ""
	}
	return false, "language " + Canonical(s.Language) + " not in keep list", ""
}

// assignAudioDefault guarantees exactly one default audio among kept tracks.
func (p *Profile) assignAudioDefault(d *Decision, audio []int, orig string) {
	var kept []int
	for _, i := range audio {
		if d.Tracks[i].Keep {
			kept = append(kept, i)
		}
	}
	if len(kept) == 0 {
		return
	}
	chosen := -1
	if orig != "" {
		for _, i := range kept {
			t := d.Tracks[i]
			l := t.SetLanguage
			if l == "" {
				l = t.Stream.Language
			}
			if Canonical(l) == orig {
				chosen = i
				break
			}
		}
	}
	if chosen < 0 {
		for _, i := range kept {
			if d.Tracks[i].Stream.Default {
				chosen = i
				break
			}
		}
	}
	if chosen < 0 {
		chosen = kept[0]
	}
	for _, i := range kept {
		d.Tracks[i].Default = i == chosen
	}
}

var fontMimes = map[string]bool{
	"font/ttf": true, "font/otf": true, "font/collection": true, "font/sfnt": true,
	"application/x-truetype-font": true, "application/vnd.ms-opentype": true,
	"application/font-sfnt": true, "application/x-font-ttf": true, "application/x-font-otf": true,
	"application/x-font": true,
}

// AttachmentKind classifies an attachment by MIME type and file extension.
func AttachmentKind(a probe.Attachment) string {
	mt := strings.ToLower(a.MimeType)
	ext := strings.ToLower(strings.TrimPrefix(path.Ext(a.FileName), "."))
	switch {
	case fontMimes[mt], ext == "ttf", ext == "otf", ext == "ttc":
		return "font"
	case strings.HasPrefix(mt, "image/"), ext == "jpg", ext == "jpeg", ext == "png", ext == "webp":
		return "image"
	}
	return "other"
}

func (p *Profile) attachmentAction(a probe.Attachment, hasTextSubs bool) AttachmentAction {
	act := AttachmentAction{Attachment: a, Kind: AttachmentKind(a)}
	switch act.Kind {
	case "font":
		switch p.Attachments.Fonts {
		case "keep":
			act.Keep, act.Reason = true, "font kept"
		case "keep_if_text_subs":
			if hasTextSubs {
				act.Keep, act.Reason = true, "font kept for text subtitles"
			} else {
				act.Reason = "font dropped: no text subtitles"
			}
		default:
			act.Reason = "font dropped by policy"
		}
	case "image":
		if p.Attachments.CoverArt == "keep" {
			act.Keep, act.Reason = true, "cover art kept"
		} else {
			act.Reason = "cover art dropped by policy"
		}
	default:
		if p.Attachments.Other == "block" {
			act.Block, act.Reason = true, "non-font, non-image attachment"
		} else {
			act.Reason = "attachment dropped by policy"
		}
	}
	return act
}
