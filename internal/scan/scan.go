// Package scan is the read-only gate. It turns one path into a FileResult
// with fixed finding codes and a verdict, using sniff, probe, mp4, policy and
// verify. It never modifies the file unless quarantine is requested.
package scan

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/nxame/amuxify/internal/exec"
	"github.com/nxame/amuxify/internal/fsutil"
	"github.com/nxame/amuxify/internal/mp4"
	"github.com/nxame/amuxify/internal/policy"
	"github.com/nxame/amuxify/internal/probe"
	"github.com/nxame/amuxify/internal/report"
	"github.com/nxame/amuxify/internal/sniff"
	"github.com/nxame/amuxify/internal/verify"
)

// Scanner holds the tools and policy for a run.
type Scanner struct {
	Runner   *exec.Runner
	Prober   *probe.Prober
	Verifier *verify.Verifier
	Profile  *policy.Profile
	// VerifyTier overrides the profile's verify.tier when non-empty.
	VerifyTier string
	// ClamAV forces a clamscan pass regardless of profile.
	ClamAV bool
	// Quarantine, when set, moves BLOCK files under this directory.
	Quarantine string
	// ScanLinks controls text-subtitle extraction; defaults from profile.
	Trace func(string)
}

// MediaExts are extensions treated as media to be probed.
var MediaExts = map[string]string{
	"mkv": "video", "mka": "audio", "mks": "subtitle", "webm": "video", "mp4": "video", "m4v": "video", "mov": "video",
	"avi": "video", "ts": "video", "m2ts": "video", "mts": "video", "mpg": "video", "mpeg": "video", "vob": "video",
	"flv": "video", "wmv": "video", "asf": "video", "3gp": "video", "ogv": "video",
	"mp3": "audio", "flac": "audio", "wav": "audio", "aac": "audio", "m4a": "audio", "ogg": "audio", "oga": "audio", "opus": "audio", "wma": "audio",
}

// Result bundles the report and the probe so remux can reuse it.
type Result struct {
	File report.FileResult
	Info *probe.MediaInfo
	Kind sniff.Kind
}

// IsMedia reports whether a path has a media extension.
func IsMedia(path string) bool {
	_, ok := MediaExts[fsutil.Ext(path)]
	return ok
}

func (s *Scanner) tier() string {
	if s.VerifyTier != "" {
		return s.VerifyTier
	}
	return s.Profile.Verify.Tier
}

// ScanPath scans one file or every file under a directory.
func (s *Scanner) ScanPath(ctx context.Context, root string) ([]Result, error) {
	abs, err := fsutil.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		r := s.ScanFile(ctx, abs, filepath.Dir(abs))
		return []Result{r}, nil
	}
	var paths []string
	walkErr := filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if d.Name() == ".DS_Store" || strings.HasPrefix(d.Name(), ".amuxify-") {
			return nil
		}
		paths = append(paths, p)
		return nil
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(paths)
	var out []Result
	for _, p := range paths {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out = append(out, s.ScanFile(ctx, p, abs))
	}
	return out, nil
}

