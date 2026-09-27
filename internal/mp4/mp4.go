// Package mp4 is a minimal ISO BMFF (MP4, M4V, MOV, M4A) box walker. It exists
// to find purchase and provenance atoms that ffprobe does not expose, to check
// that moov and mdat are present and complete, and to list XMP uuid boxes.
// It never decodes media.
package mp4

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/amuxify/amuxify/internal/fsutil"
)

// Info is what the walker learned.
type Info struct {
	TopLevel  []string // box types in file order
	HasMoov   bool
	HasMdat   bool
	Truncated bool   // a declared box size runs past the end of the file
	TruncNote string // which box and by how much
	// Ilst holds iTunes-style metadata items: key -> printable value.
	Ilst map[string]string
	// Udta lists user-data box types found directly under moov/udta.
	Udta []string
	// UUIDs lists extended-type boxes (hex) at any level; XMP uses one.
	UUIDs  []string
	HasXMP bool
}

// Provenance lists ilst keys that identify the buyer, the store, or the
// encoding tool. They are never needed for playback.
var Provenance = map[string]string{
	"ownr":                                  "purchaser name",
	"apID":                                  "iTunes account id",
	"purd":                                  "purchase date",
	"xid ":                                  "vendor identifier",
	"cprt":                                  "copyright notice",
	"©too":                                  "encoding tool",
	"©enc":                                  "encoded by",
	"©swr":                                  "software",
	"akID":                                  "iTunes account kind",
	"atID":                                  "iTunes artist id",
	"cnID":                                  "iTunes catalog id",
	"geID":                                  "iTunes genre id",
	"plID":                                  "iTunes playlist id",
	"sfID":                                  "iTunes storefront id",
	"flvr":                                  "iTunes flavour",
	"©url":                                  "URL",
	"ldes":                                  "long description",
	"desc":                                  "description",
	"©cmt":                                  "comment",
	"----:com.apple.iTunes:iTunMOVI":        "iTunes movie XML",
	"----:com.apple.iTunes:iTunEXTC":        "iTunes content rating",
	"----:com.apple.iTunes:Encoding Params": "encoder parameters",
	"----:com.apple.iTunes:iTunNORM":        "iTunes normalisation",
	"----:com.apple.iTunes:iTunes_CDDB_IDs": "CDDB ids",
	"----:com.apple.iTunes:iTunes_CDDB_1":   "CDDB id",
	"----:com.apple.iTunes:iTunes_CDDB_TrackNumber": "CDDB track",
}

// IsProvenance reports whether a key is a provenance atom, treating any
// freeform "----" key as provenance unless it is a known-benign one.
func IsProvenance(key string) (string, bool) {
	if d, ok := Provenance[key]; ok {
		return d, true
	}
	if strings.HasPrefix(key, "----:") {
		return "freeform metadata", true
	}
	return "", false
}

