// Package clean strips metadata and extended attributes in place without
// touching tracks. Matroska goes through mkvpropedit; MP4-family and AVI go
// through an ffmpeg stream copy into a temp file that replaces the original
// only after its streams verify identical.
package clean

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/pkg/xattr"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/mp4"
	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/pool"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/report"
	"github.com/amuxify/amuxify/internal/scan"
	"github.com/amuxify/amuxify/internal/verify"
)

// Finding codes.
const (
	CodeSymlink       = "SYMLINK"
	CodeXattr         = "XATTR"
	CodeMetadata      = "METADATA"
	CodeNothing       = "NOTHING_TO_CLEAN"
	CodeSkipped       = "SKIPPED"
	CodeCleanFail     = "CLEAN_FAIL"
	CodeHashMismatch  = "HASH_MISMATCH"
	CodeHardlinked    = "HARDLINKED"
	CodeSidecarRemove = "SIDECAR_REMOVED"
	CodeDryRun        = "DRY_RUN"
	CodeProvenance    = "PURCHASE_ATOM"
)

// Cleaner performs in-place cleaning.
type Cleaner struct {
	Runner   *exec.Runner
	Prober   *probe.Prober
	Verifier *verify.Verifier
	Profile  *policy.Profile

	DryRun                bool
	RemoveBlockedSidecars bool
	StripAudioTags        bool
	// Command names the command the cleaner runs under, so advice about
	// flags only names flags that command has. Empty means clean, which
	// has --strip-audio-tags; ingest sets "ingest" and has no such flag.
	Command   string
	Hardlinks string
	Timeout   time.Duration
	// Progress, when set, receives each result as soon as the file is done.
	// With Jobs above one it is called from several goroutines, one file at
	// a time each, so it must be safe to call concurrently.
	Progress func(report.FileResult)
	// Jobs is the number of files CleanPath works on at once; zero or one
	// means one after the other in walk order.
	Jobs int
}

func (c *Cleaner) hardlinks() string {
	if c.Hardlinks != "" {
		return c.Hardlinks
	}
	return c.Profile.Safety.Hardlinks
}

func (c *Cleaner) timeout() time.Duration {
	if c.Timeout == 0 {
		return 2 * time.Hour
	}
	return c.Timeout
}

// CleanPath processes a file or a tree.
func (c *Cleaner) CleanPath(ctx context.Context, root string) ([]report.FileResult, error) {
	abs, err := fsutil.Abs(root)
	if err != nil {
		return nil, err
	}
	// Walk lists every readable entry and names the unreadable ones in
	// walkErr; those are reported at run level after the readable files.
	paths, walkErr := scan.Walk(abs)
	// Two names of one inode are cleaned one after the other in walk order,
	// never at the same time, so the second sees what the first did.
	keys := make([][]string, len(paths))
	for i, p := range paths {
		if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
			if k := fsutil.InodeKey(fi); k != "" {
				keys[i] = []string{"inode:" + k}
			}
		}
	}
	results := make([]report.FileResult, len(paths))
	ran := pool.Run(ctx, c.Jobs, len(paths), keys, func(i int) {
		fr := c.CleanFile(ctx, paths[i])
		if c.Progress != nil {
			c.Progress(fr)
		}
		results[i] = fr
	})
	var out []report.FileResult
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

// CleanFile cleans one path.
func (c *Cleaner) CleanFile(ctx context.Context, path string) report.FileResult {
	start := time.Now()
	fr := report.FileResult{Path: path, Info: map[string]string{}}
	defer func() { fr.Duration = time.Since(start) }()

	fi, err := os.Lstat(path)
	if err != nil {
		fr.Addf(CodeCleanFail, report.Fail, "%v", err)
		return fr
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		fr.Addf(CodeSymlink, report.Warn, "symlink skipped")
		return fr
	}
	if !fi.Mode().IsRegular() {
		fr.Addf(CodeCleanFail, report.Fail, "not a regular file; skipped")
		return fr
	}
	ext := fsutil.Ext(path)
	if !scan.IsMedia(path) {
		c.cleanSidecar(&fr, path, ext)
		return fr
	}
	if n := fsutil.Nlink(fi); n > 1 && c.hardlinks() == "skip" {
		fr.Addf(CodeHardlinked, report.Warn, "file has %d hard links; skipped (safety.hardlinks = skip)", n)
		return fr
	}

	info, err := c.Prober.Probe(ctx, path)
	if err != nil {
		fr.Addf(CodeCleanFail, report.Fail, "probe: %v", err)
		return fr
	}
	c.cleanMedia(ctx, &fr, path, ext, info, fi)
	return fr
}