// ScanFile scans one file. root is used for quarantine tree mirroring.
func (s *Scanner) ScanFile(ctx context.Context, path, root string) Result {
	start := time.Now()
	r := Result{File: report.FileResult{Path: path, Info: map[string]string{}}}
	defer func() {
		r.File.Duration = time.Since(start)
		if r.File.Verdict == report.Block && s.Quarantine != "" {
			s.quarantine(&r.File, root)
		}
	}()
	fr := &r.File

	fi, err := os.Lstat(path)
	if err != nil {
		fr.Addf(CodeUnreadable, report.Fail, "%v", err)
		return r
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		fr.Addf(CodeSymlink, report.Warn, "symlink skipped")
		return r
	}
	name := filepath.Base(path)
	if bad := bidiChars(name); bad != "" {
		fr.Addf(CodeBidiName, report.Block, "filename contains %s (extension spoofing)", bad)
		return r
	}
	if fi.Size() == 0 {
		fr.Addf(CodeEmpty, report.Block, "empty file")
		return r
	}
	if fi.Mode().Perm()&0o111 != 0 {
		switch s.Profile.Safety.ExecPermissions {
		case "fail":
			fr.Addf(CodeExecPerm, report.Fail, "executable permission bit set (%s)", fi.Mode().Perm())
		case "warn":
			fr.Addf(CodeExecPerm, report.Warn, "executable permission bit set (%s)", fi.Mode().Perm())
		}
	}
	if n := fsutil.Nlink(fi); n > 1 {
		fr.Addf(CodeHardlinked, report.Pass, "file has %d hard links", n)
		fr.Info["nlink"] = fmt.Sprint(n)
	}
	ext := fsutil.Ext(path)
	if pen := penultimateExt(name); pen != "" && contains(s.Profile.Sidecars.Block, pen) {
		fr.Addf(CodeDoubleExt, report.Warn, "double extension .%s.%s", pen, ext)
	}

	kindRes, err := sniff.File(path)
	if err != nil {
		fr.Addf(CodeUnreadable, report.Fail, "%v", err)
		return r
	}
	r.Kind = kindRes.Kind
	fr.Info["kind"] = string(kindRes.Kind)

	category, isMedia := MediaExts[ext]
	if !isMedia {
		s.checkSidecar(fr, ext, kindRes)
		return r
	}

	// Content versus extension.
	if kindRes.Kind.Dangerous() {
		fr.Addf(CodeDangerous, report.Block, "%s content under a .%s name", kindRes.Detail+" "+string(kindRes.Kind), ext)
		return r
	}
	if allowed := sniff.ExtensionKinds(ext); allowed != nil && !kindIn(kindRes.Kind, allowed) && kindRes.Kind != sniff.Unknown {
		fr.Addf(CodeExtMismatch, report.Fail, "content is %s but extension is .%s", kindRes.Kind, ext)
	}
	if poly := s.polyglot(path, fi.Size()); poly != "" {
		fr.Addf(CodePolyglot, report.Block, "%s", poly)
		return r
	}
	if s.clamav(ctx, fr, path) {
		return r
	}

	info, err := s.Prober.Probe(ctx, path)
	if err != nil {
		fr.Addf(CodeUnparseable, report.Fail, "%v", err)
		if r.Kind == sniff.MP4 {
			// Explain the common case: a download or copy that stopped early.
			if in, perr := mp4.Parse(path); perr == nil && (!in.HasMoov || in.Truncated) {
				note := in.TruncNote
				if note == "" {
					note = "no moov box: file is truncated or not finalised"
				}
				fr.Addf(CodeTruncated, report.Fail, "%s", note)
			} else if perr != nil {
				fr.Addf(CodeTruncated, report.Fail, "mp4 structure: %v", perr)
			}
		}
		return r
	}
	r.Info = info
	fr.Info["container"] = info.Container
	if info.Duration <= 0 && category != "subtitle" {
		fr.Addf(CodeNoDuration, report.Warn, "container reports no duration")
	}
	s.checkStreams(fr, info, category)
	s.checkMkv(ctx, fr, info)
	if info.Container == "mp4" {
		s.checkMP4(fr, path)
	}
	s.checkLinks(ctx, fr, info)
	s.checkProvenanceInfo(fr, info)
	if fr.Verdict >= report.Fail {
		return r
	}
	s.decodeCheck(ctx, fr, path)
	return r
}

func (s *Scanner) checkSidecar(fr *report.FileResult, ext string, k sniff.Result) {
	switch {
	case contains(s.Profile.Sidecars.Block, ext):
		fr.Addf(CodeSidecarBlocked, report.Block, "blocked sidecar type .%s", ext)
	case k.Kind.Dangerous():
		fr.Addf(CodeDangerous, report.Block, "%s content under a .%s name", k.Kind, ext)
	case contains(s.Profile.Sidecars.Allow, ext):
		if allowed := sniff.ExtensionKinds(ext); allowed != nil && !kindIn(k.Kind, allowed) && k.Kind != sniff.Unknown {
			fr.Addf(CodeExtMismatch, report.Fail, "content is %s but extension is .%s", k.Kind, ext)
			return
		}
		fr.Addf(CodeSidecarOK, report.Pass, "allowed sidecar .%s", ext)
	default:
		fr.Addf(CodeSidecarUnknown, report.Warn, "unrecognised sidecar type .%s (%s)", ext, k.Kind)
	}
}