// ProvenanceKeys returns the sorted provenance keys present in info.
func (in *Info) ProvenanceKeys() []string {
	var keys []string
	for k := range in.Ilst {
		if _, ok := IsProvenance(k); ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// Parse walks a file. The path is opened with fsutil.OpenRegular, so a
// symbolic link is never followed and a named pipe planted at the path is
// refused rather than read from.
func Parse(path string) (*Info, error) {
	f, err := fsutil.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	in := &Info{Ilst: map[string]string{}}
	w := &walker{r: f, size: fi.Size(), in: in}
	if err := w.walk(0, fi.Size(), nil, 0); err != nil {
		return in, err
	}
	return in, nil
}

type walker struct {
	r    io.ReaderAt
	size int64
	in   *Info
}

// containers whose children are boxes. meta carries a 4-byte version/flags
// prefix in ISO files but not in QuickTime files; detectFullBox handles it.
var containers = map[string]bool{
	"moov": true, "udta": true, "meta": true, "ilst": true, "trak": true, "mdia": true,
	"minf": true, "stbl": true, "edts": true, "dinf": true, "moof": true, "traf": true,
}

func (w *walker) walk(start, end int64, path []string, depth int) error {
	if depth > 12 {
		return nil
	}
	off := start
	var hdr [16]byte
	for off+8 <= end {
		n, err := w.r.ReadAt(hdr[:8], off)
		if err != nil && n < 8 {
			return nil
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		typ := string(hdr[4:8])
		hlen := int64(8)
		if size == 1 {
			if _, err := w.r.ReadAt(hdr[8:16], off+8); err != nil {
				return nil
			}
			size = int64(binary.BigEndian.Uint64(hdr[8:16]))
			hlen = 16
		} else if size == 0 {
			size = end - off
		}
		if size < hlen {
			return fmt.Errorf("box %q at %d has invalid size %d", typ, off, size)
		}
		if off+size > w.size {
			w.in.Truncated = true
			w.in.TruncNote = fmt.Sprintf("box %q at offset %d declares %d bytes but file has %d", typ, off, size, w.size-off)
			size = w.size - off
		}
		if depth == 0 {
			w.in.TopLevel = append(w.in.TopLevel, typ)
			if typ == "moov" {
				w.in.HasMoov = true
			}
			if typ == "mdat" {
				w.in.HasMdat = true
			}
		}
		parent := ""
		if len(path) > 0 {
			parent = path[len(path)-1]
		}
		switch {
		case typ == "uuid":
			var u [16]byte
			if _, err := w.r.ReadAt(u[:], off+hlen); err == nil {
				hex := fmt.Sprintf("%x", u)
				w.in.UUIDs = append(w.in.UUIDs, hex)
				if hex == "be7acfcb97a942e89c71999491e3afac" { // XMP
					w.in.HasXMP = true
				}
			}
		case parent == "ilst":
			w.readIlstItem(typ, off+hlen, off+size)
		case parent == "udta":
			w.in.Udta = append(w.in.Udta, typ)
			if containers[typ] {
				w.walkChild(typ, off+hlen, off+size, path, depth)
			}
		case containers[typ]:
			w.walkChild(typ, off+hlen, off+size, path, depth)
		}
		off += size
	}
	return nil
}

func (w *walker) walkChild(typ string, start, end int64, path []string, depth int) {
	if typ == "meta" && w.isFullBox(start) {
		start += 4
	}
	_ = w.walk(start, end, append(append([]string{}, path...), typ), depth+1)
}

// isFullBox guesses whether a meta box carries version/flags by checking
// whether the bytes right after the header look like a child box type.
func (w *walker) isFullBox(start int64) bool {
	var b [12]byte
	if _, err := w.r.ReadAt(b[:], start); err != nil {
		return false
	}
	if isBoxType(b[4:8]) {
		return false
	}
	return isBoxType(b[8:12])
}

func isBoxType(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			if c != 0xa9 { // © in Mac Roman
				return false
			}
		}
	}
	return true
}

func (w *walker) readIlstItem(typ string, start, end int64) {
	key := strings.ReplaceAll(typ, "\xa9", "©")
	var mean, name, value string
	off := start
	var hdr [8]byte
	for off+8 <= end {
		if _, err := w.r.ReadAt(hdr[:], off); err != nil {
			return
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		sub := string(hdr[4:8])
		if size < 8 || off+size > end {
			return
		}
		payload := w.readN(off+8, size-8, 4096)
		switch sub {
		case "mean":
			if len(payload) > 4 {
				mean = string(payload[4:])
			}
		case "name":
			if len(payload) > 4 {
				name = string(payload[4:])
			}
		case "data":
			if len(payload) >= 8 {
				dtype := binary.BigEndian.Uint32(payload[:4])
				body := payload[8:]
				switch dtype {
				case 1, 2, 3: // UTF-8, UTF-16, S/JIS
					value = printable(string(body))
				case 21, 22: // signed/unsigned int
					value = fmt.Sprintf("%d", beInt(body))
				default:
					value = fmt.Sprintf("<%d bytes, type %d>", size-16, dtype)
				}
			}
		}
		off += size
	}
	if typ == "----" && (mean != "" || name != "") {
		key = "----:" + mean + ":" + name
	}
	if _, dup := w.in.Ilst[key]; dup {
		key = key + fmt.Sprintf("#%d", len(w.in.Ilst))
	}
	w.in.Ilst[key] = value
}

func (w *walker) readN(off, n, max int64) []byte {
	if n > max {
		n = max
	}
	b := make([]byte, n)
	k, _ := w.r.ReadAt(b, off)
	return b[:k]
}

func beInt(b []byte) int64 {
	var v int64
	for _, c := range b {
		v = v<<8 | int64(c)
	}
	return v
}

func printable(s string) string {
	s = strings.ToValidUTF8(s, "?")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, s)
	if len(s) > 512 {
		s = s[:512] + "…"
	}
	return s
}