// CleanScanned cleans one file that scan has already probed, without probing
// again. The result carries only the cleaner's findings; ingest merges them.
func (c *Cleaner) CleanScanned(ctx context.Context, sc scan.Result) report.FileResult {
	start := time.Now()
	path := sc.File.Path
	fr := report.FileResult{Path: path, Info: map[string]string{}}
	defer func() { fr.Duration = time.Since(start) }()

	fi, err := os.Lstat(path)
	if err != nil {
		fr.Addf(CodeCleanFail, report.Fail, "%v", err)
		return fr
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		fr.Addf(CodeSymlink, report.Warn, "symlink skipped")
		return fr
	}
	if !fi.Mode().IsRegular() {
		fr.Addf(CodeCleanFail, report.Fail, "not a regular file; skipped")
		return fr
	}
	// The scan's verdict is what lets this file be edited without a probe
	// of its own, and it describes the file the scanner examined. The entry
	// at the path must still be that file, with the size and the
	// modification time the scanner saw, or the verdict says nothing about
	// what would be edited (guarantee 9). A result without a Stat was never
	// examined by the scanner and is refused for the same reason.
	if !fsutil.Unchanged(sc.Stat, fi) {
		fr.Addf(CodeCleanFail, report.Fail, "%s changed since the scan; the file that was scanned is not the file at this path", path)
		return fr
	}
	ext := fsutil.Ext(path)
	if !scan.IsMedia(path) {
		c.cleanSidecar(&fr, path, ext)
		return fr
	}
	if n := fsutil.Nlink(fi); n > 1 && c.hardlinks() == "skip" {
		fr.Addf(CodeHardlinked, report.Warn, "file has %d hard links; skipped (safety.hardlinks = skip)", n)
		return fr
	}
	if sc.Info == nil {
		fr.Addf(CodeCleanFail, report.Fail, "no probe result; scan did not parse this file")
		return fr
	}
	c.cleanMedia(ctx, &fr, path, ext, sc.Info, fi)
	return fr
}

// cleanSidecar removes a blocked sidecar when asked to, or strips its
// extended attributes.
func (c *Cleaner) cleanSidecar(fr *report.FileResult, path, ext string) {
	if contains(c.Profile.Sidecars.Block, ext) {
		if c.RemoveBlockedSidecars {
			if c.DryRun {
				fr.Addf(CodeDryRun, report.Pass, "would remove blocked sidecar .%s", ext)
			} else if err := os.Remove(path); err != nil {
				fr.Addf(CodeCleanFail, report.Fail, "remove: %v", err)
			} else {
				fr.Addf(CodeSidecarRemove, report.Pass, "blocked sidecar removed")
			}
			return
		}
		fr.Addf(CodeSkipped, report.Warn, "blocked sidecar .%s left in place (use --remove-blocked-sidecars)", ext)
		return
	}
	c.stripXattrs(fr, path)
	if len(fr.Findings) == 0 {
		fr.Addf(CodeNothing, report.Pass, "sidecar; nothing to clean")
	}
}

