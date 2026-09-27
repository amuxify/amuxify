// Package verify proves things about media files with ffmpeg: that the
// beginning and end decode, that a whole file decodes, and that a stream's
// packets (or, when the container changed the packet framing, its decoded
// samples) are byte-identical between two files.
package verify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/probe"
)

// Verifier runs decode and hash checks.
type Verifier struct {
	Runner  *exec.Runner
	Timeout time.Duration // per ffmpeg call; zero means one hour
}

func (v *Verifier) to() time.Duration {
	if v.Timeout == 0 {
		return time.Hour
	}
	return v.Timeout
}

// DecodeHeadTail decodes the first and last 15 seconds of video and audio.
func (v *Verifier) DecodeHeadTail(ctx context.Context, path string) error {
	if err := v.decode(ctx, path, "-t", "15"); err != nil {
		return fmt.Errorf("head: %w", err)
	}
	if err := v.decode(ctx, path, "-sseof", "-15"); err != nil {
		return fmt.Errorf("tail: %w", err)
	}
	return nil
}

// DecodeFull decodes every video and audio stream end to end.
func (v *Verifier) DecodeFull(ctx context.Context, path string) error {
	return v.decode(ctx, path)
}

// Decodable reports whether a probed file holds a stream the decode checks
// map, that is any video or audio stream. A subtitle-only or data-only
// container has nothing for ffmpeg to decode: it would refuse an output
// with no streams and the check would fail for a healthy file.
func Decodable(info *probe.MediaInfo) bool {
	if info == nil {
		return false
	}
	for _, st := range info.Streams {
		if st.Type == "video" || st.Type == "audio" {
			return true
		}
	}
	return false
}

// Decode runs DecodeFull, or DecodeHeadTail when full is false, on a probed
// file and returns nil without running ffmpeg when the file has no
// decodable stream. Callers that know the probe result use this so a
// subtitle-only container is not failed for lacking video and audio.
func (v *Verifier) Decode(ctx context.Context, path string, info *probe.MediaInfo, full bool) error {
	if !Decodable(info) {
		return nil
	}
	if full {
		return v.DecodeFull(ctx, path)
	}
	return v.DecodeHeadTail(ctx, path)
}

func (v *Verifier) decode(ctx context.Context, path string, pre ...string) error {
	args := []string{"-v", "error", "-xerror"}
	args = append(args, pre...)
	args = append(args, "-i", path, "-map", "0:v?", "-map", "0:a?", "-f", "null", "-")
	res, err := v.Runner.RunWithTimeout(ctx, v.to(), exec.FFmpeg, args...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("ffmpeg exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	return nil
}

// StreamHash returns the SHA-256 of a stream's packets without decoding.
func (v *Verifier) StreamHash(ctx context.Context, path string, index int) (string, error) {
	res, err := v.Runner.RunWithTimeout(ctx, v.to(), exec.FFmpeg,
		"-v", "error", "-i", path, "-map", fmt.Sprintf("0:%d", index), "-c", "copy", "-f", "streamhash", "-hash", "sha256", "-")
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("streamhash exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	for _, line := range strings.Split(string(res.Stdout), "\n") {
		if i := strings.Index(line, "SHA256="); i >= 0 {
			return strings.TrimSpace(line[i+7:]), nil
		}
	}
	return "", fmt.Errorf("streamhash produced no hash for stream %d", index)
}

// DecodedHash returns the SHA-256 of a stream's decoded samples. It is the
// fallback when packet framing legitimately changes (MPEG-TS Annex B to
// Matroska AVCC, for example) so packet hashes cannot match.
func (v *Verifier) DecodedHash(ctx context.Context, path string, s probe.Stream) (string, error) {
	args := []string{"-v", "error", "-i", path, "-map", fmt.Sprintf("0:%d", s.Index)}
	switch s.Type {
	case "video":
		// framemd5 yields one digest per decoded frame without any frame
		// duplication or dropping, so a timestamp offset between containers
		// does not change the result. Only the digest column is hashed.
		args = append(args, "-f", "framemd5")
	case "audio":
		args = append(args, "-f", "s16le", "-ac", fmt.Sprint(maxInt(s.Channels, 1)))
	case "subtitle":
		if !s.TextSubtitle {
			return "", fmt.Errorf("bitmap subtitle cannot be sample-hashed")
		}
		args = append(args, "-f", "srt")
	default:
		return "", fmt.Errorf("unsupported stream type %s", s.Type)
	}
	// The decoded stream is hashed as ffmpeg produces it. Raw samples of a
	// feature film run to gigabytes, so they are never held in memory.
	h := sha256.New()
	var w io.Writer = h
	var dw *digestWriter
	if s.Type == "video" {
		dw = &digestWriter{w: h}
		w = dw
	}
	res, err := v.Runner.RunStreaming(ctx, v.to(), exec.FFmpeg, w, append(args, "-")...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("decode exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	if dw != nil {
		dw.flush()
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// digestWriter forwards the last comma-separated field of each framemd5
// data line, followed by a newline, to w. Comment lines starting with "#"
// and empty lines are dropped. Lines may arrive split across writes; a
// final line without a newline is forwarded by flush.
type digestWriter struct {
	w   io.Writer
	buf []byte
}

func (d *digestWriter) Write(p []byte) (int, error) {
	d.buf = append(d.buf, p...)
	for {
		i := bytes.IndexByte(d.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		line := d.buf[:i]
		d.buf = d.buf[i+1:]
		if err := d.line(line); err != nil {
			return 0, err
		}
	}
}

func (d *digestWriter) line(line []byte) error {
	if len(line) == 0 || line[0] == '#' {
		return nil
	}
	i := bytes.LastIndexByte(line, ',')
	field := bytes.TrimSpace(line[i+1:])
	if _, err := d.w.Write(field); err != nil {
		return err
	}
	_, err := d.w.Write([]byte{'\n'})
	return err
}

// flush forwards a trailing line that had no newline.
func (d *digestWriter) flush() {
	if len(d.buf) > 0 {
		_ = d.line(d.buf)
		d.buf = nil
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return s
}
