package scan

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/amuxify/amuxify/internal/fsutil"
	"github.com/amuxify/amuxify/internal/report"
)

// NfoKind says how Kodi would treat an .nfo file.
type NfoKind string

const (
	NfoKodiXML   NfoKind = "kodi-xml"   // XML with a known Kodi root
	NfoKodiURL   NfoKind = "kodi-url"   // only scraper URLs or a bare IMDb id
	NfoKodiMixed NfoKind = "kodi-mixed" // XML root followed by one scraper URL
	NfoText      NfoKind = "text"       // anything else: release notes, scene nfo
)

// NfoInfo is the classification of one .nfo file.
type NfoInfo struct {
	Kind    NfoKind
	Root    string   // movie tvshow episodedetails musicvideo artist album movies
	Scraper string   // host of the scraper URL found, for example "themoviedb.org"
	Links   []string // "where: url" entries that are neither scraper URLs nor in artwork fields
}

// nfoReadLimit is the number of bytes checkNfo reads from an .nfo file. A
// real Kodi or scene nfo is a few kilobytes; anything past the limit is
// ignored so that a huge file cannot tie up the scan.
const nfoReadLimit = 1 << 20

// maxNfoLinks caps the links recorded for one file, matching the cap in
// FindLinks, so that a crafted file cannot make the report unbounded.
const maxNfoLinks = 20

// kodiRoots are the root elements Kodi recognises in an nfo. The comparison
// is case-sensitive because Kodi's XML parser is.
var kodiRoots = map[string]bool{
	"movie": true, "tvshow": true, "episodedetails": true, "musicvideo": true,
	"artist": true, "album": true, "movies": true,
}

// kodiArtwork names the elements whose text and attributes hold artwork,
// trailer and scraper addresses that Kodi fetches itself. Links inside them
// are expected and are not reported.
var kodiArtwork = map[string]bool{
	"thumb": true, "fanart": true, "trailer": true, "episodeguide": true,
	"url": true, "videourl": true, "thumburl": true, "path": true,
}

// scraperHosts are the sites whose URLs Kodi reads from an nfo to look a
// title up. A host matches when it is one of these or a subdomain of one.
var scraperHosts = []string{"themoviedb.org", "imdb.com", "thetvdb.com", "musicbrainz.org"}

var (
	imdbIDRe    = regexp.MustCompile(`^tt[0-9]{5,}$`)
	urlSchemeRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
)

// ClassifyNfo classifies the bytes of an .nfo file. It never touches the
// filesystem; checkNfo reads at most 1 MiB.
func ClassifyNfo(data []byte) NfoInfo {
	s := nfoText(data)
	if info, ok := classifyKodiXML(s); ok {
		return info
	}
	return classifyNfoText(s)
}

// nfoText turns the raw bytes into valid UTF-8. A UTF-8 byte order mark is
// dropped and a UTF-16 file with a byte order mark is decoded, so that a
// link cannot hide behind an encoding. Bytes that are not UTF-8, such as
// CP437 box drawing in a scene nfo, become "?" so they cannot break the link
// scanner. A NUL byte becomes a space: Kodi's parser treats the file as a C
// string and stops there, and a NUL must not glue two tokens together.
func nfoText(data []byte) string {
	var text string
	switch {
	case bytes.HasPrefix(data, []byte("\xef\xbb\xbf")):
		data = data[3:]
	case bytes.HasPrefix(data, []byte("\xff\xfe")):
		text = decodeUTF16(data[2:], binary.LittleEndian)
	case bytes.HasPrefix(data, []byte("\xfe\xff")):
		text = decodeUTF16(data[2:], binary.BigEndian)
	}
	if text == "" {
		text = strings.ToValidUTF8(string(data), "?")
	}
	return strings.ReplaceAll(text, "\x00", " ")
}

func decodeUTF16(b []byte, order binary.ByteOrder) string {
	units := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		units = append(units, order.Uint16(b[i:]))
	}
	return string(utf16.Decode(units))
}