// cleanMedia picks the cleaner for a probed media file and strips extended
// attributes afterwards. fi is the os.Lstat the caller took of the file
// before it was probed or, under ingest, checked against the scan; it pins
// the file every later step must still find at the path.
func (c *Cleaner) cleanMedia(ctx context.Context, fr *report.FileResult, path, ext string, info *probe.MediaInfo, fi os.FileInfo) {
	switch {
	case info.IsMatroska():
		c.cleanMatroska(ctx, fr, info, fi)
	case info.Container == "mp4":
		c.cleanRewrite(ctx, fr, info, mp4Muxer(ext), fi)
	case info.Container == "avi":
		c.cleanRewrite(ctx, fr, info, "avi", fi)
	case info.Container == "flv":
		c.cleanRewrite(ctx, fr, info, "flv", fi)
	case info.Container == "mpegts", info.Container == "mpegps":
		fr.Addf(CodeNothing, report.Pass, "%s carries no writable metadata; use remux to convert", info.Container)
	case info.Container == "mp3", info.Container == "flac", info.Container == "ogg", info.Container == "wav":
		if c.StripAudioTags {
			c.cleanRewrite(ctx, fr, info, audioMuxer(info.Container), fi)
		} else {
			fr.Addf(CodeSkipped, report.Pass, "%s", c.audioTagsAdvice())
		}
	default:
		fr.Addf(CodeSkipped, report.Warn, "no cleaner for container %s", info.Container)
	}
	c.stripXattrs(fr, path)
}

func mp4Muxer(ext string) string {
	switch ext {
	case "mov":
		return "mov"
	case "m4a":
		return "ipod"
	}
	return "mp4"
}

func audioMuxer(c string) string {
	switch c {
	case "mp3":
		return "mp3"
	case "flac":
		return "flac"
	case "ogg":
		return "ogg"
	}
	return "wav"
}

func (c *Cleaner) cleanMatroska(ctx context.Context, fr *report.FileResult, info *probe.MediaInfo, fi os.FileInfo) {
	p := c.Profile
	args := []string{info.Path}
	var what []string
	if p.Metadata.StripTags && (info.GlobalTagN > 0 || info.TrackTagN > 0) {
		args = append(args, "--tags", "all:")
		what = append(what, fmt.Sprintf("%d tag entries", info.GlobalTagN+info.TrackTagN))
	}
	infoEdits := []string{}
	if p.Metadata.StripTitle && info.Title != "" {
		infoEdits = append(infoEdits, "--delete", "title")
		what = append(what, "title")
	}
	if p.Metadata.StripProvenance && (info.MuxingApp != "" || info.WritingApp != "") {
		infoEdits = append(infoEdits, "--set", "muxing-application=", "--set", "writing-application=", "--delete", "date")
		what = append(what, "muxing/writing app, date")
	}
	if len(infoEdits) > 0 {
		args = append(append(args, "--edit", "info"), infoEdits...)
	}
	if p.Metadata.StripTrackTitles {
		for _, s := range info.Streams {
			if s.Title != "" && s.MkvID >= 0 {
				args = append(args, "--edit", fmt.Sprintf("track:@%d", s.MkvID+1), "--delete", "name")
				what = append(what, fmt.Sprintf("track #%d name", s.Index))
			}
		}
	}
	if len(what) == 0 {
		fr.Addf(CodeNothing, report.Pass, "already clean")
		return
	}
	if c.DryRun {
		fr.Addf(CodeDryRun, report.Pass, "would remove %s", strings.Join(what, ", "))
		return
	}
	// mkvpropedit opens the name with an ordinary open and edits whatever
	// it reaches, and the name was last examined before a probe that takes
	// seconds, or under ingest before a whole scan. The entry at the path
	// must still be the file that was probed: a symbolic link swapped onto
	// it would otherwise have its target's headers edited (guarantee 3),
	// and a different file renamed onto it would be edited on the strength
	// of another file's probe.
	if err := sourceUnchanged(info.Path, fi); err != nil {
		fr.Addf(CodeCleanFail, report.Fail, "%v; nothing was edited", err)
		return
	}
	res, err := c.Runner.RunWithTimeout(ctx, c.timeout(), exec.MKVPropedit, args...)
	if err != nil {
		fr.Addf(CodeCleanFail, report.Fail, "mkvpropedit: %v", err)
		return
	}
	if res.ExitCode >= 2 {
		fr.Add(report.Finding{Code: CodeCleanFail, Severity: report.Fail, Message: "mkvpropedit failed", Detail: strings.TrimSpace(string(res.Stdout) + string(res.Stderr))})
		return
	}
	fr.Addf(CodeMetadata, report.Pass, "removed %s", strings.Join(what, ", "))
}

