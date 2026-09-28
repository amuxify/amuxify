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
	"unicode"
	"unicode/utf8"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/mp4"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/pool"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/sniff"
	"github.com/amuxify/amuxify/internal/verify"
)

// Scanner holds the tools and policy for a run.
type Scanner struct {
	Runner   *exec.Runner
	Prober   *probe.Prober
	Verifier *verify.Verifier
	Profile  *policy.Profile
	// VerifyTier overrides the profile's verify.tier when non-empty.
	VerifyTier string
	// ClamAV forces a clamscan pass when the profile says off. It never
	// lowers a profile that says required.
	ClamAV bool
	// Timeout bounds the clamscan call; zero means DefaultClamTimeout. The
	// command line sets it from --timeout, as for the prober and verifier.
	Timeout time.Duration
	// Quarantine, when set, moves BLOCK files under this directory.
	Quarantine string
	// Progress, when set, receives each result as soon as the file is done.
	// With Jobs above one it is called from several goroutines, one file at
	// a time each, so it must be safe to call concurrently.
	Progress func(Result)
	Trace    func(string)
	// Jobs is the number of files ScanPath works on at once; zero or one
	// means one after the other in walk order.
	Jobs int

	// claims holds the quarantine destinations this run has taken, so two
	// files that would be moved to the same place, from two workers or
	// from two roots, produce exactly one quarantined file (guarantee 1).
	claims pool.Claims
}

// MediaExts are extensions treated as media to be probed.
var MediaExts = map[string]string{
	"mkv": "video", "mka": "audio", "mks": "subtitle", "webm": "video", "mp4": "video", "m4v": "video", "mov": "video",
	"avi": "video", "ts": "video", "m2ts": "video", "mts": "video", "mpg": "video", "mpeg": "video", "vob": "video",
	"flv": "video", "wmv": "video", "asf": "video", "3gp": "video", "ogv": "video",
	"mp3": "audio", "flac": "audio", "wav": "audio", "aac": "audio", "m4a": "audio", "ogg": "audio", "oga": "audio", "opus": "audio", "wma": "audio",
}

// Result bundles the report and the probe so remux can reuse it. Stat is
// the os.Lstat the scanner took of the file before it read a byte of it,
// so the verdict is bound to that file: the remuxer and the cleaner, which
// may act on the path long after the scan, require the entry at the path
// to still be that file with the same size and modification time before
// they open it or replace it, and refuse it otherwise. It is nil when the
// scanner could not examine the path.
type Result struct {
	File report.FileResult
	Info *probe.MediaInfo
	Kind sniff.Kind
	Stat os.FileInfo
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
	scanRoot := abs
	if !fi.IsDir() {
		scanRoot = filepath.Dir(abs)
	}
	if err := CheckQuarantineRoot(abs, s.Quarantine); err != nil {
		return nil, err
	}
	// Walk lists every readable entry and names the unreadable ones in
	// walkErr; those are reported at run level after the readable files.
	// A quarantine directory inside the tree is not entered.
	paths, walkErr := Walk(abs, QuarantineExcludes(s.Quarantine)...)
	keys := make([][]string, len(paths))
	for i, p := range paths {
		keys[i] = s.SerialKeys(p, scanRoot)
	}
	results := make([]Result, len(paths))
	ran := pool.Run(ctx, s.Jobs, len(paths), keys, func(i int) {
		r := s.ScanFile(ctx, paths[i], scanRoot)
		if s.Progress != nil {
			s.Progress(r)
		}
		results[i] = r
	})
	var out []Result
	for i, ok := range ran {
		if ok {
			out = append(out, results[i])
		}
	}
	if len(out) < len(paths) {
		return out, ctx.Err()
	}
	return out, walkErr
}

