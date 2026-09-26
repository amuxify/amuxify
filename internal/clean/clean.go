// Package clean strips metadata and extended attributes in place without
// touching tracks. Matroska goes through mkvpropedit; MP4-family and AVI go
// through an ffmpeg stream copy into a temp file that replaces the original
// only after its streams verify identical.
package clean

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/pkg/xattr"

	"github.com/nxame/amuxify/internal/exec"
	"github.com/nxame/amuxify/internal/fsutil"
	"github.com/nxame/amuxify/internal/mp4"
	"github.com/nxame/amuxify/internal/policy"
	"github.com/nxame/amuxify/internal/probe"
	"github.com/nxame/amuxify/internal/report"
	"github.com/nxame/amuxify/internal/scan"
	"github.com/nxame/amuxify/internal/verify"
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
	Hardlinks             string
	Timeout               time.Duration
	// Progress, when set, receives each result as soon as the file is done.
	Progress func(report.FileResult)
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
	fi, err := os.Lstat(abs)
	if err != nil {
		return nil, err
	}
	var paths []string
	if fi.IsDir() {
		_ = filepath.WalkDir(abs, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() == ".DS_Store" || strings.HasPrefix(d.Name(), ".amuxify-") {
				return nil
			}
			paths = append(paths, p)
			return nil
		})
		sort.Strings(paths)
	} else {
		paths = []string{abs}
	}
	var out []report.FileResult
	for _, p := range paths {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		fr := c.CleanFile(ctx, p)
		if c.Progress != nil {
			c.Progress(fr)
		}
		out = append(out, fr)
	}
	return out, nil
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
	ext := fsutil.Ext(path)
	if !scan.IsMedia(path) {
		if contains(c.Profile.Sidecars.Block, ext) {
			if c.RemoveBlockedSidecars {
				if c.DryRun {
					fr.Addf(CodeDryRun, report.Pass, "would remove blocked sidecar .%s", ext)
				} else if err := os.Remove(path); err != nil {
					fr.Addf(CodeCleanFail, report.Fail, "remove: %v", err)
				} else {
					fr.Addf(CodeSidecarRemove, report.Pass, "blocked sidecar removed")
				}
				return fr
			}
			fr.Addf(CodeSkipped, report.Warn, "blocked sidecar .%s left in place (use --remove-blocked-sidecars)", ext)
			return fr
		}
		c.stripXattrs(&fr, path)
		if len(fr.Findings) == 0 {
			fr.Addf(CodeNothing, report.Pass, "sidecar; nothing to clean")
		}
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
	switch {
	case info.IsMatroska():
		c.cleanMatroska(ctx, &fr, info)
	case info.Container == "mp4":
		c.cleanRewrite(ctx, &fr, info, mp4Muxer(ext))
	case info.Container == "avi":
		c.cleanRewrite(ctx, &fr, info, "avi")
	case info.Container == "flv":
		c.cleanRewrite(ctx, &fr, info, "flv")
	case info.Container == "mpegts", info.Container == "mpegps":
		fr.Addf(CodeNothing, report.Pass, "%s carries no writable metadata; use remux to convert", info.Container)
	case info.Container == "mp3", info.Container == "flac", info.Container == "ogg", info.Container == "wav":
		if c.StripAudioTags {
			c.cleanRewrite(ctx, &fr, info, audioMuxer(info.Container))
		} else {
			fr.Addf(CodeSkipped, report.Pass, "audio tags left alone (use --strip-audio-tags)")
		}
	default:
		fr.Addf(CodeSkipped, report.Warn, "no cleaner for container %s", info.Container)
	}
	c.stripXattrs(&fr, path)
	return fr
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

func (c *Cleaner) cleanMatroska(ctx context.Context, fr *report.FileResult, info *probe.MediaInfo) {
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
func (c *Cleaner) cleanRewrite(ctx context.Context, fr *report.FileResult, info *probe.MediaInfo, muxer string) {
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
	if len(info.Tags) > 0 {
		what = append(what, fmt.Sprintf("%d format tag(s)", len(info.Tags)))
	}
	for _, s := range info.Streams {
		for k := range s.Tags {
			if strings.EqualFold(k, "language") {
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
	tmp := fsutil.TempName(info.Path)
	_ = os.Remove(tmp)
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
	if err := fsutil.Fsync(tmp); err == nil {
		err = fsutil.ReplaceInPlace(tmp, info.Path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		fr.Addf(CodeCleanFail, report.Fail, "replace: %v", err)
		return
	}
	fr.Addf(CodeMetadata, report.Pass, "rewritten without %s", strings.Join(what, ", "))
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
		match := false
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				match = true
			}
		}
		if !match {
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
