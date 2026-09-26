// Package remux rebuilds a file into a sanitized Matroska container with
// mkvmerge, then proves the result: every kept stream hashes identically to
// the source, the structure matches the decision, and the head and tail
// decode. Only then is the output placed, never over an existing file.
package remux

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/verify"
)

// Finding codes.
const (
	CodeSkipped          = "SKIPPED"
	CodeRefused          = "REFUSED"
	CodeOutputExists     = "OUTPUT_EXISTS"
	CodeUnsupportedInput = "UNSUPPORTED_INPUT"
	CodeRemuxFail        = "REMUX_FAIL"
	CodeRemuxWarning     = "REMUX_WARNING"
	CodeHashMismatch     = "HASH_MISMATCH"
	CodeHashOK           = "HASH_OK"
	CodeVerifyFail       = "VERIFY_FAIL"
	CodeHDRLost          = "HDR_LOST"
	CodeDecodeFail       = "DECODE_FAIL"
	CodeHardlinked       = "HARDLINKED"
	CodeDryRun           = "DRY_RUN"
	CodePlaced           = "PLACED"
	CodeTrack            = "TRACK"
	CodeAttachment       = "ATTACHMENT"
)

// Remuxer performs remux runs.
type Remuxer struct {
	Runner   *exec.Runner
	Prober   *probe.Prober
	Verifier *verify.Verifier
	Scanner  *scan.Scanner
	Profile  *policy.Profile

	OutputRoot string // mirrored output tree; empty means <root>__remuxed
	InPlace    bool
	Hardlinks  string // skip | break | copy; empty means profile value
	VerifyTier string // quick | full | none; empty means profile value
	Force      bool   // proceed on FAIL scan findings (never on BLOCK)
	DryRun     bool
	Original   string // original language hint for default-audio selection
	Timeout    time.Duration
	// Progress, when set, receives each result as soon as the file is done.
	Progress func(report.FileResult)
}

func (r *Remuxer) hardlinks() string {
	if r.Hardlinks != "" {
		return r.Hardlinks
	}
	return r.Profile.Safety.Hardlinks
}

func (r *Remuxer) tier() string {
	if r.VerifyTier != "" {
		return r.VerifyTier
	}
	return r.Profile.Verify.Tier
}

func (r *Remuxer) timeout() time.Duration {
	if r.Timeout == 0 {
		return 6 * time.Hour
	}
	return r.Timeout
}

// Roots returns the tree relative paths are computed against and the output
// tree used by output mode and by the copy hard-link mode.
func (r *Remuxer) Roots(abs string, isDir bool) (inputRoot, outRoot string) {
	inputRoot = abs
	if !isDir {
		inputRoot = filepath.Dir(abs)
	}
	outRoot = r.OutputRoot
	if outRoot == "" {
		outRoot = filepath.Join(filepath.Dir(inputRoot), filepath.Base(inputRoot)+"__remuxed")
	}
	return inputRoot, outRoot
}

// RemuxPath processes a file or a directory tree.
func (r *Remuxer) RemuxPath(ctx context.Context, root string) ([]report.FileResult, error) {
	abs, err := fsutil.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	inputRoot, outRoot := r.Roots(abs, fi.IsDir())
	if !r.InPlace && !r.DryRun {
		if err := os.MkdirAll(outRoot, 0o755); err != nil {
			return nil, err
		}
	}
	if r.tier() == "none" && r.InPlace {
		return nil, fmt.Errorf("refusing --in-place together with verify tier none")
	}
	// A tree with an unreadable corner still yields every readable file;
	// the scan error naming the corner is returned after them so the run is
	// reported as FAIL at run level, the same as scan, clean and ingest do.
	results, scanErr := r.Scanner.ScanPath(ctx, abs)
	if scanErr != nil && len(results) == 0 {
		return nil, scanErr
	}
	var out []report.FileResult
	for _, sc := range results {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		fr := r.RemuxScanned(ctx, sc, inputRoot, outRoot)
		if r.Progress != nil {
			r.Progress(fr)
		}
		out = append(out, fr)
	}
	return out, scanErr
}

