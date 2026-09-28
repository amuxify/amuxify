// Package probe builds one MediaInfo from ffprobe's JSON and, for Matroska
// files, mkvmerge's identification JSON. Every downstream decision reads
// MediaInfo, never tool output.
package probe

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
)

// Stream is one elementary stream.
type Stream struct {
	Index        int               `json:"index"`
	MkvID        int               `json:"mkv_id"`
	Type         string            `json:"type"` // video audio subtitle attachment data
	Codec        string            `json:"codec"`
	CodecTag     string            `json:"codec_tag,omitempty"`
	Language     string            `json:"language"` // ISO 639-2/B code or "und"
	LanguageIETF string            `json:"language_ietf,omitempty"`
	Title        string            `json:"title,omitempty"`
	Default      bool              `json:"default"`
	Forced       bool              `json:"forced"`
	Commentary   bool              `json:"commentary"`
	HearingImp   bool              `json:"hearing_impaired"`
	VisualImp    bool              `json:"visual_impaired"`
	Original     bool              `json:"original"`
	AttachedPic  bool              `json:"attached_pic"`
	Channels     int               `json:"channels,omitempty"`
	SampleRate   int               `json:"sample_rate,omitempty"`
	Width        int               `json:"width,omitempty"`
	Height       int               `json:"height,omitempty"`
	PixFmt       string            `json:"pix_fmt,omitempty"`
	ColorTrc     string            `json:"color_transfer,omitempty"`
	ColorPrim    string            `json:"color_primaries,omitempty"`
	HDR          []string          `json:"hdr,omitempty"` // hdr10 hlg dovi hdr10plus
	Color        Color             `json:"color"`         // the full colour and HDR signalling of a video stream
	TextSubtitle bool              `json:"text_subtitle"`
	Tags         map[string]string `json:"tags,omitempty"`
}