func (s *Scanner) checkStreams(fr *report.FileResult, info *probe.MediaInfo, category string) {
	hasVideo, hasAudio := false, false
	for _, st := range info.Streams {
		switch st.Type {
		case "video":
			if !st.AttachedPic {
				hasVideo = true
				if len(st.HDR) > 0 {
					fr.Addf(CodeHDR, report.Pass, "stream #%d: %s", st.Index, strings.Join(st.HDR, "+"))
					fr.Info["hdr"] = strings.Join(st.HDR, "+")
				}
			}
		case "audio":
			hasAudio = true
			if policy.IsUnd(st.Language) && policy.IsUnd(st.LanguageIETF) {
				fr.Addf(CodeUndTrack, report.Pass, "stream #%d audio has no language tag", st.Index)
			}
		case "subtitle":
			if policy.IsUnd(st.Language) && policy.IsUnd(st.LanguageIETF) {
				fr.Addf(CodeUndTrack, report.Pass, "stream #%d subtitle has no language tag", st.Index)
			}
		case "data":
			if !contains(s.Profile.Streams.AllowDataTags, strings.ToLower(st.CodecTag)) {
				fr.Addf(CodeDataStream, report.Block, "stream #%d is a data stream (codec=%s tag=%q); allow with streams.allow_data_tags", st.Index, st.Codec, st.CodecTag)
			}
		case "attachment":
			if !info.IsMatroska() {
				fr.Addf(CodeAttachBlocked, report.Block, "stream #%d is an attachment in a non-Matroska container", st.Index)
			}
		}
	}
	switch category {
	case "video":
		if !hasVideo {
			fr.Addf(CodeNoVideo, report.Fail, "no video stream in a video container")
		}
	case "audio":
		if !hasAudio {
			fr.Addf(CodeNoAudio, report.Fail, "no audio stream in an audio container")
		}
	}
}

func (s *Scanner) checkMkv(ctx context.Context, fr *report.FileResult, info *probe.MediaInfo) {
	if !info.IsMatroska() {
		return
	}
	for _, e := range info.MkvErrors {
		fr.Addf(CodeMkvError, report.Fail, "mkvmerge: %s", e)
	}
	for _, w := range info.MkvWarnings {
		fr.Addf(CodeMkvWarning, report.Warn, "mkvmerge: %s", w)
	}
	if len(info.Attachments) == 0 {
		return
	}
	d := s.Profile.Decide(info, policy.Options{})
	for _, a := range d.Attachments {
		at := a.Attachment
		label := fmt.Sprintf("attachment #%d %q (%s, %d bytes)", at.ID, at.FileName, at.MimeType, at.Size)
		if k := s.sniffAttachment(ctx, info.Path, at); k != "" {
			fr.Addf(CodeAttachExec, report.Block, "%s contains %s", label, k)
			continue
		}
		switch {
		case a.Block:
			fr.Addf(CodeAttachBlocked, report.Block, "%s: %s", label, a.Reason)
		case a.Keep:
			fr.Addf(CodeAttachOK, report.Pass, "%s: %s", label, a.Reason)
		default:
			fr.Addf(CodeAttachDrop, report.Pass, "%s: %s", label, a.Reason)
		}
	}
}