// cleanRewrite stream-copies into a temp file with all container metadata
// dropped, verifies stream hashes, then replaces the original.
// inertFormatTags counts the format tags that ffprobe reports for every
// MP4-family file and that a rewrite can never remove: the ftyp brand
// fields. Counting them would make the rewrite run again on a file it has
// just cleaned. The encoder tag is not on this list: ffmpeg does not write
// it back under -bitexact, so its presence means the file was not cleaned.
func inertFormatTags(tags map[string]string) int {
	n := 0
	for k := range tags {
		switch strings.ToLower(k) {
		case "major_brand", "minor_version", "compatible_brands":
			n++
		}
	}
	return n
}

// inertStreamTag reports whether a stream tag is one the MP4 muxer writes
// on its own with a fixed value (the default handler names and ffmpeg's own
// vendor id), so it survives every rewrite. Any other value, including a
// handler name that carries text of someone's choosing, still counts as
// metadata to strip.
func inertStreamTag(key, value string) bool {
	switch strings.ToLower(key) {
	case "handler_name":
		switch value {
		case "VideoHandler", "SoundHandler", "SubtitleHandler", "DataHandler":
			return true
		}
	case "vendor_id":
		switch value {
		case "FFMP", "[0][0][0][0]":
			return true
		}
	}
	return false
}