// RemuxScanned remuxes one already scanned file. r.Scanner may be nil.
func (r *Remuxer) RemuxScanned(ctx context.Context, sc scan.Result, inputRoot, outRoot string) report.FileResult {
	start := time.Now()
	fr := sc.File
	defer func() { fr.Duration = time.Since(start) }()

	if sc.Info == nil || !scan.IsMedia(fr.Path) {
		if fr.Verdict < report.Fail {
			fr.Addf(CodeSkipped, report.Pass, "not a media container; scanned only")
		}
		return fr
	}
	if fr.Verdict >= report.Block {
		fr.Addf(CodeRefused, report.Block, "scan blocked this file; not remuxed")
		return fr
	}
	if fr.Verdict >= report.Fail && !r.Force {
		fr.Addf(CodeRefused, report.Fail, "scan failed this file; use --force to remux anyway")
		return fr
	}
	if sc.Info.Container != "matroska" && sc.Info.Container != "webm" && !sc.Info.MkvSupported {
		fr.Addf(CodeUnsupportedInput, report.Fail, "mkvmerge cannot read this %s file", sc.Info.Container)
		return fr
	}
	if len(sc.Info.StreamsOf("video")) == 0 {
		fr.Addf(CodeSkipped, report.Warn, "no video stream; remux handles video containers only")
		return fr
	}

	// Destination.
	rel, err := filepath.Rel(inputRoot, fr.Path)
	if err != nil {
		rel = filepath.Base(fr.Path)
	}
	rel = strings.TrimSuffix(rel, filepath.Ext(rel)) + ".mkv"
	dest := filepath.Join(outRoot, rel)
	inPlace := r.InPlace
	fi, _ := os.Lstat(fr.Path)
	if inPlace && fi != nil && fsutil.Nlink(fi) > 1 {
		switch r.hardlinks() {
		case "skip":
			fr.Addf(CodeHardlinked, report.Warn, "file has %d hard links; skipped (safety.hardlinks = skip)", fsutil.Nlink(fi))
			return fr
		case "copy":
			inPlace = false
			fr.Addf(CodeHardlinked, report.Pass, "hard-linked; output written to %s instead of in place", outRoot)
		default:
			fr.Addf(CodeHardlinked, report.Warn, "hard-linked; replacing in place breaks the link")
		}
	}
	// sameEntry is set when dest and fr.Path are two spellings of one
	// directory entry, which is what X.MKV against X.mkv is on a
	// case-insensitive filesystem. That is not a collision: the verified
	// output replaces the source under its own name and takes the .mkv
	// spelling last. A hard-linked twin that really is spelled X.mkv is a
	// distinct entry and stays a collision.
	sameEntry := false
	if inPlace {
		dest = filepath.Join(filepath.Dir(fr.Path), strings.TrimSuffix(filepath.Base(fr.Path), filepath.Ext(fr.Path))+".mkv")
		if dest != fr.Path {
			if dfi, err := os.Lstat(dest); err == nil {
				if fi != nil && os.SameFile(fi, dfi) && !hasEntry(filepath.Dir(dest), filepath.Base(dest)) {
					sameEntry = true
				} else {
					fr.Addf(CodeOutputExists, report.Fail, "%s already exists", dest)
					return fr
				}
			}
		}
	} else if _, err := os.Lstat(dest); err == nil {
		fr.Addf(CodeOutputExists, report.Fail, "%s already exists", dest)
		return fr
	}

	d := r.Profile.Decide(sc.Info, policy.Options{OriginalLanguage: r.Original})
	for _, f := range d.Findings {
		fr.Add(f)
	}
	if fr.Verdict >= report.Fail && !r.Force {
		return fr
	}
	r.describe(&fr, d)
	if r.DryRun {
		fr.Addf(CodeDryRun, report.Pass, "would write %s", dest)
		fr.Output = dest
		return fr
	}

	// The scan refused symlinks, but a remux takes minutes and mkvmerge
	// opens whatever the path names when it starts, so the source is
	// checked again right before it is handed over (guarantee 3).
	if err := sourceIsRegular(fr.Path); err != nil {
		fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
		return fr
	}

	// In place, the destination sits beside the source in a directory that
	// exists and was walked without following symlinks. Otherwise the
	// mirrored directory chain is created component by component so a
	// symlink planted inside the output tree cannot redirect the remuxed
	// file outside the root the user named (guarantee 4). The refusal is
	// reported as REMUX_FAIL because the finding codes are frozen and, from
	// the caller's point of view, the remux of this file did not happen; the
	// message carries the reason.
	if !inPlace {
		if err := fsutil.MkdirAllUnder(outRoot, filepath.Dir(dest)); err != nil {
			fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
			return fr
		}
	}
	tmp := fsutil.TempName(dest)
	_ = os.Remove(tmp)
	cleanup := func() { _ = os.Remove(tmp) }

	args := r.mkvmergeArgs(tmp, sc.Info, d)
	res, err := r.Runner.RunWithTimeout(ctx, r.timeout(), exec.MKVMerge, args...)
	if err != nil {
		cleanup()
		fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
		return fr
	}
	if res.ExitCode >= 2 {
		cleanup()
		fr.Add(report.Finding{Code: CodeRemuxFail, Severity: report.Fail, Message: "mkvmerge failed", Detail: strings.TrimSpace(string(res.Stdout) + string(res.Stderr))})
		return fr
	}
	if res.ExitCode == 1 {
		fr.Add(report.Finding{Code: CodeRemuxWarning, Severity: report.Warn, Message: "mkvmerge warnings: " + warningsOf(res.Stdout), Detail: strings.TrimSpace(string(res.Stdout))})
	}
	if d.StripProvenance || d.StripTitle {
		pargs := []string{tmp, "--edit", "info"}
		if d.StripProvenance {
			pargs = append(pargs, "--set", "muxing-application=", "--set", "writing-application=", "--delete", "date")
		}
		if d.StripTitle {
			pargs = append(pargs, "--delete", "title")
		}
		pres, err := r.Runner.RunWithTimeout(ctx, 10*time.Minute, exec.MKVPropedit, pargs...)
		if err != nil || pres.ExitCode >= 2 {
			cleanup()
			msg := ""
			if err != nil {
				msg = err.Error()
			} else {
				msg = strings.TrimSpace(string(pres.Stdout) + string(pres.Stderr))
			}
			fr.Addf(CodeRemuxFail, report.Fail, "mkvpropedit: %s", msg)
			return fr
		}
	}

	if !r.verifyOutput(ctx, &fr, sc.Info, d, tmp) {
		cleanup()
		return fr
	}
	if err := fsutil.Fsync(tmp); err != nil {
		cleanup()
		fr.Addf(CodeRemuxFail, report.Fail, "fsync: %v", err)
		return fr
	}
	if !inPlace {
		// Only a file that appeared at dest is OUTPUT_EXISTS; any other
		// placement failure, such as a directory that became unwritable,
		// is a failed remux and says so.
		if err := fsutil.PlaceNoClobber(tmp, dest); err != nil {
			cleanup()
			if errors.Is(err, fsutil.ErrExists) {
				fr.Addf(CodeOutputExists, report.Fail, "%v", err)
			} else {
				fr.Addf(CodeRemuxFail, report.Fail, "place: %v", err)
			}
			return fr
		}
		fr.Output = dest
		fr.Addf(CodePlaced, report.Pass, "written and verified")
		return fr
	}
	// In place. The source is checked once more before it is replaced or
	// removed, so a symlink planted during the run is never followed.
	if err := sourceIsRegular(fr.Path); err != nil {
		cleanup()
		fr.Addf(CodeRemuxFail, report.Fail, "%v", err)
		return fr
	}
	switch {
	case dest == fr.Path || sameEntry:
		// The verified output replaces the source under the source's own
		// name. When only the extension case differs the entry is renamed
		// to the .mkv spelling last; it is the same entry, so that rename
		// cannot replace any other file.
		if err := fsutil.ReplaceInPlace(tmp, fr.Path); err != nil {
			cleanup()
			fr.Addf(CodeRemuxFail, report.Fail, "replace: %v", err)
			return fr
		}
		if sameEntry {
			if err := os.Rename(fr.Path, dest); err != nil {
				fr.Output = fr.Path
				fr.Addf(CodeRemuxFail, report.Fail, "rename to %s: %v; the rebuilt file was left under its original name", dest, err)
				return fr
			}
			if !hasEntry(filepath.Dir(dest), filepath.Base(dest)) {
				// The filesystem kept the original spelling; report the
				// name that is really there.
				dest = fr.Path
			}
		}
	default:
		// The source keeps a different name (a.mp4 becomes a.mkv). The
		// output takes the source's identity and is placed under the new
		// name with the no-clobber primitive, and only then is the source
		// removed: a file that appeared at dest during the run is never
		// replaced (guarantee 1), and a crash leaves the source or the
		// placed output, never Matroska content under the old name.
		if err := fsutil.CopyIdentity(fr.Path, tmp); err != nil {
			cleanup()
			fr.Addf(CodeRemuxFail, report.Fail, "identity: %v", err)
			return fr
		}
		if err := fsutil.PlaceNoClobber(tmp, dest); err != nil {
			cleanup()
			if errors.Is(err, fsutil.ErrExists) {
				fr.Addf(CodeOutputExists, report.Fail, "%s appeared while the file was being rebuilt; the source was left untouched and the unplaced output was discarded", dest)
			} else {
				fr.Addf(CodeRemuxFail, report.Fail, "place: %v", err)
			}
			return fr
		}
		if err := os.Remove(fr.Path); err != nil {
			fr.Output = dest
			fr.Addf(CodeRemuxFail, report.Fail, "the rebuilt file was placed at %s but the source could not be removed: %v", dest, err)
			return fr
		}
	}
	fr.Output = dest
	fr.Addf(CodePlaced, report.Pass, "written and verified")
	return fr
}