// classifyKodiXML runs the XML pass. It reports false when the text is not
// an XML document with a known Kodi root, in which case the caller treats
// the whole text as plain text.
func classifyKodiXML(s string) (NfoInfo, bool) {
	dec := xml.NewDecoder(strings.NewReader(s))
	dec.Strict = false
	// Kodi nfo files often declare an encoding such as ISO-8859-1. The bytes
	// were already made valid UTF-8, so the declared charset is ignored.
	dec.CharsetReader = func(_ string, input io.Reader) (io.Reader, error) { return input, nil }

	var root xml.StartElement
	for {
		tok, err := dec.Token()
		if err != nil {
			return NfoInfo{}, false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			root = t
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				// Text before the root means Kodi's parser would reject the
				// document, so the whole file is treated as text.
				return NfoInfo{}, false
			}
			continue
		default:
			// Processing instructions, comments and the DOCTYPE directive are
			// skipped. Go's decoder never expands entity declarations or
			// fetches external subsets.
			continue
		}
		break
	}
	if !kodiRoots[root.Name.Local] {
		return NfoInfo{}, false
	}

	info := NfoInfo{Kind: NfoKodiXML, Root: root.Name.Local}
	path := []string{root.Name.Local}
	artwork := 0 // number of artwork elements on the current path
	feed := func(text string) {
		if artwork > 0 || len(info.Links) >= maxNfoLinks {
			return
		}
		for _, l := range findNfoLinks(text) {
			if _, ok := scraperHost(l); ok {
				continue
			}
			info.Links = append(info.Links, elementPath(path)+": "+l)
			if len(info.Links) >= maxNfoLinks {
				return
			}
		}
	}
	for _, a := range root.Attr {
		feed(a.Value)
	}
walk:
	for {
		tok, err := dec.Token()
		if err != nil {
			// Any error before the root closes, including EOF, means the
			// document is malformed and Kodi would not load it.
			return NfoInfo{}, false
		}
		switch t := tok.(type) {
		case xml.StartElement:
			path = append(path, t.Name.Local)
			if kodiArtwork[t.Name.Local] {
				artwork++
			}
			for _, a := range t.Attr {
				feed(a.Value)
			}
		case xml.EndElement:
			if kodiArtwork[path[len(path)-1]] {
				artwork--
			}
			path = path[:len(path)-1]
			if len(path) == 0 {
				break walk
			}
		case xml.CharData:
			feed(string(t))
		}
	}

	rest := strings.TrimSpace(s[dec.InputOffset():])
	if rest == "" {
		return info, true
	}
	if fields := strings.Fields(stripInvisible(rest)); len(fields) == 1 {
		if host, ok := scraperHost(fields[0]); ok {
			info.Kind = NfoKodiMixed
			info.Scraper = host
			return info, true
		}
	}
	for _, l := range findNfoLinks(rest) {
		if _, ok := scraperHost(l); ok {
			continue
		}
		if len(info.Links) >= maxNfoLinks {
			break
		}
		info.Links = append(info.Links, "trailing text: "+l)
	}
	return info, true
}

// classifyNfoText handles everything that is not Kodi XML: scraper URL
// files, bare IMDb ids and scene release notes.
func classifyNfoText(s string) NfoInfo {
	info := NfoInfo{Kind: NfoText}
	fields := strings.Fields(stripInvisible(s))
	if len(fields) == 0 {
		return info
	}
	onlyScrapers := true
	for _, f := range fields {
		if imdbIDRe.MatchString(f) {
			if info.Scraper == "" {
				info.Scraper = "imdb.com"
			}
			continue
		}
		if host, ok := scraperHost(f); ok {
			if info.Scraper == "" {
				info.Scraper = host
			}
			continue
		}
		onlyScrapers = false
		break
	}
	if onlyScrapers {
		info.Kind = NfoKodiURL
		return info
	}
	info.Scraper = ""
	for _, l := range findNfoLinks(s) {
		if host, ok := scraperHost(l); ok {
			if info.Scraper == "" {
				info.Scraper = host
			}
			continue
		}
		if len(info.Links) >= maxNfoLinks {
			break
		}
		info.Links = append(info.Links, "text: "+l)
	}
	return info
}