// SerialKeys lists what keeps path from being scanned beside another file
// of the same walk when Jobs is above one: its inode when it has other hard
// links, so two names of one file are handled one after the other, and the
// place quarantine would move it to, spelled in lower case, so two files
// that map to one quarantine destination are decided in walk order. A
// quarantine name that holds a character outside ASCII also keys its
// directory, for the filesystems that treat two Unicode spellings of one
// name as one entry; pool.PathKeys explains the rule.
func (s *Scanner) SerialKeys(path, root string) []string {
	var keys []string
	if fi, err := os.Lstat(path); err == nil && fi.Mode().IsRegular() {
		if k := fsutil.InodeKey(fi); k != "" {
			keys = append(keys, "inode:"+k)
		}
	}
	if s.Quarantine != "" {
		keys = append(keys, pool.PathKeys("quarantine", s.quarantineDest(path, root))...)
	}
	return keys
}

// ScanFile scans one file. root is used for quarantine tree mirroring.
//
// The result is named so that the deferred quarantine step, which runs after
// the verdict is final, records its QUARANTINED finding and the duration in
// the value the caller receives.
func (s *Scanner) ScanFile(ctx context.Context, path, root string) (r Result) {
	start := time.Now()
	r = Result{File: report.FileResult{Path: path, Info: map[string]string{}}}
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
	r.Stat = fi
	if fi.Mode()&os.ModeSymlink != 0 {
		fr.Addf(CodeSymlink, report.Warn, "symlink skipped")
		return r
	}
	// Anything else that is not a regular file is refused before any read:
	// a named pipe would block the first open until a writer appeared, and
	// a device or a socket is never media. The path is reported as
	// unreadable, which is what it is for this run.
	if !fi.Mode().IsRegular() {
		fr.Addf(CodeUnreadable, report.Fail, "%s; skipped", entryKind(fi.Mode()))
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
		s.checkSidecar(fr, path, ext, kindRes)
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
	if s.clamav(ctx, fr, path, fi.Size()) {
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
	s.decodeCheck(ctx, fr, path, info)
	return r
}

func (s *Scanner) checkSidecar(fr *report.FileResult, path, ext string, k sniff.Result) {
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
		if ext == "nfo" {
			s.checkNfo(fr, path)
		}
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
					// The detail lists the values the remux verifier
					// holds the output to, so a report shows what the
					// source carried.
					fr.Add(report.Finding{Code: CodeHDR, Severity: report.Pass, Message: fmt.Sprintf("stream #%d: %s", st.Index, strings.Join(st.HDR, "+")), Detail: st.Color.String()})
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
// description when its content is executable or an archive. The file is
// written under a directory this run creates with mode 0700 and a random
// name, so no other user can plant an entry at the path mkvextract is
// given, and it is read back with sniff.File, which refuses anything that
// is not a regular file without following or waiting on it. An attachment
// that cannot be extracted or read is not sniffed and the policy decides
// on its declared type alone.
func (s *Scanner) sniffAttachment(ctx context.Context, path string, a probe.Attachment) string {
	if a.Size > 64<<20 || !s.Runner.Have(exec.MKVExtract) {
		return ""
	}
	dir, err := os.MkdirTemp("", "amuxify-att-*")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	tmp := filepath.Join(dir, "attachment")
	res, err := s.Runner.RunWithTimeout(ctx, 2*time.Minute, exec.MKVExtract, path, "attachments", fmt.Sprintf("%d:%s", a.ID, tmp))
	if err != nil || res.ExitCode >= 2 {
		return ""
	}
	k, err := sniff.File(tmp)
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

// sortedKeys returns the keys of m in sorted order.
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
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
	// Tags are maps; their keys are visited in sorted order so the finding
	// reads the same on every run of the same file.
	add("title", info.Title)
	for _, k := range sortedKeys(info.Tags) {
		add("tag "+k, info.Tags[k])
	}
	for _, st := range info.Streams {
		add(fmt.Sprintf("stream #%d title", st.Index), st.Title)
		for _, k := range sortedKeys(st.Tags) {
			if strings.EqualFold(k, "title") || strings.EqualFold(k, "language") {
				continue
			}
			add(fmt.Sprintf("stream #%d tag %s", st.Index, k), st.Tags[k])
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
		if res.OutputTruncated {
			// The runner kept only the first part of the track, so a link
			// placed after the cut was never seen. The track is reported at
			// the same severity as a link, because a text track longer than
			// the runner keeps is not something a subtitle needs to be and a
			// file padded to reach the cut is exactly how a link would hide.
			fr.Add(report.Finding{Code: CodeLinkInSubs, Severity: ssev,
				Message: fmt.Sprintf("stream #%d subtitle text is longer than the runner keeps and was not fully checked for links", st.Index),
				Detail:  fmt.Sprintf("only the first %d bytes of the extracted track were inspected", len(res.Stdout))})
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

// decodeCheck runs the tiered decode pass. It is skipped for a container
// with no video or audio stream, such as a subtitle-only .mks, because
// ffmpeg cannot decode anything there and would fail a healthy file.
func (s *Scanner) decodeCheck(ctx context.Context, fr *report.FileResult, path string, info *probe.MediaInfo) {
	tier := s.tier()
	if tier == "none" {
		return
	}
	if err := s.Verifier.Decode(ctx, path, info, tier == "full"); err != nil {
		fr.Addf(CodeDecodeFail, report.Fail, "%v", err)
	}
}

// DefaultClamTimeout bounds one clamscan call when the run sets no
// --timeout. Loading the signature database and scanning a file of several
// gigabytes takes minutes on slow hardware; half an hour leaves room for
// that without letting a hung scanner hold the run forever.
const DefaultClamTimeout = 30 * time.Minute

// ClamMaxFileSize is the largest file one clamscan call is asked to scan,
// passed as --max-filesize. libclamav cannot scan a file of 2 GiB or more
// at all, and its own default limit is 100 MB, above which it reports the
// file clean without reading it; the value here is the largest whole
// number of MiB below the hard cap, so every file that libclamav can scan
// is scanned in full and every file it cannot scan is refused. A file
// larger than this is CLAMAV_ERROR before clamscan starts, in the same
// words for every version of the scanner, and --alert-exceeds-max makes
// clamscan itself exit 2 for one that grew past the limit after the stat.
const ClamMaxFileSize = 2047 << 20

// clamLimitArgs are the limit options every clamscan call carries. By
// default libclamav skips a file above --max-filesize, stops reading a
// file above --max-scansize and gives up on one that takes longer than
// --max-scantime, and in each case clamscan prints nothing under
// --infected and exits 0, so a required scan would pass a file that was
// never read. --alert-exceeds-max turns a skipped file into an alert
// instead. The scan size and scan time limits are then switched off rather
// than raised, because an alert on either of them exits 1, which is the
// exit status of an infected file, and a clean file that was merely slow
// or large must never be blocked and quarantined as one: the run's own
// timeout bounds a slow scan and reports it CLAMAV_ERROR, and the file
// size limit, whose alert exits 2, is the one limit left that can fire.
// --alert-exceeds-max needs ClamAV 0.103 or newer; an older clamscan
// refuses the option and exits with an error, which is CLAMAV_ERROR and
// under a required profile fails the file, and doctor reports the version
// as too old.
var clamLimitArgs = []string{
	fmt.Sprintf("--max-filesize=%dM", ClamMaxFileSize>>20),
	"--max-scansize=0",
	"--max-scantime=0",
	"--alert-exceeds-max",
}

// The bounds on what a finding keeps of the clamscan output. A scanner
// that floods its output, or an output crafted to look like a report,
// cannot grow the finding beyond these. The per-line and message bounds
// are applied to the raw text before it is sanitised, so an escaped
// control byte can make a kept line up to six times longer; the bound on
// the whole detail counts the sanitised lines and holds as written.
const (
	clamDetailLines  = 8    // lines kept in the finding detail
	clamLineBytes    = 512  // bytes kept of each line, before sanitising
	clamDetailBytes  = 4096 // bytes of sanitised lines kept in the whole detail
	clamMessageBytes = 200  // bytes kept of the error line in the message, before sanitising
)

func (s *Scanner) clamTimeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return DefaultClamTimeout
}

// MaxClamScans is how many clamscan processes amuxify runs at the same time
// in one process, whatever --jobs says. clamscan loads the whole signature
// database on every start, which takes well over a gigabyte of memory and
// tens of seconds, so one process per worker would exhaust the memory of
// most hosts long before the disks were busy. A worker whose file is due
// for clamscan waits for a slot; the other checks of the other workers go
// on meanwhile.
const MaxClamScans = 1

// clamSlots is the process-wide gate that enforces MaxClamScans. It is one
// for the whole process rather than one per Scanner because ingest and the
// hook adapters build their own Scanner and the memory is shared either way.
var clamSlots = make(chan struct{}, MaxClamScans)

// clamav runs clamscan when the profile or the --clamav flag asks for it
// and reports the outcome. The verdict rests on the exit status alone
// (0 clean, 1 infected, anything else an error): nothing printed by the
// scanner can raise or lower it. What is kept of the output for the
// finding is bounded and sanitised by clamDetail. It returns true when the
// scan of this file should stop here.
//
// A scanner that produced no verdict, because it could not start, was
// killed at the timeout or exited with a status other than 0 or 1, is
// WARN CLAMAV_ERROR under an optional scan and the file is probed like
// any other. Under a required scan the same outcome is FAIL CLAMAV_ERROR
// and the scan of the file stops there, the same as a missing scanner:
// a profile that requires the scan must not let a file through that was
// never scanned, and both a short --timeout and a scanner without a
// signature database would otherwise turn "required" into a pass.
//
// A file larger than ClamMaxFileSize, which libclamav cannot scan, is the
// same outcome without starting the scanner: size is the file's size from
// the scan's own stat, and the finding says in plain words why the file
// was not scanned. The limit is also passed to clamscan (clamLimitArgs),
// so a file that grew past it after the stat is refused by the scanner
// itself rather than reported clean unread.
func (s *Scanner) clamav(ctx context.Context, fr *report.FileResult, path string, size int64) bool {
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
	sev, stop := report.Warn, false
	if mode == "required" {
		sev, stop = report.Fail, true
	}
	if size > ClamMaxFileSize {
		fr.Addf(CodeClamError, sev, "file is %d bytes and clamscan cannot scan files larger than %d MiB (libclamav does not support files of 2 GiB or more), so it was not scanned", size, ClamMaxFileSize>>20)
		return stop
	}
	// The wait for a slot gives up when the run is cancelled, so an
	// interrupt is not held behind another worker's clamscan.
	select {
	case clamSlots <- struct{}{}:
		defer func() { <-clamSlots }()
	case <-ctx.Done():
		fr.Addf(CodeClamError, sev, "%v", ctx.Err())
		return stop
	}
	args := append([]string{"--no-summary", "--infected"}, clamLimitArgs...)
	args = append(args, "--", path)
	res, err := s.Runner.RunWithTimeout(ctx, s.clamTimeout(), exec.ClamScan, args...)
	if err != nil {
		// A start failure or a timeout: the child was killed or never ran,
		// so there is no exit status to read a verdict from.
		fr.Addf(CodeClamError, sev, "%s", clamText(err.Error(), clamMessageBytes))
		return stop
	}
	switch res.ExitCode {
	case 0:
		return false
	case 1:
		fr.Add(report.Finding{Code: CodeClamInfected, Severity: report.Block, Message: "clamscan reports infected", Detail: clamDetail(res, path)})
		return true
	default:
		fr.Add(report.Finding{Code: CodeClamError, Severity: sev,
			Message: clamErrorMessage(res, path),
			Detail:  clamDetail(res, path)})
		return stop
	}
}

// clamErrorMessage is the message of a CLAMAV_ERROR finding for a scanner
// that exited with a status other than 0 or 1. It carries the first line
// of standard error, where libclamav reports a failure to load the
// signature database, or, when that is empty, the first line about the
// scanned file on standard output, where clamscan reports a file it could
// not open ("<path>: Can't open file ERROR"). When neither says anything
// the message says so rather than ending in a bare colon.
func clamErrorMessage(res *exec.Result, path string) string {
	line := firstLine(res.Stderr)
	if line == "" {
		line = firstLineWithPrefix(res.Stdout, path+":")
	}
	if line == "" {
		return fmt.Sprintf("clamscan exit %d with no message", res.ExitCode)
	}
	return fmt.Sprintf("clamscan exit %d: %s", res.ExitCode, clamText(line, clamMessageBytes))
}

// clamDetail returns the lines of the clamscan output that refer to the
// scanned file, which clamscan prints as "<path>: <signature> FOUND" or
// "<path>: <reason> ERROR", each cut to clamLineBytes and sanitised, at
// most clamDetailLines of them and clamDetailBytes of sanitised text in
// all. Every other line is dropped: a summary block, a line about some
// other path, or text crafted to read like a verdict never reaches the
// report. When no line refers to the file the detail says so rather than
// carrying anything else the scanner printed.
//
// The output is walked in place, one line at a time, and nothing of it is
// copied until a line has passed the prefix test, and then no more of it
// than the per-line bound, so the memory the call takes is bounded by the
// detail it returns and not by what the scanner printed: the runner keeps
// up to its MaxOutput of each stream, splitting that into lines first
// would cost a string header per newline, and a single line as long as
// the whole stream would otherwise be copied in full before it was cut.
// A line is sanitised before it is counted against the byte bound, so the
// bound holds on the sanitised text and a line that would cross it is
// left out and counted as not shown.
func clamDetail(res *exec.Result, path string) string {
	prefix := []byte(path + ":")
	var lines []string
	total, more := 0, 0
	for _, raw := range [][]byte{res.Stdout, res.Stderr} {
		eachLine(raw, func(line []byte) bool {
			if !bytes.HasPrefix(line, prefix) {
				return true
			}
			if len(lines) >= clamDetailLines {
				more++
				return true
			}
			// One byte past the bound is enough for clamText to see that
			// the line was longer and mark the cut.
			s := clamText(string(line[:min(len(line), clamLineBytes+1)]), clamLineBytes)
			if total+len(s) > clamDetailBytes {
				more++
				return true
			}
			total += len(s)
			lines = append(lines, s)
			return true
		})
	}
	if len(lines) == 0 {
		lines = append(lines, "no line of the clamscan output refers to this file")
	}
	if more > 0 {
		lines = append(lines, fmt.Sprintf("%d more line(s) about this file not shown", more))
	}
	if res.OutputTruncated {
		lines = append(lines, "clamscan output was longer than the runner keeps and was cut")
	}
	return strings.Join(lines, "\n")
}

// eachLine calls fn with every newline-separated line of b, without the
// trailing newline and without copying b, until fn returns false.
func eachLine(b []byte, fn func(line []byte) bool) {
	for len(b) > 0 {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			fn(b)
			return
		}
		if !fn(b[:i]) {
			return
		}
		b = b[i+1:]
	}
}

// clamText bounds s to max bytes, cut on a rune boundary, and passes it
// through the report sanitiser so a control character, an ANSI escape
// sequence or a bidirectional override in the scanner's output cannot
// reshape a report line.
func clamText(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		cut := max
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "..."
	}
	return report.Sanitize(s)
}

// firstLine returns the first non-blank line of b, or "" when there is
// none. Like clamDetail it walks b in place so a flood of blank lines
// costs nothing to skip.
func firstLine(b []byte) string {
	return firstLineWithPrefix(b, "")
}

// firstLineWithPrefix returns the first non-blank line of b that starts
// with prefix, trimmed, or "" when there is none.
func firstLineWithPrefix(b []byte, prefix string) string {
	var found string
	p := []byte(prefix)
	eachLine(b, func(line []byte) bool {
		if !bytes.HasPrefix(line, p) {
			return true
		}
		if t := bytes.TrimSpace(line); len(t) > 0 {
			found = string(t)
			return false
		}
		return true
	})
	return found
}

var polySigs = [][]byte{
	[]byte("PK\x03\x04"), []byte("PK\x05\x06"), []byte("Rar!\x1a\x07"), []byte("7z\xbc\xaf\x27\x1c"), []byte("\x7fELF"),
	[]byte("This program cannot be run in DOS mode"),
}

// polyglot looks for archive or executable signatures in the last 1 MiB and
// for the PE stub string in the first 1 MiB. Matroska and MP4 payloads are
// compressed video, so these strings essentially never occur by accident.
// The file is opened with fsutil.OpenRegular, so a named pipe swapped onto
// the path since the scan's own stat cannot block the read.
func (s *Scanner) polyglot(path string, size int64) string {
	f, err := fsutil.OpenRegular(path)
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

// quarantineAbs returns the cleaned absolute path of the quarantine
// directory, or "" when no quarantine is set.
func quarantineAbs(quarantine string) string {
	if quarantine == "" {
		return ""
	}
	abs, err := fsutil.Abs(quarantine)
	if err != nil {
		return filepath.Clean(quarantine)
	}
	return abs
}

// QuarantineExcludes returns the directories a walk must not enter for the
// given quarantine directory: its cleaned absolute path, which Walk matches
// by identity when the directory exists, so a quarantine named through a
// symlink, a ".." component, a relative path or a different letter case on
// a case-insensitive filesystem is still recognised. It is nil when
// quarantine is empty.
func QuarantineExcludes(quarantine string) []string {
	q := quarantineAbs(quarantine)
	if q == "" {
		return nil
	}
	return []string{q}
}

// escapes reports whether a relative path leaves the directory it is
// relative to: it is "..", starts with "../", or is absolute.
func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel)
}

// CheckQuarantineRoot returns an error when root is the quarantine
// directory or lies inside it, in which case quarantined files would be
// scanned again and moved one level deeper on every run. The two paths are
// first compared in their cleaned absolute form, which also covers a
// quarantine directory that does not exist yet. When the directory exists
// it is then compared by identity (os.SameFile) with root and with each of
// root's ancestors, in the spelling given and in the symlink-resolved
// spelling, so a trailing slash, a ".." component, a relative path, a
// symlink or a different letter case on a case-insensitive filesystem
// cannot slip past. It returns nil when quarantine is empty.
func CheckQuarantineRoot(root, quarantine string) error {
	q := quarantineAbs(quarantine)
	if q == "" {
		return nil
	}
	isQ := fmt.Errorf("%s is the quarantine directory; the quarantine directory must lie outside the tree it serves", root)
	inQ := fmt.Errorf("%s lies inside the quarantine directory %s; the quarantine directory must lie outside the tree it serves", root, quarantine)
	r := quarantineAbs(root)
	if rel, err := filepath.Rel(q, r); err == nil {
		if rel == "." {
			return isQ
		}
		if !escapes(rel) {
			return inQ
		}
	}
	qfi, err := os.Stat(q)
	if err != nil || !qfi.IsDir() {
		return nil
	}
	starts := []string{r}
	if real, err := filepath.EvalSymlinks(r); err == nil && real != r {
		starts = append(starts, real)
	}
	for _, start := range starts {
		for p, depth := start, 0; ; p, depth = filepath.Dir(p), depth+1 {
			if fi, err := os.Stat(p); err == nil && os.SameFile(fi, qfi) {
				if depth == 0 {
					return isQ
				}
				return inQ
			}
			if filepath.Dir(p) == p {
				break
			}
		}
	}
	return nil
}

// quarantineDest is the place under the quarantine directory that path,
// found under root, is moved to: its path relative to root, or its base
// name when it does not sit under root.
func (s *Scanner) quarantineDest(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "" || rel == "." || escapes(rel) {
		// A caller that hands the file itself as the root, or a root the
		// file does not sit under, still gets the file placed under the
		// quarantine directory by its base name; the destination is never
		// the quarantine root itself. The test is on the first path
		// component, so a directory whose name merely starts with two dots
		// keeps its mirrored place.
		rel = filepath.Base(path)
	}
	return filepath.Join(s.Quarantine, rel)
}

func (s *Scanner) quarantine(fr *report.FileResult, root string) {
	dest := s.quarantineDest(fr.Path, root)
	if dest == filepath.Clean(s.Quarantine) {
		fr.Addf(CodeQuarantined, report.Warn, "quarantine failed: %s has no usable file name", fr.Path)
		return
	}
	// The destination is claimed while it is being created: a second
	// worker of the same run that maps to the same place at the same time
	// is refused here with the words the move primitive uses for a file
	// that is already there, so the two never race for one name. The claim
	// is given back afterwards in every case, because from then on the
	// disk itself tells a later file that the place is taken, and the move
	// primitive never replaces what sits there (guarantee 1).
	if !s.claims.Claim(dest) {
		fr.Addf(CodeQuarantined, report.Warn, "quarantine failed: %v", fsutil.ErrExists)
		return
	}
	defer s.claims.Release(dest)
	// Create the mirrored directory chain without following symlinks so a
	// planted link inside the quarantine tree cannot redirect the file
	// elsewhere (guarantee 3).
	if err := fsutil.MkdirAllUnder(s.Quarantine, filepath.Dir(dest)); err != nil {
		fr.Addf(CodeQuarantined, report.Warn, "quarantine failed: %v", err)
		return
	}
	// The quarantine directory often sits on another filesystem than the
	// media tree (the bare --quarantine form uses the state directory), so
	// the move copies and verifies across devices; it never replaces a
	// file that already sits at the destination.
	if err := fsutil.MoveNoClobber(fr.Path, dest); err != nil {
		fr.Addf(CodeQuarantined, report.Warn, "quarantine failed: %v", err)
		return
	}
	fr.Addf(CodeQuarantined, report.Block, "moved to %s", dest)
}

// bidiChars names the first character of name that can hide or reorder
// what a file is called: a bidirectional control (unicode.Bidi_Control,
// which includes U+061C) or any other Unicode format character (unicode.Cf:
// zero-width characters, the byte order mark, the soft hyphen, the tag
// characters and the rest of the invisible ones). It returns "" for a clean
// name. The property tests, rather than a list of code points, keep a
// newly noticed invisible character from slipping through.
func bidiChars(name string) string {
	for _, r := range name {
		switch {
		case unicode.Is(unicode.Bidi_Control, r):
			return fmt.Sprintf("bidi control U+%04X", r)
		case unicode.Is(unicode.Cf, r):
			return fmt.Sprintf("zero-width character U+%04X", r)
		}
	}
	return ""
}

// entryKind names what a directory entry is when it is not a regular file,
// for the finding that refuses it.
func entryKind(m os.FileMode) string {
	switch {
	case m&os.ModeNamedPipe != 0:
		return "not a regular file (named pipe)"
	case m&os.ModeSocket != 0:
		return "not a regular file (socket)"
	case m&os.ModeDevice != 0:
		return "not a regular file (device)"
	case m&os.ModeDir != 0:
		return "not a regular file (directory)"
	}
	return "not a regular file"
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