func (c *Cleaner) cleanRewrite(ctx context.Context, fr *report.FileResult, info *probe.MediaInfo, muxer string, fi os.FileInfo) {
	var what []string
	if info.Container == "mp4" {
		if in, err := mp4.Parse(info.Path); err == nil {
			if keys := in.ProvenanceKeys(); len(keys) > 0 {
				what = append(what, fmt.Sprintf("%d provenance atom(s): %s", len(keys), strings.Join(keys, ", ")))
			}
			if in.HasXMP {
				what = append(what, "XMP box")
			}
			// Count remaining ilst entries and udta children as generic metadata.
			if n := len(in.Ilst); n > 0 {
				what = append(what, fmt.Sprintf("%d ilst item(s)", n))
			}
		}
	}
	if n := len(info.Tags) - inertFormatTags(info.Tags); n > 0 {
		what = append(what, fmt.Sprintf("%d format tag(s)", n))
	}
	for _, s := range info.Streams {
		for k, v := range s.Tags {
			if strings.EqualFold(k, "language") || inertStreamTag(k, v) {
				continue
			}
			what = append(what, fmt.Sprintf("stream #%d tag %s", s.Index, k))
		}
	}
	if len(what) == 0 {
		fr.Addf(CodeNothing, report.Pass, "already clean")
		return
	}
	if c.DryRun {
		fr.Addf(CodeDryRun, report.Pass, "would rewrite without %s", strings.Join(what, ", "))
		return
	}
	// The temp file is created here, empty and exclusively, before its name
	// is handed to ffmpeg, and the identity recorded is what every later
	// step requires of the entry at the name: as soon as ffmpeg returns, and
	// before ffprobe, the stream hashes or the atom parser open that name,
	// tempUnchanged checks that it still leads to this run's own regular
	// file, so a symlink swapped onto it is never read through and a named
	// pipe swapped onto it is never opened, which would block the run until
	// the planter chose to write. The replacement below then writes the
	// source's mode, ownership and time through a descriptor of that same
	// file, never through the name (guarantee 3). ffmpeg writes into the
	// existing file rather than unlinking and recreating it, so the identity
	// survives the rewrite. os.Remove never follows a link, so the cleanup
	// on every failure removes a planted link or pipe and leaves any target
	// alone. The handle stays open until the replacement has been checked,
	// so the inode number cannot be freed and reused by a file swapped onto
	// the name, and the flush before the replacement goes through it.
	//
	// The source is checked against fi, the identity pinned before the
	// probe, right before ffmpeg opens its name, because ffmpeg follows
	// whatever the name leads to, and again after the last read of it and
	// before the replacement, because a different file renamed onto the
	// path after the hashes were taken is not the file that was verified
	// and must not be replaced and lost.
	if err := sourceUnchanged(info.Path, fi); err != nil {
		fr.Addf(CodeCleanFail, report.Fail, "%v; original untouched", err)
		return
	}
	tmp := fsutil.TempName(info.Path)
	own, err := fsutil.CreateTemp(tmp)
	if err != nil {
		fr.Addf(CodeCleanFail, report.Fail, "temp file: %v", err)
		return
	}
	defer own.Close()
	created := own.Info()
	args := []string{"-v", "error", "-i", info.Path, "-map", "0", "-c", "copy", "-map_metadata", "-1", "-fflags", "+bitexact", "-flags", "+bitexact"}
	if c.Profile.Chapters.Keep {
		args = append(args, "-map_chapters", "0")
	} else {
		args = append(args, "-map_chapters", "-1")
	}
	for _, s := range info.Streams {
		if (s.Type == "audio" || s.Type == "subtitle") && !policy.IsUnd(s.Language) {
			args = append(args, fmt.Sprintf("-metadata:s:%d", s.Index), "language="+s.Language)
		}
		if s.Type == "video" && s.AttachedPic {
			args = append(args, fmt.Sprintf("-disposition:%d", s.Index), "attached_pic")
		}
	}
	if muxer == "mp4" || muxer == "mov" || muxer == "ipod" {
		args = append(args, "-movflags", "+faststart")
	}
	args = append(args, "-f", muxer, "-y", tmp)
	res, err := c.Runner.RunWithTimeout(ctx, c.timeout(), exec.FFmpeg, args...)
	if err != nil || res.ExitCode != 0 {
		_ = os.Remove(tmp)
		msg := ""
		if err != nil {
			msg = err.Error()
		} else {
			msg = strings.TrimSpace(string(res.Stderr))
		}
		fr.Addf(CodeCleanFail, report.Fail, "ffmpeg rewrite: %s", msg)
		return
	}
	if err := tempUnchanged(tmp, created); err != nil {
		_ = os.Remove(tmp)
		fr.Addf(CodeCleanFail, report.Fail, "replace: %v; original untouched", err)
		return
	}
	out, err := c.Prober.Probe(ctx, tmp)
	if err != nil || len(out.Streams) != len(info.Streams) {
		_ = os.Remove(tmp)
		fr.Addf(CodeCleanFail, report.Fail, "rewritten file does not match source stream layout")
		return
	}
	for i, s := range info.Streams {
		if s.Type != "video" && s.Type != "audio" && s.Type != "subtitle" {
			continue
		}
		h1, e1 := c.Verifier.StreamHash(ctx, info.Path, s.Index)
		h2, e2 := c.Verifier.StreamHash(ctx, tmp, out.Streams[i].Index)
		if e1 != nil || e2 != nil || h1 == "" || h1 != h2 {
			_ = os.Remove(tmp)
			fr.Addf(CodeHashMismatch, report.Fail, "stream #%d differs after rewrite; original untouched", s.Index)
			return
		}
	}
	if info.Container == "mp4" {
		if in, err := mp4.Parse(tmp); err == nil {
			if keys := in.ProvenanceKeys(); len(keys) > 0 || in.HasXMP {
				_ = os.Remove(tmp)
				fr.Addf(CodeProvenance, report.Fail, "provenance survived rewrite: %s", strings.Join(keys, ", "))
				return
			}
		}
	}
	// The replacement's error is assigned to the function-level err on
	// purpose: an if-scoped err here would be discarded and a failed
	// replacement reported as a successful rewrite. The replacement is
	// handed fi as well, so the entry it renames over must be the file the
	// hashes were taken from, with the same size and time, or nothing is
	// renamed.
	err = sourceUnchanged(info.Path, fi)
	if err == nil {
		err = own.Sync()
	}
	if err == nil {
		err = fsutil.ReplaceInPlaceOwn(tmp, info.Path, created, fi)
	}
	if err != nil {
		_ = os.Remove(tmp)
		fr.Addf(CodeCleanFail, report.Fail, "replace: %v; original untouched", err)
		return
	}
	fr.Addf(CodeMetadata, report.Pass, "rewritten without %s", strings.Join(what, ", "))
}

