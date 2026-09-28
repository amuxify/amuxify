// Package sniff identifies file content from bytes rather than from the
// extension. It replaces file(1), which is missing on Alpine and Windows.
package sniff

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/amuxify/amuxify/internal/fsutil"
)

// Kind is a coarse content class.
type Kind string

const (
	Unknown    Kind = "unknown"
	Matroska   Kind = "matroska"
	WebM       Kind = "webm"
	MP4        Kind = "mp4" // also m4v, m4a, mov, 3gp
	AVI        Kind = "avi"
	MPEGTS     Kind = "mpegts"
	MPEGPS     Kind = "mpegps"
	FLV        Kind = "flv"
	ASF        Kind = "asf" // wmv, wma
	OGG        Kind = "ogg"
	FLAC       Kind = "flac"
	WAV        Kind = "wav"
	MP3        Kind = "mp3"
	Text       Kind = "text"
	Executable Kind = "executable"
	Archive    Kind = "archive"
	Image      Kind = "image"
	PDF        Kind = "pdf"
	HTML       Kind = "html"
	Script     Kind = "script"
	Empty      Kind = "empty"
)

// Result of sniffing.
type Result struct {
	Kind   Kind
	Detail string
}

// Video reports whether the kind is a video container amuxify can ingest.
func (k Kind) Video() bool {
	switch k {
	case Matroska, WebM, MP4, AVI, MPEGTS, MPEGPS, FLV, ASF:
		return true
	}
	return false
}

// Audio reports whether the kind is an audio-only container.
func (k Kind) Audio() bool {
	switch k {
	case OGG, FLAC, WAV, MP3:
		return true
	}
	return false
}

// Dangerous reports whether the kind must never appear in a media tree.
func (k Kind) Dangerous() bool {
	switch k {
	case Executable, Archive, PDF, HTML, Script:
		return true
	}
	return false
}

// File sniffs a path by reading its first 8 KiB. The path is opened with
// fsutil.OpenRegular, so a symbolic link is never followed and a named pipe
// planted at the path is refused rather than read from, which would block
// until the planter chose to write.
func File(path string) (Result, error) {
	f, err := fsutil.OpenRegular(path)
	if err != nil {
		return Result{Kind: Unknown}, err
	}
	defer f.Close()
	buf := make([]byte, 8192)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
		return Result{Kind: Unknown}, err
	}
	return Bytes(buf[:n]), nil
}

// Bytes classifies a header buffer.
func Bytes(b []byte) Result {
	if len(b) == 0 {
		return Result{Kind: Empty}
	}
	has := func(off int, sig string) bool {
		return len(b) >= off+len(sig) && string(b[off:off+len(sig)]) == sig
	}
	switch {
	case has(0, "\x1a\x45\xdf\xa3"):
		head := b
		if len(head) > 256 {
			head = head[:256]
		}
		if bytes.Contains(head, []byte("webm")) {
			return Result{Kind: WebM, Detail: "EBML doctype webm"}
		}
		return Result{Kind: Matroska, Detail: "EBML header"}
	case has(4, "ftyp"), has(4, "moov"), has(4, "mdat"), has(4, "wide"), has(4, "free"), has(4, "skip"):
		brand := ""
		if len(b) >= 12 && has(4, "ftyp") {
			brand = strings.TrimSpace(string(b[8:12]))
		}
		return Result{Kind: MP4, Detail: "ISO BMFF brand " + brand}
	case has(0, "RIFF") && has(8, "AVI "):
		return Result{Kind: AVI, Detail: "RIFF AVI"}
	case has(0, "RIFF") && has(8, "WAVE"):
		return Result{Kind: WAV, Detail: "RIFF WAVE"}
	case has(0, "\x00\x00\x01\xba"):
		return Result{Kind: MPEGPS, Detail: "MPEG program stream pack header"}
	case has(0, "FLV\x01"):
		return Result{Kind: FLV}
	case has(0, "\x30\x26\xb2\x75\x8e\x66\xcf\x11"):
		return Result{Kind: ASF, Detail: "ASF/WMV"}
	case has(0, "OggS"):
		return Result{Kind: OGG}
	case has(0, "fLaC"):
		return Result{Kind: FLAC}
	case has(0, "ID3") || (len(b) >= 2 && b[0] == 0xff && b[1]&0xe0 == 0xe0 && b[1]&0x06 != 0):
		return Result{Kind: MP3, Detail: "MPEG audio frame or ID3"}
	case has(0, "MZ"):
		return Result{Kind: Executable, Detail: "PE/DOS executable"}
	case has(0, "\x7fELF"):
		return Result{Kind: Executable, Detail: "ELF"}
	case has(0, "\xfe\xed\xfa\xce"), has(0, "\xfe\xed\xfa\xcf"), has(0, "\xce\xfa\xed\xfe"), has(0, "\xcf\xfa\xed\xfe"), has(0, "\xca\xfe\xba\xbe"):
		return Result{Kind: Executable, Detail: "Mach-O"}
	case has(0, "PK\x03\x04"), has(0, "PK\x05\x06"):
		return Result{Kind: Archive, Detail: "zip"}
	case has(0, "Rar!\x1a\x07"):
		return Result{Kind: Archive, Detail: "rar"}
	case has(0, "7z\xbc\xaf\x27\x1c"):
		return Result{Kind: Archive, Detail: "7z"}
	case has(0, "\x1f\x8b"):
		return Result{Kind: Archive, Detail: "gzip"}
	case has(0, "%PDF-"):
		return Result{Kind: PDF}
	case has(0, "\x89PNG"), has(0, "\xff\xd8\xff"), has(0, "GIF8"), has(0, "BM"), (has(0, "RIFF") && has(8, "WEBP")):
		return Result{Kind: Image}
	case has(0, "#!"):
		return Result{Kind: Script, Detail: "shebang"}
	}
	if isTS(b) {
		return Result{Kind: MPEGTS, Detail: "188-byte packets"}
	}
	if isM2TS(b) {
		return Result{Kind: MPEGTS, Detail: "192-byte packets (m2ts)"}
	}
	if looksText(b) {
		lower := strings.ToLower(string(b))
		if strings.Contains(lower, "<html") || strings.Contains(lower, "<!doctype html") || strings.Contains(lower, "<script") {
			return Result{Kind: HTML}
		}
		return Result{Kind: Text}
	}
	return Result{Kind: Unknown}
}