// sourceIsRegular reports an error when path no longer names a regular
// file: a symlink planted after the scan, a directory, a device or nothing
// at all. The scan's own symlink check does not cover the time a remux
// takes, so this runs right before the source is opened by a tool and
// right before it is replaced.
func sourceIsRegular(path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("source changed since the scan: %v", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is now a symlink; refusing to follow it (changed since the scan)", path)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is no longer a regular file (changed since the scan)", path)
	}
	return nil
}

// hasEntry reports whether dir holds an entry spelled exactly name. On a
// case-insensitive filesystem Lstat resolves X.mkv to the entry X.MKV, and
// only the directory listing tells two spellings of one entry from two
// hard-linked files with different names. When the listing cannot be read
// the answer is true, which callers treat as a collision.
func hasEntry(dir, name string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true
	}
	for _, e := range entries {
		if e.Name() == name {
			return true
		}
	}
	return false
}

func warningsOf(out []byte) string {
	var w []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "Warning:") {
			w = append(w, strings.TrimSpace(strings.TrimPrefix(line, "Warning:")))
		}
	}
	if len(w) == 0 {
		return "(see detail)"
	}
	return strings.Join(w, "; ")
}

func (r *Remuxer) describe(fr *report.FileResult, d *policy.Decision) {
	for _, t := range d.Tracks {
		s := t.Stream
		if s.Type == "attachment" || s.Type == "data" {
			continue
		}
		verb := "drop"
		if t.Keep {
			verb = "keep"
		}
		flags := ""
		if t.Keep && t.Default && s.Type != "video" {
			flags += " default"
		}
		if t.Keep && t.Forced {
			flags += " forced"
		}
		if t.SetLanguage != "" {
			flags += " lang=" + t.SetLanguage
		}
		fr.Actions = append(fr.Actions, fmt.Sprintf("%s #%d %s %s %s%s: %s", verb, s.Index, s.Type, s.Codec, s.Language, flags, t.Reason))
	}
	for _, a := range d.Attachments {
		verb := "drop"
		if a.Keep {
			verb = "keep"
		}
		fr.Actions = append(fr.Actions, fmt.Sprintf("%s attachment #%d %s: %s", verb, a.Attachment.ID, a.Attachment.FileName, a.Reason))
	}
	if d.KeepChapters {
		fr.Actions = append(fr.Actions, "keep chapters")
	} else {
		fr.Actions = append(fr.Actions, "drop chapters")
	}
}