// sourceUnchanged reports an error when path no longer names the regular
// file described by was, the os.Lstat taken before the file was probed or
// checked against the scan: a symlink planted since, a directory, a device,
// nothing at all, another regular file renamed onto the path, or the same
// file written to since, which inode, size and modification time tell
// apart. It runs right before a tool opens the source by name and right
// before the source is replaced, because the Lstat that refused a symlink
// at the start does not cover the time a probe, a scan or a rewrite takes.
func sourceUnchanged(path string, was os.FileInfo) error {
	now, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("source changed since it was examined: %v", err)
	}
	if now.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is now a symlink; refusing to follow it", path)
	}
	if !now.Mode().IsRegular() {
		return fmt.Errorf("%s is no longer a regular file", path)
	}
	if !fsutil.Unchanged(was, now) {
		return fmt.Errorf("%s was replaced after it was examined", path)
	}
	return nil
}

// tempUnchanged reports an error when tmp no longer names the regular file
// created describes with a single name: a symlink, a named pipe or another
// entry swapped onto the name, or a hard link added to the file. It runs
// as soon as ffmpeg has returned, before any tool or parser opens the name
// again.
func tempUnchanged(tmp string, created os.FileInfo) error {
	now, err := os.Lstat(tmp)
	if err != nil {
		return fmt.Errorf("temp file: %v", err)
	}
	if now.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is now a symlink; refusing to follow it", tmp)
	}
	if !now.Mode().IsRegular() || !os.SameFile(created, now) {
		return fmt.Errorf("%s is not the file this run created", tmp)
	}
	if n := fsutil.Nlink(now); n != 1 {
		return fmt.Errorf("%s has %d hard links; expected 1", tmp, n)
	}
	return nil
}

// audioTagsAdvice is the SKIPPED text for an audio file whose tags are left
// alone. It names --strip-audio-tags only for the clean command, which has
// that flag; ingest and the hook that runs it do not (review C10).
func (c *Cleaner) audioTagsAdvice() string {
	if c.Command == "ingest" {
		return "audio tags left alone; ingest does not strip audio tags"
	}
	return "audio tags left alone (use --strip-audio-tags)"
}

// xattrPrefixes are the only namespaces amuxify removes. Security labels,
// ACLs and system attributes are never touched.
func xattrPrefixes() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"com.apple."}
	case "linux", "freebsd":
		return []string{"user."}
	}
	return nil
}

// inNamespace reports whether the attribute name lies in one of the
// namespaces amuxify may remove: an exact, case-sensitive prefix match with
// a non-empty attribute name after it. "user." alone, "USER.x",
// "trusted.user.x", "com.applex.y" and a name that merely contains a prefix
// somewhere after its start are all outside (guarantee 7).
func inNamespace(name string, prefixes []string) bool {
	for _, p := range prefixes {
		if p != "" && len(name) > len(p) && strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

func (c *Cleaner) stripXattrs(fr *report.FileResult, path string) {
	prefixes := xattrPrefixes()
	if prefixes == nil {
		return
	}
	names, err := xattr.LList(path)
	if err != nil {
		return
	}
	var removed, failed []string
	for _, n := range names {
		if !inNamespace(n, prefixes) {
			continue
		}
		if c.DryRun {
			removed = append(removed, n)
			continue
		}
		if err := xattr.LRemove(path, n); err != nil {
			failed = append(failed, n)
		} else {
			removed = append(removed, n)
		}
	}
	if len(removed) > 0 {
		verb := "removed"
		if c.DryRun {
			verb = "would remove"
		}
		fr.Addf(CodeXattr, report.Pass, "%s xattr %s", verb, strings.Join(removed, ", "))
	}
	if len(failed) > 0 {
		fr.Addf(CodeXattr, report.Warn, "could not remove xattr %s", strings.Join(failed, ", "))
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