// Attachment is a Matroska attachment.
type Attachment struct {
	ID       int    `json:"id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	Size     int64  `json:"size"`
}

// Chapter is one chapter entry.
type Chapter struct {
	Start float64 `json:"start"`
	End   float64 `json:"end"`
	Title string  `json:"title,omitempty"`
}

// MediaInfo is the merged view of one file.
type MediaInfo struct {
	Path          string            `json:"path"`
	Container     string            `json:"container"` // matroska webm mp4 mov avi mpegts mpegps flv asf ogg flac wav mp3 subtitle
	FormatName    string            `json:"format_name"`
	Duration      float64           `json:"duration"`
	Size          int64             `json:"size"`
	BitRate       int64             `json:"bit_rate"`
	Title         string            `json:"title,omitempty"`
	Tags          map[string]string `json:"tags,omitempty"`
	Streams       []Stream          `json:"streams"`
	Attachments   []Attachment      `json:"attachments"`
	Chapters      []Chapter         `json:"chapters"`
	ChapterN      int               `json:"chapter_count"`
	GlobalTagN    int               `json:"global_tag_count"`
	TrackTagN     int               `json:"track_tag_count"`
	MuxingApp     string            `json:"muxing_app,omitempty"`
	WritingApp    string            `json:"writing_app,omitempty"`
	SegmentUID    string            `json:"segment_uid,omitempty"`
	MkvWarnings   []string          `json:"mkv_warnings,omitempty"`
	MkvErrors     []string          `json:"mkv_errors,omitempty"`
	MkvIdentified bool              `json:"mkv_identified"`
	MkvSupported  bool              `json:"mkv_supported"`
}

// IsMatroska reports whether mkvmerge identified the file as Matroska/WebM.
func (m *MediaInfo) IsMatroska() bool { return m.Container == "matroska" || m.Container == "webm" }

// StreamsOf returns streams of one type in index order.
func (m *MediaInfo) StreamsOf(t string) []Stream {
	var out []Stream
	for _, s := range m.Streams {
		if s.Type == t {
			out = append(out, s)
		}
	}
	return out
}

// HasTextSubtitles reports whether any text-based subtitle track exists.
func (m *MediaInfo) HasTextSubtitles() bool {
	for _, s := range m.Streams {
		if s.Type == "subtitle" && s.TextSubtitle {
			return true
		}
	}
	return false
}

// Prober runs the tools.
type Prober struct {
	Runner  *exec.Runner
	Timeout time.Duration
}

// Probe identifies a file. ffprobe failure is returned as an error; mkvmerge
// failure on a Matroska file is recorded in MkvErrors.
func (p *Prober) Probe(ctx context.Context, path string) (*MediaInfo, error) {
	to := p.Timeout
	if to == 0 {
		to = 60 * time.Second
	}
	res, err := p.Runner.RunWithTimeout(ctx, to, exec.FFprobe,
		"-v", "error", "-print_format", "json", "-show_format", "-show_streams", "-show_chapters", "-show_error", "--", path)
	if err != nil {
		return nil, err
	}
	if res.OutputTruncated {
		// The runner kept only the first part of the document, which would
		// fail to parse below; the reason is named instead so the report
		// does not call a file unparseable for the wrong reason.
		return nil, fmt.Errorf("ffprobe: output was longer than the runner keeps (%d bytes) and was cut", len(res.Stdout))
	}
	var fp ffprobeOut
	if jerr := json.Unmarshal(res.Stdout, &fp); jerr != nil {
		return nil, fmt.Errorf("ffprobe: unparseable output: %v", jerr)
	}
	if res.ExitCode != 0 || fp.Error != nil {
		msg := strings.TrimSpace(string(res.Stderr))
		if fp.Error != nil {
			msg = fp.Error.String
		}
		return nil, fmt.Errorf("ffprobe: %s", msg)
	}
	m := fromFFprobe(path, &fp)
	if m.Container != "subtitle" && p.Runner.Have(exec.MKVMerge) {
		p.mergeMkv(ctx, to, m)
	}
	return m, nil
}

type ffprobeOut struct {
	Streams []struct {
		Index          int               `json:"index"`
		CodecName      string            `json:"codec_name"`
		CodecType      string            `json:"codec_type"`
		CodecTagString string            `json:"codec_tag_string"`
		Width          int               `json:"width"`
		Height         int               `json:"height"`
		PixFmt         string            `json:"pix_fmt"`
		ColorTransfer  string            `json:"color_transfer"`
		ColorPrimaries string            `json:"color_primaries"`
		ColorSpace     string            `json:"color_space"`
		ColorRange     string            `json:"color_range"`
		Channels       int               `json:"channels"`
		SampleRate     string            `json:"sample_rate"`
		Disposition    map[string]int    `json:"disposition"`
		Tags           map[string]string `json:"tags"`
		// SideData is kept raw and decoded entry by entry, so that one
		// malformed entry, or a list of the wrong shape, cannot make the
		// whole probe fail or hide the other entries.
		SideData json.RawMessage `json:"side_data_list"`
	} `json:"streams"`
	Chapters []struct {
		Start string            `json:"start_time"`
		End   string            `json:"end_time"`
		Tags  map[string]string `json:"tags"`
	} `json:"chapters"`
	Format struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		Size       string            `json:"size"`
		BitRate    string            `json:"bit_rate"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
	Error *struct {
		String string `json:"string"`
	} `json:"error"`
}

var textSubCodecs = map[string]bool{
	"subrip": true, "srt": true, "ass": true, "ssa": true, "webvtt": true, "mov_text": true, "text": true, "ttml": true,
}

func fromFFprobe(path string, fp *ffprobeOut) *MediaInfo {
	m := &MediaInfo{Path: path, FormatName: fp.Format.FormatName, Tags: fp.Format.Tags}
	m.Container = containerOf(fp.Format.FormatName)
	m.Duration, _ = strconv.ParseFloat(fp.Format.Duration, 64)
	m.Size, _ = strconv.ParseInt(fp.Format.Size, 10, 64)
	m.BitRate, _ = strconv.ParseInt(fp.Format.BitRate, 10, 64)
	m.Title = tagCI(fp.Format.Tags, "title")
	for _, s := range fp.Streams {
		st := Stream{
			Index: s.Index, MkvID: -1, Type: s.CodecType, Codec: s.CodecName, CodecTag: s.CodecTagString,
			Width: s.Width, Height: s.Height, PixFmt: s.PixFmt, ColorTrc: s.ColorTransfer, ColorPrim: s.ColorPrimaries,
			Channels: s.Channels, Tags: s.Tags,
		}
		st.SampleRate, _ = strconv.Atoi(s.SampleRate)
		st.Language = normLang(tagCI(s.Tags, "language"))
		st.Title = tagCI(s.Tags, "title")
		st.Default = s.Disposition["default"] == 1
		st.Forced = s.Disposition["forced"] == 1
		st.Commentary = s.Disposition["comment"] == 1
		st.HearingImp = s.Disposition["hearing_impaired"] == 1
		st.VisualImp = s.Disposition["visual_impaired"] == 1
		st.Original = s.Disposition["original"] == 1
		st.AttachedPic = s.Disposition["attached_pic"] == 1
		if s.CodecType == "subtitle" {
			st.TextSubtitle = textSubCodecs[s.CodecName]
		}
		if s.CodecType == "video" {
			st.HDR = hdrLabels(s.ColorTransfer, s.SideData)
			st.Color = colorFromFFprobe(s.ColorPrimaries, s.ColorTransfer, s.ColorSpace, s.ColorRange, s.SideData)
		}
		m.Streams = append(m.Streams, st)
	}
	for _, c := range fp.Chapters {
		ch := Chapter{Title: tagCI(c.Tags, "title")}
		ch.Start, _ = strconv.ParseFloat(c.Start, 64)
		ch.End, _ = strconv.ParseFloat(c.End, 64)
		m.Chapters = append(m.Chapters, ch)
	}
	m.ChapterN = len(m.Chapters)
	return m
}