// mkvmergeArgs builds the command line from the decision.
func (r *Remuxer) mkvmergeArgs(out string, m *probe.MediaInfo, d *policy.Decision) []string {
	args := []string{"-o", out, "--disable-track-statistics-tags", "--no-date"}
	if d.StripTitle {
		args = append(args, "--title", "")
	}
	if d.StripTags {
		args = append(args, "--no-global-tags", "--no-track-tags")
	}
	if !d.KeepChapters {
		args = append(args, "--no-chapters")
	}
	var keepAtt []string
	for _, a := range d.Attachments {
		if a.Keep {
			keepAtt = append(keepAtt, fmt.Sprint(a.Attachment.ID))
		}
	}
	if len(keepAtt) == 0 {
		args = append(args, "--no-attachments")
	} else {
		args = append(args, "--attachments", strings.Join(keepAtt, ","))
	}
	ids := map[string][]string{}
	var trackOpts []string
	for _, t := range d.Tracks {
		if !t.Keep {
			continue
		}
		id := t.Stream.MkvID
		if id < 0 {
			id = t.Stream.Index
		}
		sid := fmt.Sprint(id)
		ids[t.Stream.Type] = append(ids[t.Stream.Type], sid)
		if t.Stream.Type == "audio" || t.Stream.Type == "subtitle" {
			trackOpts = append(trackOpts, "--default-track-flag", sid+":"+boolFlag(t.Default))
			trackOpts = append(trackOpts, "--forced-display-flag", sid+":"+boolFlag(t.Forced))
		}
		if t.SetLanguage != "" {
			trackOpts = append(trackOpts, "--language", sid+":"+t.SetLanguage)
		}
		if t.ClearTitle {
			trackOpts = append(trackOpts, "--track-name", sid+":")
		}
	}
	args = append(args, "--video-tracks", strings.Join(ids["video"], ","))
	if len(ids["audio"]) == 0 {
		args = append(args, "--no-audio")
	} else {
		args = append(args, "--audio-tracks", strings.Join(ids["audio"], ","))
	}
	if len(ids["subtitle"]) == 0 {
		args = append(args, "--no-subtitles")
	} else {
		args = append(args, "--subtitle-tracks", strings.Join(ids["subtitle"], ","))
	}
	args = append(args, "--no-buttons")
	args = append(args, trackOpts...)
	args = append(args, "(", m.Path, ")")
	return args
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// verifyOutput runs the verification battery. Returns false on failure.
func (r *Remuxer) verifyOutput(ctx context.Context, fr *report.FileResult, src *probe.MediaInfo, d *policy.Decision, tmp string) bool {
	out, err := r.Prober.Probe(ctx, tmp)
	if err != nil {
		fr.Addf(CodeVerifyFail, report.Fail, "output unreadable: %v", err)
		return false
	}
	if !out.MkvIdentified {
		fr.Addf(CodeVerifyFail, report.Fail, "mkvmerge cannot identify output: %s", strings.Join(out.MkvErrors, "; "))
		return false
	}
	fail := func(format string, a ...interface{}) bool {
		fr.Addf(CodeVerifyFail, report.Fail, format, a...)
		return false
	}
	// Structure.
	for _, typ := range []string{"video", "audio", "subtitle"} {
		want := len(d.KeptOf(typ))
		got := len(out.StreamsOf(typ))
		if want != got {
			return fail("expected %d %s stream(s), output has %d", want, typ, got)
		}
	}
	if len(out.StreamsOf("attachment")) > 0 || len(out.Attachments) > 0 {
		keep := 0
		for _, a := range d.Attachments {
			if a.Keep {
				keep++
			}
		}
		if len(out.Attachments) != keep {
			return fail("expected %d attachment(s), output has %d", keep, len(out.Attachments))
		}
	}
	if !d.KeepChapters && out.ChapterN > 0 {
		return fail("chapters present in output")
	}
	if d.KeepChapters && src.ChapterN > 0 && out.ChapterN == 0 {
		return fail("chapters lost in output")
	}
	if d.StripTags && (out.GlobalTagN > 0 || out.TrackTagN > 0) {
		return fail("tags present in output (%d global, %d track)", out.GlobalTagN, out.TrackTagN)
	}
	if d.StripTitle && out.Title != "" {
		return fail("title present in output")
	}
	if d.StripProvenance && (out.MuxingApp != "" || out.WritingApp != "") {
		return fail("muxing/writing application still set")
	}
	// HDR and Dolby Vision survive only if mkvmerge carried the side data.
	sv := src.StreamsOf("video")
	ov := out.StreamsOf("video")
	if len(sv) > 0 && len(ov) > 0 && len(sv[0].HDR) > 0 {
		if strings.Join(sv[0].HDR, "+") != strings.Join(ov[0].HDR, "+") {
			fr.Addf(CodeHDRLost, report.Fail, "source video is %s, output is %s", strings.Join(sv[0].HDR, "+"), orNone(strings.Join(ov[0].HDR, "+")))
			return false
		}
	}
	// Stream hashes, kept streams in order versus output streams in order.
	if r.Profile.Verify.StreamHash && r.tier() != "none" {
		outByType := map[string][]probe.Stream{"video": out.StreamsOf("video"), "audio": out.StreamsOf("audio"), "subtitle": out.StreamsOf("subtitle")}
		pos := map[string]int{}
		n := 0
		for _, t := range d.Tracks {
			if !t.Keep || (t.Stream.Type != "video" && t.Stream.Type != "audio" && t.Stream.Type != "subtitle") {
				continue
			}
			o := outByType[t.Stream.Type][pos[t.Stream.Type]]
			pos[t.Stream.Type]++
			ok, how, err := r.sameStream(ctx, src, t.Stream, out, o)
			if err != nil {
				return fail("stream #%d hash: %v", t.Stream.Index, err)
			}
			if !ok {
				fr.Addf(CodeHashMismatch, report.Fail, "stream #%d (%s) differs from source", t.Stream.Index, t.Stream.Type)
				return false
			}
			_ = how
			n++
		}
		fr.Addf(CodeHashOK, report.Pass, "%d stream(s) verified identical to source", n)
	}
	// The output carries the source's video and audio streams, so the
	// source probe decides whether there is anything to decode.
	if tier := r.tier(); tier != "none" {
		if err := r.Verifier.Decode(ctx, tmp, src, tier == "full"); err != nil {
			fr.Addf(CodeDecodeFail, report.Fail, "%v", err)
			return false
		}
	}
	return true
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// sameStream compares packet hashes first and falls back to decoded hashes
// when the source container frames packets differently from Matroska.
func (r *Remuxer) sameStream(ctx context.Context, src *probe.MediaInfo, s probe.Stream, out *probe.MediaInfo, o probe.Stream) (bool, string, error) {
	h1, err1 := streamHash(ctx, r.Verifier, src.Path, s.Index)
	h2, err2 := streamHash(ctx, r.Verifier, out.Path, o.Index)
	if err1 == nil && err2 == nil && h1 != "" && h1 == h2 {
		return true, "packet", nil
	}
	if src.IsMatroska() && err1 == nil && err2 == nil {
		return false, "packet", nil
	}
	// Different framing is expected across containers; compare samples.
	d1, err := decodedHash(ctx, r.Verifier, src.Path, s)
	if err != nil {
		return false, "", err
	}
	d2, err := decodedHash(ctx, r.Verifier, out.Path, o)
	if err != nil {
		return false, "", err
	}
	return d1 != "" && d1 == d2, "decoded", nil
}