// sniffAttachment extracts an attachment to a temp file and returns a
// description when its content is executable or an archive.
func (s *Scanner) sniffAttachment(ctx context.Context, path string, a probe.Attachment) string {
	if a.Size > 64<<20 || !s.Runner.Have(exec.MKVExtract) {
		return ""
	}
	tmp, err := os.CreateTemp("", "amuxify-att-*")
	if err != nil {
		return ""
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	res, err := s.Runner.RunWithTimeout(ctx, 2*time.Minute, exec.MKVExtract, path, "attachments", fmt.Sprintf("%d:%s", a.ID, tmp.Name()))
	if err != nil || res.ExitCode >= 2 {
		return ""
	}
	k, err := sniff.File(tmp.Name())
	if err != nil {
		return ""
	}
	if k.Kind.Dangerous() {
		return string(k.Kind) + " " + k.Detail
	}
	return ""
}

func (s *Scanner) checkMP4(fr *report.FileResult, path string) {
	in, err := mp4.Parse(path)
	if err != nil {
		fr.Addf(CodeUnparseable, report.Fail, "mp4 structure: %v", err)
		return
	}
	if !in.HasMoov {
		fr.Addf(CodeTruncated, report.Fail, "no moov box: file is truncated or not finalised")
	}
	if in.Truncated {
		fr.Addf(CodeTruncated, report.Fail, "%s", in.TruncNote)
	}
	sev := report.Warn
	if s.Profile.Metadata.Provenance == "fail" {
		sev = report.Fail
	}
	var ident, tools []string
	for _, k := range in.ProvenanceKeys() {
		desc, _ := mp4.IsProvenance(k)
		entry := fmt.Sprintf("%s (%s)", strings.TrimSpace(k), desc)
		if v := in.Ilst[k]; v != "" && len(v) < 80 {
			entry += " = " + v
		}
		switch k {
		case "©too", "©enc", "©swr", "----:com.apple.iTunes:Encoding Params", "----:com.apple.iTunes:iTunNORM":
			tools = append(tools, entry)
		default:
			ident = append(ident, entry)
		}
	}
	if len(ident) > 0 {
		fr.Add(report.Finding{Code: CodePurchaseAtom, Severity: sev,
			Message: fmt.Sprintf("%d identifying metadata atom(s): %s", len(ident), strings.Join(ident, "; ")),
			Detail:  strings.Join(ident, "\n")})
	}
	if in.HasXMP {
		fr.Add(report.Finding{Code: CodePurchaseAtom, Severity: sev, Message: "XMP metadata box present"})
	}
	if len(tools) > 0 {
		fr.Add(report.Finding{Code: CodeProvenanceInfo, Severity: report.Pass,
			Message: "encoder fingerprint: " + strings.Join(tools, "; ")})
	}
}

func (s *Scanner) checkLinks(ctx context.Context, fr *report.FileResult, info *probe.MediaInfo) {
	sev := report.Warn
	if s.Profile.Metadata.Links == "fail" {
		sev = report.Fail
	}
	var hits []string
	add := func(where, text string) {
		for _, l := range FindLinks(text) {
			hits = append(hits, where+": "+l)
		}
	}
	add("title", info.Title)
	for k, v := range info.Tags {
		add("tag "+k, v)
	}
	for _, st := range info.Streams {
		add(fmt.Sprintf("stream #%d title", st.Index), st.Title)
		for k, v := range st.Tags {
			if strings.EqualFold(k, "title") || strings.EqualFold(k, "language") {
				continue
			}
			add(fmt.Sprintf("stream #%d tag %s", st.Index, k), v)
		}
	}
	for i, c := range info.Chapters {
		add(fmt.Sprintf("chapter %d", i+1), c.Title)
	}
	for _, a := range info.Attachments {
		add("attachment name", a.FileName)
	}
	if len(hits) > 0 {
		fr.Add(report.Finding{Code: CodeLinkInTag, Severity: sev,
			Message: fmt.Sprintf("%d link(s) in metadata: %s", len(hits), strings.Join(hits, "; ")), Detail: strings.Join(hits, "\n")})
	}
	if !s.Profile.Subtitles.ScanLinks {
		return
	}
	ssev := report.Warn
	if s.Profile.Subtitles.Links == "fail" {
		ssev = report.Fail
	}
	for _, st := range info.Streams {
		if st.Type != "subtitle" || !st.TextSubtitle {
			continue
		}
		res, err := s.Runner.RunWithTimeout(ctx, 5*time.Minute, exec.FFmpeg,
			"-v", "error", "-i", info.Path, "-map", fmt.Sprintf("0:%d", st.Index), "-f", "srt", "-")
		if err != nil || res.ExitCode != 0 {
			continue
		}
		if links := FindLinks(string(res.Stdout)); len(links) > 0 {
			fr.Add(report.Finding{Code: CodeLinkInSubs, Severity: ssev,
				Message: fmt.Sprintf("stream #%d subtitle text contains %d link(s): %s", st.Index, len(links), strings.Join(links, ", ")),
				Detail:  strings.Join(links, "\n")})
		}
	}
}

func (s *Scanner) checkProvenanceInfo(fr *report.FileResult, info *probe.MediaInfo) {
	var parts []string
	if info.WritingApp != "" {
		parts = append(parts, "writing app "+info.WritingApp)
	}
	if info.MuxingApp != "" {
		parts = append(parts, "muxing app "+info.MuxingApp)
	}
	for k, v := range info.Tags {
		switch strings.ToLower(k) {
		case "encoder", "encoded_by", "comment", "description", "creation_time", "handler_name":
			parts = append(parts, k+"="+v)
		}
	}
	if info.GlobalTagN > 0 {
		parts = append(parts, fmt.Sprintf("%d global tag(s)", info.GlobalTagN))
	}
	if info.TrackTagN > 0 {
		parts = append(parts, fmt.Sprintf("%d track tag(s)", info.TrackTagN))
	}
	if info.Title != "" {
		parts = append(parts, "title "+info.Title)
	}
	if len(parts) > 0 {
		sort.Strings(parts)
		fr.Add(report.Finding{Code: CodeProvenanceInfo, Severity: report.Pass, Message: strings.Join(parts, "; ")})
	}
}

func (s *Scanner) decodeCheck(ctx context.Context, fr *report.FileResult, path string) {
	var err error
	switch s.tier() {
	case "none":
		return
	case "full":
		err = s.Verifier.DecodeFull(ctx, path)
	default:
		err = s.Verifier.DecodeHeadTail(ctx, path)
	}
	if err != nil {
		fr.Addf(CodeDecodeFail, report.Fail, "%v", err)
	}
}

func (s *Scanner) clamav(ctx context.Context, fr *report.FileResult, path string) bool {
	mode := s.Profile.Safety.ClamAV
	if s.ClamAV && mode == "off" {
		mode = "optional"
	}
	if mode == "off" {
		return false
	}
	if !s.Runner.Have(exec.ClamScan) {
		if mode == "required" {
			fr.Addf(CodeClamMissing, report.Fail, "clamscan required by profile but not installed")
			return true
		}
		return false
	}
	res, err := s.Runner.RunWithTimeout(ctx, 30*time.Minute, exec.ClamScan, "--no-summary", "--infected", "--", path)
	if err != nil {
		fr.Addf(CodeClamError, report.Warn, "%v", err)
		return false
	}
	switch res.ExitCode {
	case 0:
		return false
	case 1:
		fr.Add(report.Finding{Code: CodeClamInfected, Severity: report.Block, Message: "clamscan reports infected", Detail: strings.TrimSpace(string(res.Stdout))})
		return true
	default:
		fr.Addf(CodeClamError, report.Warn, "clamscan exit %d: %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		return false
	}
}

var polySigs = [][]byte{
	[]byte("PK\x03\x04"), []byte("PK\x05\x06"), []byte("Rar!\x1a\x07"), []byte("7z\xbc\xaf\x27\x1c"), []byte("\x7fELF"),
	[]byte("This program cannot be run in DOS mode"),
}

// polyglot looks for archive or executable signatures in the last 1 MiB and
// for the PE stub string in the first 1 MiB. Matroska and MP4 payloads are
// compressed video, so these strings essentially never occur by accident.
func (s *Scanner) polyglot(path string, size int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	const win = 1 << 20
	head := make([]byte, minInt64(win, size))
	if _, err := io.ReadFull(f, head); err != nil && err != io.ErrUnexpectedEOF {
		return ""
	}
	if bytes.Contains(head, polySigs[5]) {
		return "PE executable stub string inside file"
	}
	if size > win {
		tail := make([]byte, win)
		if _, err := f.ReadAt(tail, size-win); err != nil && err != io.EOF {
			return ""
		}
		head = tail
	}
	for _, sig := range polySigs[:5] {
		if i := bytes.LastIndex(head, sig); i >= 0 {
			return fmt.Sprintf("archive/executable signature %q near end of file (appended payload)", string(sig[:2]))
		}
	}
	return ""
}

func (s *Scanner) quarantine(fr *report.FileResult, root string) {
	rel, err := filepath.Rel(root, fr.Path)
	if err != nil || strings.HasPrefix(rel, "..") {
		rel = filepath.Base(fr.Path)
	}
	dest := filepath.Join(s.Quarantine, rel)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		fr.Addf(CodeQuarantined, report.Warn, "quarantine failed: %v", err)
		return
	}
	if err := fsutil.PlaceNoClobber(fr.Path, dest); err != nil {
		fr.Addf(CodeQuarantined, report.Warn, "quarantine failed: %v", err)
		return
	}
	fr.Addf(CodeQuarantined, report.Block, "moved to %s", dest)
}

func bidiChars(name string) string {
	for _, r := range name {
		switch r {
		case 0x202A, 0x202B, 0x202C, 0x202D, 0x202E, 0x200E, 0x200F, 0x2066, 0x2067, 0x2068, 0x2069:
			return fmt.Sprintf("bidi control U+%04X", r)
		case 0x200B, 0x200C, 0x200D, 0xFEFF, 0x2060:
			return fmt.Sprintf("zero-width character U+%04X", r)
		}
	}
	return ""
}

func penultimateExt(name string) string {
	parts := strings.Split(strings.ToLower(name), ".")
	if len(parts) < 3 {
		return ""
	}
	return parts[len(parts)-2]
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func kindIn(k sniff.Kind, list []sniff.Kind) bool {
	for _, x := range list {
		if x == k {
			return true
		}
	}
	return false
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