// scraperHost reports whether a link token is a scraper URL and, when it
// is, which scraper site it belongs to. The host is taken from a real URL
// parse, so a look-alike such as themoviedb.org.evil.example, a path that
// merely mentions the site, or user information in front of the real host
// never counts. An IMDb URL counts only under /title/.
func scraperHost(token string) (string, bool) {
	t := token
	if !urlSchemeRe.MatchString(t) {
		t = "http://" + t
	}
	u, err := url.Parse(t)
	if err != nil {
		return "", false
	}
	host := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
	if host == "" {
		return "", false
	}
	for _, h := range scraperHosts {
		if host != h && !strings.HasSuffix(host, "."+h) {
			continue
		}
		if h == "imdb.com" && !strings.HasPrefix(u.Path, "/title/") {
			return "", false
		}
		return h, true
	}
	return "", false
}

// findNfoLinks runs FindLinks over text with zero-width and bidirectional
// control characters removed, so that a link split by invisible characters
// is still found.
func findNfoLinks(text string) []string {
	return FindLinks(stripInvisible(text))
}

// stripInvisible removes the zero-width and bidirectional control characters
// that bidiChars rejects in file names.
func stripInvisible(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 0x200B && bidiChars(string(r)) != "" {
			return -1
		}
		return r
	}, s)
}

// elementPath renders the element stack as movie/plot. A crafted document
// can nest thousands of levels, so the rendering is bounded.
func elementPath(path []string) string {
	const limit = 120
	p := strings.Join(path, "/")
	if len(p) > limit {
		p = "..." + p[len(p)-limit:]
	}
	return p
}

// readNfo reads at most nfoReadLimit bytes from a regular file. It refuses
// symlinks and anything that is not a regular file, and it checks that the
// file it opened is the one it looked at, so a swap between the two steps
// is caught.
func readNfo(path string) ([]byte, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: not a regular file", path)
	}
	f, err := fsutil.OpenRegular(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || !os.SameFile(fi, st) {
		return nil, fmt.Errorf("%s: file changed while it was being read", path)
	}
	return io.ReadAll(io.LimitReader(f, nfoReadLimit))
}

// checkNfo reads the file (io.LimitReader 1 MiB), classifies it and adds findings.
func (s *Scanner) checkNfo(fr *report.FileResult, path string) {
	data, err := readNfo(path)
	if err != nil {
		fr.Addf(CodeUnreadable, report.Fail, "%v", err)
		return
	}
	info := ClassifyNfo(data)
	switch {
	case info.Kind == NfoKodiXML:
		fr.Addf(CodeNfoKodi, report.Pass, "Kodi %s nfo", info.Root)
	case info.Kind == NfoKodiURL:
		fr.Addf(CodeNfoKodi, report.Pass, "Kodi URL nfo (%s)", info.Scraper)
	case info.Kind == NfoKodiMixed:
		fr.Addf(CodeNfoKodi, report.Pass, "Kodi %s nfo with a scraper URL (%s)", info.Root, info.Scraper)
	case info.Scraper != "":
		fr.Addf(CodeNfoKodi, report.Pass, "text nfo with a scraper URL (%s); Kodi uses it as a URL nfo", info.Scraper)
	default:
		fr.Addf(CodeNfoText, report.Pass, "plain text nfo; Kodi ignores it")
	}
	if len(info.Links) == 0 {
		return
	}
	sev := report.Warn
	if s.Profile.Metadata.Links == "fail" {
		sev = report.Fail
	}
	fr.Add(report.Finding{Code: CodeLinkInSidecar, Severity: sev,
		Message: fmt.Sprintf("%d link(s) in nfo outside Kodi artwork and scraper fields: %s", len(info.Links), strings.Join(info.Links, "; ")),
		Detail:  strings.Join(info.Links, "\n")})
}