func containerOf(formatName string) string {
	f := strings.Split(formatName, ",")[0]
	switch formatName {
	case "matroska,webm":
		return "matroska"
	case "mov,mp4,m4a,3gp,3g2,mj2":
		return "mp4"
	}
	switch f {
	case "mpegts":
		return "mpegts"
	case "mpeg":
		return "mpegps"
	case "avi", "flv", "asf", "ogg", "flac", "wav", "mp3", "webm":
		return f
	case "srt", "ass", "webvtt", "microdvd", "vobsub":
		return "subtitle"
	}
	return f
}

func tagCI(tags map[string]string, key string) string {
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return v
		}
	}
	return ""
}

// normLang lower-cases and maps empty to und.
func normLang(l string) string {
	l = strings.ToLower(strings.TrimSpace(l))
	if l == "" {
		return "und"
	}
	return l
}

type mkvOut struct {
	Attachments []struct {
		ID          int    `json:"id"`
		FileName    string `json:"file_name"`
		ContentType string `json:"content_type"`
		Size        int64  `json:"size"`
	} `json:"attachments"`
	Chapters []struct {
		NumEntries int `json:"num_entries"`
	} `json:"chapters"`
	Container struct {
		Recognized bool   `json:"recognized"`
		Supported  bool   `json:"supported"`
		Type       string `json:"type"`
		Properties struct {
			MuxingApp  string `json:"muxing_application"`
			WritingApp string `json:"writing_application"`
			Title      string `json:"title"`
			SegmentUID string `json:"segment_uid"`
		} `json:"properties"`
	} `json:"container"`
	Errors     []string `json:"errors"`
	Warnings   []string `json:"warnings"`
	GlobalTags []struct {
		NumEntries int `json:"num_entries"`
	} `json:"global_tags"`
	TrackTags []struct {
		NumEntries int `json:"num_entries"`
		TrackID    int `json:"track_id"`
	} `json:"track_tags"`
	Tracks []struct {
		ID         int           `json:"id"`
		Type       string        `json:"type"`
		Codec      string        `json:"codec"`
		Properties mkvTrackProps `json:"properties"`
	} `json:"tracks"`
}

// mkvTrackFields are the typed track properties mergeMkv reads directly.
type mkvTrackFields struct {
	Language        string `json:"language"`
	LanguageIETF    string `json:"language_ietf"`
	TrackName       string `json:"track_name"`
	Default         bool   `json:"default_track"`
	Forced          bool   `json:"forced_track"`
	Commentary      bool   `json:"flag_commentary"`
	HearingImpaired bool   `json:"flag_hearing_impaired"`
	VisualImpaired  bool   `json:"flag_visual_impaired"`
	Original        bool   `json:"flag_original"`
	TextSubtitles   bool   `json:"text_subtitles"`
	CodecID         string `json:"codec_id"`
}

// mkvTrackProps decodes the typed fields and keeps every property raw as
// well, so the colour properties, whose keys mkvmerge has spelled two ways
// and whose values must be validated one by one, can be read leniently
// without a wrong type in one of them failing the whole identification.
type mkvTrackProps struct {
	mkvTrackFields
	raw map[string]json.RawMessage
}

