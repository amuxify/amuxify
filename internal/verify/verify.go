// Package verify proves things about media files with ffmpeg: that the
// beginning and end decode, that a whole file decodes, and that a stream's
// packets (or, when the container changed the packet framing, its decoded
// samples) are byte-identical between two files.
package verify

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/nxame/amuxify/internal/exec"
	"github.com/nxame/amuxify/internal/probe"
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
	res, err := v.Runner.RunWithTimeout(ctx, v.to(), exec.FFmpeg, append(args, "-")...)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("decode exit %d: %s", res.ExitCode, firstLine(res.Stderr))
	}
	if s.Type == "video" {
		return sha256hex([]byte(digestColumn(res.Stdout))), nil
	}
	return sha256hex(res.Stdout), nil
}

// digestColumn extracts the last comma-separated field of each framemd5
// data line, joined by newlines.
func digestColumn(out []byte) string {
	var sb strings.Builder
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.LastIndexByte(line, ',')
		sb.WriteString(strings.TrimSpace(line[i+1:]))
		sb.WriteByte('\n')
	}
	return sb.String()
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