func isTS(b []byte) bool {
	if len(b) < 188*3 {
		return false
	}
	return b[0] == 0x47 && b[188] == 0x47 && b[376] == 0x47
}

func isM2TS(b []byte) bool {
	if len(b) < 192*3+4 {
		return false
	}
	return b[4] == 0x47 && b[196] == 0x47 && b[388] == 0x47
}

func looksText(b []byte) bool {
	if bytes.HasPrefix(b, []byte("\xef\xbb\xbf")) {
		b = b[3:]
	}
	// UTF-16 with BOM is common for subtitles from Windows tools.
	if bytes.HasPrefix(b, []byte("\xff\xfe")) || bytes.HasPrefix(b, []byte("\xfe\xff")) {
		return true
	}
	if !utf8.Valid(b) {
		// Allow a truncated final rune from the 8 KiB window.
		trim := b
		for i := 0; i < 4 && len(trim) > 0 && !utf8.Valid(trim); i++ {
			trim = trim[:len(trim)-1]
		}
		if !utf8.Valid(trim) {
			return false
		}
		b = trim
	}
	for _, r := range string(b) {
		if r == 0 {
			return false
		}
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' && r != '\f' {
			return false
		}
	}
	return true
}

// ExtensionKinds maps a lower-case extension to the kinds it may legitimately
// contain. Unknown extensions map to nil.
func ExtensionKinds(ext string) []Kind {
	switch ext {
	case "mkv", "mka", "mks":
		return []Kind{Matroska}
	case "webm":
		return []Kind{WebM, Matroska}
	case "mp4", "m4v", "mov", "m4a", "3gp", "3g2":
		return []Kind{MP4}
	case "avi":
		return []Kind{AVI}
	case "ts", "m2ts", "mts":
		return []Kind{MPEGTS}
	case "mpg", "mpeg", "vob":
		return []Kind{MPEGPS}
	case "flv":
		return []Kind{FLV}
	case "wmv", "wma", "asf":
		return []Kind{ASF}
	case "ogg", "ogv", "oga", "opus":
		return []Kind{OGG}
	case "flac":
		return []Kind{FLAC}
	case "wav":
		return []Kind{WAV}
	case "mp3":
		return []Kind{MP3}
	case "srt", "ass", "ssa", "vtt", "sub", "txt", "nfo", "sfv", "md5", "sha256":
		return []Kind{Text}
	case "idx":
		return []Kind{Text}
	case "jpg", "jpeg", "png", "gif", "webp", "bmp":
		return []Kind{Image}
	case "par2":
		return []Kind{Unknown}
	}
	return nil
}
