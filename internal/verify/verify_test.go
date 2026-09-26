package verify

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/exec"
	"github.com/amuxify/amuxify/internal/probe"
	"github.com/amuxify/amuxify/internal/testutil"
)

func sha256hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// digestColumnBuffered is the implementation DecodedHash used before the
// decoded stream was hashed as it arrived. It is the reference the
// streaming writer must match byte for byte.
func digestColumnBuffered(out []byte) string {
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

// The streaming digest writer must produce the same hash as the buffered
// implementation for any framemd5 output, however the bytes are chunked,
// including hostile shapes: no trailing newline, comment lines, lines
// without a comma, carriage returns, blank lines and very long lines.
func TestDigestWriterMatchesBufferedReference(t *testing.T) {
	inputs := map[string]string{
		"empty":            "",
		"typical":          "#format: frame checksums\n#version: 2\n#hash: MD5\n#stream#, dts, pts, duration, size, hash\n0,   0,   0,   1,  1382, 6f1e2a\n0,   1,   1,   1,  1382, 9b0c1d\n",
		"no trailing nl":   "0, 0, 0, 1, 10, abc\n0, 1, 1, 1, 10, def",
		"only comments":    "#a\n#b\n",
		"blank lines":      "\n\n0, 0, 0, 1, 10, abc\n\n\n",
		"no comma":         "justonefield\n#c\nanother\n",
		"cr bytes":         "0, 0, 0, 1, 10, abc\r\n0, 1, 1, 1, 10, def\r\n",
		"trailing spaces":  "0, 0, 0, 1, 10,   abc   \n",
		"hash in comment":  "#0, 0, 0, 1, 10, zzz\n0, 0, 0, 1, 10, abc\n",
		"comma at end":     "0, 0, 0, 1, 10,\n",
		"long line":        strings.Repeat("x", 1<<16) + ", " + strings.Repeat("f", 32) + "\n",
		"binary junk":      "\x00\xff, \x01\n#\x00\n,\n",
		"partial last com": "0, 0, 0, 1, 10, abc\n#unterminated comment",
	}
	for name, in := range inputs {
		want := sha256hex([]byte(digestColumnBuffered([]byte(in))))
		for _, chunk := range []int{1, 2, 3, 7, 64, 4096} {
			h := sha256.New()
			dw := &digestWriter{w: h}
			for i := 0; i < len(in); i += chunk {
				end := i + chunk
				if end > len(in) {
					end = len(in)
				}
				if n, err := dw.Write([]byte(in[i:end])); err != nil || n != end-i {
					t.Fatalf("%s chunk %d: write %d %v", name, chunk, n, err)
				}
			}
			dw.flush()
			if got := hex.EncodeToString(h.Sum(nil)); got != want {
				t.Errorf("%s chunk %d: streamed %s want %s", name, chunk, got, want)
			}
		}
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// A write failure inside the digest writer is not swallowed.
func TestDigestWriterReportsWriteError(t *testing.T) {
	dw := &digestWriter{w: failingWriter{}}
	if _, err := dw.Write([]byte("0, 0, abc\n")); err == nil {
		t.Fatal("write error swallowed")
	}
}

// Guarantee 5 rests on the decoded hash being exactly what it was: for
// every video, audio and text subtitle stream of a real file, the streamed
// DecodedHash equals the hash of the fully buffered ffmpeg output computed
// the old way.
func TestDecodedHashEqualsBufferedHash(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe, exec.MKVMerge)
	pr := &probe.Prober{Runner: r, Timeout: time.Minute}
	v := &Verifier{Runner: r, Timeout: time.Minute}
	checked := 0
	for _, name := range []string{"multi.mkv", "clean.mkv", "sample.ts"} {
		path := filepath.Join(testutil.Fixtures(t), name)
		if _, err := os.Stat(path); err != nil {
			t.Logf("%s: %v", name, err)
			continue
		}
		info, err := pr.Probe(context.Background(), path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		for _, s := range info.Streams {
			args := []string{"-v", "error", "-i", path, "-map", "0:" + strconv.Itoa(s.Index)}
			switch s.Type {
			case "video":
				args = append(args, "-f", "framemd5")
			case "audio":
				args = append(args, "-f", "s16le", "-ac", strconv.Itoa(maxInt(s.Channels, 1)))
			case "subtitle":
				if !s.TextSubtitle {
					continue
				}
				args = append(args, "-f", "srt")
			default:
				continue
			}
			res, err := r.RunWithTimeout(context.Background(), time.Minute, exec.FFmpeg, append(args, "-")...)
			if err != nil || res.ExitCode != 0 {
				t.Fatalf("%s stream %d: %v exit %d", name, s.Index, err, res.ExitCode)
			}
			want := sha256hex(res.Stdout)
			if s.Type == "video" {
				want = sha256hex([]byte(digestColumnBuffered(res.Stdout)))
			}
			got, err := v.DecodedHash(context.Background(), path, s)
			if err != nil {
				t.Fatalf("%s stream %d: %v", name, s.Index, err)
			}
			if got != want {
				t.Errorf("%s stream %d (%s): streamed %s, buffered %s", name, s.Index, s.Type, got, want)
			}
			if len(res.Stdout) == 0 {
				t.Errorf("%s stream %d (%s): ffmpeg produced no output, the comparison is vacuous", name, s.Index, s.Type)
			}
			checked++
		}
	}
	if checked < 3 {
		t.Fatalf("only %d streams compared", checked)
	}
}

// DecodedHash exists so a stream can be compared across containers whose
// packet framing differs: clean.mkv and sample.ts carry the same content,
// so their decoded hashes must agree. The hash must still differ when the
// samples differ, or a damaged remux would pass verification; an audio
// copy at half volume proves that.
func TestDecodedHashCrossContainerAndContent(t *testing.T) {
	r := testutil.Need(t, exec.FFmpeg, exec.FFprobe)
	pr := &probe.Prober{Runner: r, Timeout: time.Minute}
	v := &Verifier{Runner: r, Timeout: time.Minute}
	hashesOf := func(path string) map[string]string {
		t.Helper()
		info, err := pr.Probe(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, s := range info.Streams {
			if s.Type != "video" && s.Type != "audio" {
				continue
			}
			h, err := v.DecodedHash(context.Background(), path, s)
			if err != nil {
				t.Fatalf("%s stream %d: %v", path, s.Index, err)
			}
			out[s.Type] = h
		}
		return out
	}
	mkv := hashesOf(filepath.Join(testutil.Fixtures(t), "clean.mkv"))
	ts := hashesOf(filepath.Join(testutil.Fixtures(t), "sample.ts"))
	if mkv["audio"] == "" || mkv["video"] == "" {
		t.Fatalf("clean.mkv hashes incomplete: %v", mkv)
	}
	if mkv["audio"] != ts["audio"] || mkv["video"] != ts["video"] {
		t.Fatalf("the same content hashes differently across containers:\nmkv %v\nts  %v", mkv, ts)
	}
	if mkv["audio"] == mkv["video"] {
		t.Fatal("audio and video hash the same")
	}
	quiet := filepath.Join(t.TempDir(), "quiet.mkv")
	res, err := r.RunWithTimeout(context.Background(), time.Minute, exec.FFmpeg,
		"-v", "error", "-i", filepath.Join(testutil.Fixtures(t), "clean.mkv"), "-map", "0:v", "-map", "0:a", "-c:v", "copy", "-af", "volume=0.5", quiet)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("ffmpeg: %v exit %d %s", err, res.ExitCode, res.Stderr)
	}
	q := hashesOf(quiet)
	if q["audio"] == mkv["audio"] {
		t.Fatal("altered audio hashes like the original")
	}
	if q["video"] != mkv["video"] {
		t.Fatal("copied video hashes differently")
	}
}

// A fake ffmpeg that prints a framemd5 table and then fails must not yield
// a hash: the exit code is checked before the digest is returned, and an
// ffmpeg that writes the table to stderr instead of stdout hashes nothing.
func TestDecodedHashRefusesFailedDecode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake ffmpeg is a POSIX shell script")
	}
	dir := t.TempDir()
	fake := filepath.Join(dir, "ffmpeg")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '0, 0, 0, 1, 10, abc\\n'\nprintf 'Error while decoding stream #0:0\\n' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AMUXIFY_FFMPEG", fake)
	v := &Verifier{Runner: &exec.Runner{}, Timeout: time.Minute}
	h, err := v.DecodedHash(context.Background(), filepath.Join(dir, "x.mkv"), probe.Stream{Index: 0, Type: "video"})
	if err == nil || h != "" {
		t.Fatalf("hash %q err %v from a failed decode", h, err)
	}
	if !strings.Contains(err.Error(), "Error while decoding") {
		t.Fatalf("error does not carry ffmpeg's message: %v", err)
	}
	if _, err := v.DecodedHash(context.Background(), filepath.Join(dir, "x.mkv"), probe.Stream{Index: 1, Type: "subtitle"}); err == nil {
		t.Fatal("bitmap subtitle was hashed")
	}
	if _, err := v.DecodedHash(context.Background(), filepath.Join(dir, "x.mkv"), probe.Stream{Index: 1, Type: "attachment"}); err == nil {
		t.Fatal("attachment was hashed")
	}
}