func (p *mkvTrackProps) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, &p.mkvTrackFields); err != nil {
		return err
	}
	_ = json.Unmarshal(b, &p.raw)
	return nil
}

// IdentifyMkv runs mkvmerge -J and returns the raw parsed output plus the
// exit status. Status 1 means warnings only and is not a failure.
func (p *Prober) mergeMkv(ctx context.Context, to time.Duration, m *MediaInfo) {
	res, err := p.Runner.RunWithTimeout(ctx, to, exec.MKVMerge, "-J", m.Path)
	if err != nil {
		m.MkvErrors = append(m.MkvErrors, err.Error())
		return
	}
	if res.OutputTruncated {
		if m.IsMatroska() {
			m.MkvErrors = append(m.MkvErrors, fmt.Sprintf("mkvmerge -J output was longer than the runner keeps (%d bytes) and was cut", len(res.Stdout)))
		}
		return
	}
	var mk mkvOut
	if jerr := json.Unmarshal(res.Stdout, &mk); jerr != nil || res.ExitCode >= 2 || !mk.Container.Recognized {
		if !m.IsMatroska() {
			// Not an error for other containers: mkvmerge simply cannot read it.
			return
		}
		msg := strings.TrimSpace(string(res.Stderr))
		if msg == "" {
			msg = fmt.Sprintf("mkvmerge -J exit %d", res.ExitCode)
		}
		m.MkvErrors = append(m.MkvErrors, msg)
		m.MkvErrors = append(m.MkvErrors, mk.Errors...)
		return
	}
	m.MkvIdentified = true
	m.MkvSupported = mk.Container.Supported
	m.MkvWarnings = mk.Warnings
	m.MkvErrors = append(m.MkvErrors, mk.Errors...)
	m.MuxingApp = mk.Container.Properties.MuxingApp
	m.WritingApp = mk.Container.Properties.WritingApp
	m.SegmentUID = mk.Container.Properties.SegmentUID
	if m.Title == "" {
		m.Title = mk.Container.Properties.Title
	}
	for _, a := range mk.Attachments {
		m.Attachments = append(m.Attachments, Attachment{ID: a.ID, FileName: a.FileName, MimeType: a.ContentType, Size: a.Size})
	}
	for _, c := range mk.Chapters {
		if c.NumEntries > m.ChapterN {
			m.ChapterN = c.NumEntries
		}
	}
	for _, g := range mk.GlobalTags {
		m.GlobalTagN += g.NumEntries
	}
	for _, t := range mk.TrackTags {
		m.TrackTagN += t.NumEntries
	}
	// mkvmerge track ids and ffprobe stream indexes both follow track order
	// for Matroska; attachments are not streams in mkvmerge's view.
	byType := map[string][]int{}
	for i, s := range m.Streams {
		if s.Type == "attachment" {
			continue
		}
		byType[s.Type] = append(byType[s.Type], i)
	}
	seen := map[string]int{}
	for _, t := range mk.Tracks {
		typ := t.Type
		if typ == "subtitles" {
			typ = "subtitle"
		}
		idxs := byType[typ]
		k := seen[typ]
		seen[typ]++
		if k >= len(idxs) {
			continue
		}
		s := &m.Streams[idxs[k]]
		s.MkvID = t.ID
		if t.Properties.LanguageIETF != "" {
			s.LanguageIETF = t.Properties.LanguageIETF
		}
		if s.Language == "und" && t.Properties.Language != "" {
			s.Language = normLang(t.Properties.Language)
		}
		if s.Title == "" {
			s.Title = t.Properties.TrackName
		}
		if m.IsMatroska() {
			s.Default = t.Properties.Default
			s.Forced = t.Properties.Forced
		}
		s.Commentary = s.Commentary || t.Properties.Commentary
		s.HearingImp = s.HearingImp || t.Properties.HearingImpaired
		s.VisualImp = s.VisualImp || t.Properties.VisualImpaired
		s.Original = s.Original || t.Properties.Original
		if typ == "subtitle" {
			s.TextSubtitle = s.TextSubtitle || t.Properties.TextSubtitles
		}
		if typ == "video" && t.Properties.raw != nil {
			s.Color.merge(colorFromMkv(t.Properties.raw))
			s.HDR = addLabel(s.HDR, transferLabel(s.Color.Transfer))
		}
	}
}
