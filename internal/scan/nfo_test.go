package scan

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amuxify/amuxify/internal/policy"
	"github.com/amuxify/amuxify/internal/report"
)

// The Kodi fixtures below mirror testdata/gen-fixtures.sh byte for byte so
// that this package's expectations and the corpus test agree.
const (
	kodiMovie = `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>
<movie>
  <title>Sample Movie</title>
  <plot>A short test film with no links in the text.</plot>
  <uniqueid type="imdb" default="true">tt0120616</uniqueid>
  <thumb aspect="poster" preview="https://image.tmdb.org/t/p/w500/poster.jpg">https://image.tmdb.org/t/p/original/poster.jpg</thumb>
  <fanart><thumb preview="https://image.tmdb.org/t/p/w780/fanart.jpg">https://image.tmdb.org/t/p/original/fanart.jpg</thumb></fanart>
  <trailer>plugin://plugin.video.youtube/?action=play_video&amp;videoid=abc123</trailer>
  <actor><name>Some Actor</name><role>Lead</role><thumb>https://image.tmdb.org/t/p/w500/actor.jpg</thumb></actor>
</movie>
`
	kodiTvshow = `<tvshow>
  <title>Sample Show</title>
  <plot>Episodes about testing.</plot>
  <thumb aspect="poster" season="1" type="season">https://artworks.thetvdb.com/banners/seasons/1-1.jpg</thumb>
  <episodeguide>{"tvdb":"121361","tmdb":"1399"}</episodeguide>
</tvshow>
`
	kodiTvshowURLGuide = `<tvshow>
  <title>Sample Show</title>
  <episodeguide><url cache="tmdb-1399.json">https://api.themoviedb.org/3/tv/1399?language=en</url></episodeguide>
</tvshow>
`
	kodiEpisode = `<episodedetails>
  <title>Pilot</title>
  <season>1</season>
  <episode>1</episode>
  <thumb>https://artworks.thetvdb.com/banners/episodes/121361/1.jpg</thumb>
</episodedetails>
`
	kodiMovies  = "<movies><movie><title>One</title><plot>First.</plot></movie><movie><title>Two</title><plot>Second.</plot></movie></movies>\n"
	kodiURL     = "https://www.themoviedb.org/movie/11-star-wars\n"
	kodiMixed   = "<movie><title>Mixed</title><plot>Plain plot.</plot></movie>\nhttps://www.themoviedb.org/movie/11\n"
	kodiBadPlot = "<movie><title>Bad Plot</title><plot>Get more at http://tracker.example/x</plot></movie>\n"
	kodiBroken  = "<movie><title>Broken</title><plot>unterminated\n"
	sceneBanner = "\xdb\xdb\xdb\xdb\xdb\xdb\xdb\xdb\xdb\xdb\xdb\xdb\r\n\xb0\xb1\xb2 SAMPLE.GROUP \xb2\xb1\xb0\r\n\r\n"
	scenePlain  = sceneBanner + "Release notes: 1080p, x264, 5.1 audio.\r\nGreets to everyone.\r\n"
	sceneLinks  = sceneBanner + "Visit www.example-group.to for more.\r\n"
	sceneImdb   = sceneBanner + "https://www.imdb.com/title/tt0120616/\r\n"
)

type nfoCase struct {
	name    string
	in      string
	kind    NfoKind
	root    string
	scraper string
	links   []string
}

func checkNfoCase(t *testing.T, c nfoCase) {
	t.Helper()
	got := ClassifyNfo([]byte(c.in))
	if got.Kind != c.kind {
		t.Errorf("%s: kind %q, want %q (%+v)", c.name, got.Kind, c.kind, got)
	}
	if got.Root != c.root {
		t.Errorf("%s: root %q, want %q", c.name, got.Root, c.root)
	}
	if got.Scraper != c.scraper {
		t.Errorf("%s: scraper %q, want %q", c.name, got.Scraper, c.scraper)
	}
	if strings.Join(got.Links, "|") != strings.Join(c.links, "|") {
		t.Errorf("%s: links %q, want %q", c.name, got.Links, c.links)
	}
}

func TestClassifyNfo(t *testing.T) {
	cases := []nfoCase{
		{name: "kodi movie with artwork", in: kodiMovie, kind: NfoKodiXML, root: "movie"},
		{name: "tvshow with json episodeguide", in: kodiTvshow, kind: NfoKodiXML, root: "tvshow"},
		{name: "tvshow with url episodeguide", in: kodiTvshowURLGuide, kind: NfoKodiXML, root: "tvshow"},
		{name: "episodedetails", in: kodiEpisode, kind: NfoKodiXML, root: "episodedetails"},
		{name: "movies container", in: kodiMovies, kind: NfoKodiXML, root: "movies"},
		{name: "url only tmdb", in: kodiURL, kind: NfoKodiURL, scraper: "themoviedb.org"},
		{name: "url only tvdb", in: "https://thetvdb.com/series/the-show\n", kind: NfoKodiURL, scraper: "thetvdb.com"},
		{name: "url only musicbrainz", in: "https://musicbrainz.org/release/1234\n", kind: NfoKodiURL, scraper: "musicbrainz.org"},
		{name: "two scraper urls", in: "https://www.themoviedb.org/movie/11\nhttps://www.imdb.com/title/tt0120616/\n", kind: NfoKodiURL, scraper: "themoviedb.org"},
		{name: "bare imdb id", in: "tt0120616\n", kind: NfoKodiURL, scraper: "imdb.com"},
		{name: "mixed", in: kodiMixed, kind: NfoKodiMixed, root: "movie", scraper: "themoviedb.org"},
		{name: "scene without links", in: scenePlain, kind: NfoText},
		{name: "scene with group url", in: sceneLinks, kind: NfoText, links: []string{"text: www.example-group.to"}},
		{name: "scene with imdb link", in: sceneImdb, kind: NfoText, scraper: "imdb.com"},
		{name: "tracker in plot", in: kodiBadPlot, kind: NfoKodiXML, root: "movie", links: []string{"movie/plot: http://tracker.example/x"}},
		{name: "comment before root", in: "<!-- http://tracker.example/c -->\n<movie><title>X</title></movie>", kind: NfoKodiXML, root: "movie"},
		{name: "malformed with known root", in: kodiBroken, kind: NfoText},
		{name: "unknown root", in: "<html><body>http://tracker.example/h</body></html>", kind: NfoText, links: []string{"text: http://tracker.example/h"}},
		{name: "empty", in: "", kind: NfoText},
		{name: "whitespace only", in: " \r\n\t", kind: NfoText},
		{name: "cp437 art", in: "\xc9\xcd\xcd\xbb\r\n\xba  \xba\r\n\xc8\xcd\xcd\xbc\r\n", kind: NfoText},
		{name: "utf8 bom", in: "\xef\xbb\xbf<movie><title>BOM</title></movie>", kind: NfoKodiXML, root: "movie"},
		{name: "declared latin1 charset", in: "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><movie><title>Caf\xe9</title></movie>", kind: NfoKodiXML, root: "movie"},
		{name: "scraper url inside plot is not a link", in: "<movie><plot>see https://www.imdb.com/title/tt0120616/</plot></movie>", kind: NfoKodiXML, root: "movie"},
		{name: "xml followed by trailing link", in: "<movie><title>T</title></movie>\nvisit www.example-group.to\n", kind: NfoKodiXML, root: "movie", links: []string{"trailing text: www.example-group.to"}},
		{name: "xml followed by two scraper urls", in: "<movie><title>T</title></movie>\nhttps://www.themoviedb.org/movie/11\nhttps://www.themoviedb.org/movie/12\n", kind: NfoKodiXML, root: "movie"},
		{name: "link in attribute of a plain element", in: "<movie><plot source=\"http://tracker.example/a\">Text.</plot></movie>", kind: NfoKodiXML, root: "movie", links: []string{"movie/plot: http://tracker.example/a"}},
		{name: "link in thumb attribute is artwork", in: "<movie><thumb preview=\"http://cdn.example.net/p.jpg\">http://cdn.example.net/o.jpg</thumb></movie>", kind: NfoKodiXML, root: "movie"},
		{name: "link in text under artwork parent", in: "<movie><fanart><thumb>http://cdn.example.net/f.jpg</thumb></fanart></movie>", kind: NfoKodiXML, root: "movie"},
		{name: "link in set name is reported", in: "<movie><set><name>http://tracker.example/s</name></set></movie>", kind: NfoKodiXML, root: "movie", links: []string{"movie/set/name: http://tracker.example/s"}},
	}
	for _, c := range cases {
		checkNfoCase(t, c)
	}
}

// TestClassifyNfoAdversarial feeds crafted documents to the classifier.
// Every case must return without a panic and must not let a link that Kodi
// would display slip past by hiding it in structure or encoding tricks.
func TestClassifyNfoAdversarial(t *testing.T) {
	cases := []nfoCase{
		{name: "doctype with internal and external entities",
			in: `<!DOCTYPE movie [<!ENTITY xxe SYSTEM "file:///etc/passwd"><!ENTITY lol "http://tracker.example/lol"><!ENTITY ext PUBLIC "-//X" "http://tracker.example/dtd">]>` +
				"<movie><plot>&xxe;&lol;&ext;</plot></movie>",
			kind: NfoKodiXML, root: "movie"},
		{name: "billion laughs is not expanded",
			in: `<!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol1 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;"><!ENTITY lol2 "&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;&lol1;"><!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">]>` +
				"<movie><plot>&lol3;</plot></movie>",
			kind: NfoKodiXML, root: "movie"},
		{name: "external doctype reference",
			in:   `<!DOCTYPE movie SYSTEM "http://tracker.example/movie.dtd"><movie><title>X</title></movie>`,
			kind: NfoKodiXML, root: "movie"},
		{name: "cdata carrying a link",
			in:   "<movie><plot><![CDATA[Get it at http://tracker.example/cd]]></plot></movie>",
			kind: NfoKodiXML, root: "movie", links: []string{"movie/plot: http://tracker.example/cd"}},
		{name: "numeric entities spelling a link",
			in:   "<movie><plot>&#x68;&#116;tp://tracker.example/ent</plot></movie>",
			kind: NfoKodiXML, root: "movie", links: []string{"movie/plot: http://tracker.example/ent"}},
		{name: "link split by zero-width characters in xml",
			in:   "<movie><plot>http://trac​ker.exa‍mple/zw</plot></movie>",
			kind: NfoKodiXML, root: "movie", links: []string{"movie/plot: http://tracker.example/zw"}},
		{name: "bare domain split by zero-width and bidi characters in text",
			in:   "visit www.exa​mple-gr‮oup.to now",
			kind: NfoText, links: []string{"text: www.example-group.to"}},
		{name: "link with bidi override in text",
			in:   "‮http://tracker.example/rtl‬",
			kind: NfoText, links: []string{"text: http://tracker.example/rtl"}},
		{name: "nul bytes in xml are separators and links are still found",
			in:   "<movie><plot>x\x00y</plot></movie> http://tracker.example/nul",
			kind: NfoKodiXML, root: "movie", links: []string{"trailing text: http://tracker.example/nul"}},
		{name: "nul inside a link does not hide it",
			in:   "<movie><plot>http://tracker.example/nu\x00l</plot></movie>",
			kind: NfoKodiXML, root: "movie", links: []string{"movie/plot: http://tracker.example/nu"}},
		{name: "nul bytes in text",
			in:   "a\x00b\x00c www.example-group.to\x00",
			kind: NfoText, links: []string{"text: www.example-group.to"}},
		{name: "invalid utf-8 mid token",
			in:   "<movie><plot>http://tra\xffcker.example/bad</plot></movie>",
			kind: NfoKodiXML, root: "movie", links: []string{"movie/plot: http://tra?cker.example/bad"}},
		{name: "invalid utf-8 in a tag name falls to text",
			in:   "<mo\xffvie><plot>http://tracker.example/t</plot></mo\xffvie>",
			kind: NfoText, links: []string{"text: http://tracker.example/t"}},
		{name: "link in a comment inside the root is inert",
			in:   "<movie><title>X</title><!-- http://tracker.example/c --></movie>",
			kind: NfoKodiXML, root: "movie"},
		{name: "processing instruction before and after the root",
			in:   "<?xml-stylesheet href=\"http://tracker.example/s.xsl\"?><movie><title>X</title></movie><?pi http://tracker.example/after?>",
			kind: NfoKodiXML, root: "movie", links: []string{"trailing text: http://tracker.example/after?"}},
		{name: "upper-case root is not a kodi root",
			in:   "<MOVIE><THUMB>http://tracker.example/up</THUMB></MOVIE>",
			kind: NfoText, links: []string{"text: http://tracker.example/up"}},
		{name: "mixed-case root is not a kodi root",
			in:   "<Movie><plot>x</plot></Movie>",
			kind: NfoText},
		{name: "upper-case artwork element gets no exemption",
			in:   "<movie><THUMB>http://tracker.example/thumb</THUMB></movie>",
			kind: NfoKodiXML, root: "movie", links: []string{"movie/THUMB: http://tracker.example/thumb"}},
		{name: "namespaced root is still movie",
			in:   "<k:movie xmlns:k=\"urn:kodi\"><k:plot>ok</k:plot></k:movie>",
			kind: NfoKodiXML, root: "movie"},
		{name: "look-alike scraper host suffix",
			in:   "https://www.themoviedb.org.evil.example/movie/11\n",
			kind: NfoText, links: []string{"text: https://www.themoviedb.org.evil.example/movie/11"}},
		{name: "scraper host in the path",
			in:   "https://evil.example/themoviedb.org/movie/11\n",
			kind: NfoText, links: []string{"text: https://evil.example/themoviedb.org/movie/11"}},
		{name: "scraper host as userinfo",
			in:   "https://www.themoviedb.org@evil.example/movie/11\n",
			kind: NfoText, links: []string{"text: https://www.themoviedb.org@evil.example/movie/11"}},
		{name: "scraper host in a query",
			in:   "https://evil.example/?u=https://www.themoviedb.org/movie/11\n",
			kind: NfoText, links: []string{"text: https://evil.example/?u=https://www.themoviedb.org/movie/11"}},
		{name: "scraper host in the fragment",
			in:   "https://evil.example/#themoviedb.org\n",
			kind: NfoText, links: []string{"text: https://evil.example/#themoviedb.org"}},
		{name: "prefix look-alike",
			in:   "https://notthemoviedb.org/movie/11\n",
			kind: NfoText, links: []string{"text: https://notthemoviedb.org/movie/11"}},
		{name: "imdb outside /title/ is a link",
			in:   "https://www.imdb.com/list/ls123456789/\n",
			kind: NfoText, links: []string{"text: https://www.imdb.com/list/ls123456789/"}},
		{name: "imdb with upper-case path is a link",
			in:   "https://www.imdb.com/TITLE/tt0120616/\n",
			kind: NfoText, links: []string{"text: https://www.imdb.com/TITLE/tt0120616/"}},
		{name: "imdb look-alike with a title path",
			in:   "https://imdb.com.evil.example/title/tt0120616/\n",
			kind: NfoText, links: []string{"text: https://imdb.com.evil.example/title/tt0120616/"}},
		{name: "upper-case scraper url",
			in:   "HTTPS://WWW.THEMOVIEDB.ORG/movie/11\n",
			kind: NfoKodiURL, scraper: "themoviedb.org"},
		{name: "scraper url with a port and trailing dot host",
			in:   "https://www.themoviedb.org.:443/movie/11\n",
			kind: NfoKodiURL, scraper: "themoviedb.org"},
		{name: "scraper url and a tracker together",
			in:   "https://www.themoviedb.org/movie/11 http://tracker.example/t\n",
			kind: NfoText, scraper: "themoviedb.org", links: []string{"text: http://tracker.example/t"}},
		{name: "short tt id is not a scraper id",
			in:   "tt1234\n",
			kind: NfoText},
		{name: "tt id with a suffix is text and the torrent name is a link",
			in:   "tt0120616.torrent\n",
			kind: NfoText, links: []string{"text: tt0120616.torrent"}},
		{name: "html root with script is text",
			in:   "<html><script>location='http://tracker.example/js'</script></html>",
			kind: NfoText, links: []string{"text: http://tracker.example/js"}},
		{name: "text before the root disables the xml pass",
			in:   "GROUP PRESENTS <movie><thumb>http://tracker.example/pre</thumb></movie>",
			kind: NfoText, links: []string{"text: http://tracker.example/pre"}},
		{name: "unclosed root followed by link",
			in:   "<movie><plot>x</plot>\nhttp://tracker.example/open\n",
			kind: NfoText, links: []string{"text: http://tracker.example/open"}},
		{name: "mismatched end tags are balanced by the decoder",
			in:   "<movie><plot>http://tracker.example/m</title></movie>",
			kind: NfoKodiXML, root: "movie", links: []string{"movie/plot: http://tracker.example/m"}},
		{name: "second root after the first is trailing text",
			in:   "<movie><title>A</title></movie><movie><plot>http://tracker.example/2</plot></movie>",
			kind: NfoKodiXML, root: "movie", links: []string{"trailing text: http://tracker.example/2"}},
		{name: "trailing bare imdb id is not mixed",
			in:   "<movie><title>A</title></movie>\ntt0120616\n",
			kind: NfoKodiXML, root: "movie"},
		{name: "trailing scraper url plus text is not mixed",
			in:   "<movie><title>A</title></movie>\nhttps://www.themoviedb.org/movie/11 thanks\n",
			kind: NfoKodiXML, root: "movie"},
		{name: "artwork element used as the root is not kodi",
			in:   "<thumb>http://tracker.example/root</thumb>",
			kind: NfoText, links: []string{"text: http://tracker.example/root"}},
		{name: "utf-16 little endian with bom",
			in:   utf16le("<movie><plot>http://tracker.example/u16</plot></movie>"),
			kind: NfoKodiXML, root: "movie", links: []string{"movie/plot: http://tracker.example/u16"}},
		{name: "utf-16 big endian with bom",
			in:   utf16be("see http://tracker.example/be"),
			kind: NfoText, links: []string{"text: http://tracker.example/be"}},
		{name: "utf-16 with an odd trailing byte",
			in:   utf16le("tt0120616") + "\x00",
			kind: NfoKodiURL, scraper: "imdb.com"},
		{name: "lone surrogate in utf-16",
			in:   "\xff\xfe\x00\xd8h\x00t\x00t\x00p\x00:\x00/\x00/\x00t\x00.\x00e\x00x\x00a\x00m\x00p\x00l\x00e\x00/\x00",
			kind: NfoText, links: []string{"text: http://t.example/"}},
		{name: "many links are capped",
			in:   manyLinks(500),
			kind: NfoText, links: linkList(maxNfoLinks)},
	}
	for _, c := range cases {
		checkNfoCase(t, c)
	}
}

func utf16le(s string) string {
	var b bytes.Buffer
	b.WriteString("\xff\xfe")
	for _, r := range s {
		b.WriteByte(byte(r))
		b.WriteByte(byte(r >> 8))
	}
	return b.String()
}

func utf16be(s string) string {
	var b bytes.Buffer
	b.WriteString("\xfe\xff")
	for _, r := range s {
		b.WriteByte(byte(r >> 8))
		b.WriteByte(byte(r))
	}
	return b.String()
}

func manyLinks(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("http://tracker.example/")
		b.WriteString(strings.Repeat("a", i%7+1))
		b.WriteString(itoa(i))
		b.WriteString("\n")
	}
	return b.String()
}

func linkList(n int) []string {
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, "text: http://tracker.example/"+strings.Repeat("a", i%7+1)+itoa(i))
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var d []byte
	for ; i > 0; i /= 10 {
		d = append([]byte{byte('0' + i%10)}, d...)
	}
	return string(d)
}

// TestClassifyNfoDeepNesting proves that thousands of nested elements,
// closed or not, are handled in linear time and bounded memory, and that a
// link at the bottom of a deep document is still reported.
func TestClassifyNfoDeepNesting(t *testing.T) {
	const depth = 50000
	open := strings.Repeat("<a>", depth)
	closeTags := strings.Repeat("</a>", depth)
	start := time.Now()

	got := ClassifyNfo([]byte("<movie>" + open + "http://tracker.example/deep" + closeTags + "</movie>"))
	if got.Kind != NfoKodiXML || len(got.Links) != 1 {
		t.Fatalf("closed deep document: %+v", got)
	}
	if !strings.HasPrefix(got.Links[0], "...") || !strings.HasSuffix(got.Links[0], "/a: http://tracker.example/deep") {
		t.Fatalf("deep path not bounded: %q", got.Links[0])
	}
	if len(got.Links[0]) > 200 {
		t.Fatalf("deep path too long: %d", len(got.Links[0]))
	}

	got = ClassifyNfo([]byte("<movie>" + open + "http://tracker.example/never"))
	if got.Kind != NfoText || len(got.Links) != 1 || got.Links[0] != "text: http://tracker.example/never" {
		t.Fatalf("unclosed deep document: %+v", got)
	}

	// Text between every level, so the artwork check runs at every depth.
	var b strings.Builder
	b.WriteString("<movie>")
	for i := 0; i < depth; i++ {
		b.WriteString("<a>x")
	}
	b.WriteString("http://tracker.example/leaf")
	b.WriteString(closeTags)
	b.WriteString("</movie>")
	got = ClassifyNfo([]byte(b.String()))
	if got.Kind != NfoKodiXML || len(got.Links) != 1 {
		t.Fatalf("text-between deep document: %+v", got)
	}

	// Artwork wrapping at depth must still suppress the link and must
	// unwind correctly.
	got = ClassifyNfo([]byte("<movie><thumb>" + open + "http://cdn.example.net/x" + closeTags + "</thumb><plot>http://tracker.example/after</plot></movie>"))
	if got.Kind != NfoKodiXML || len(got.Links) != 1 || got.Links[0] != "movie/plot: http://tracker.example/after" {
		t.Fatalf("artwork unwinding: %+v", got)
	}

	if d := time.Since(start); d > 20*time.Second {
		t.Fatalf("deep nesting took %v", d)
	}
}

// TestClassifyNfoJustOverLimit checks the pure classifier on an input one
// byte longer than the read limit and on a link that straddles the limit:
// the classifier itself has no limit, the reader applies it.
func TestClassifyNfoJustOverLimit(t *testing.T) {
	in := make([]byte, nfoReadLimit+1)
	for i := range in {
		in[i] = 'x'
	}
	got := ClassifyNfo(in)
	if got.Kind != NfoText || len(got.Links) != 0 {
		t.Fatalf("1 MiB + 1: %+v", got)
	}
	pad := strings.Repeat("y ", nfoReadLimit/2)
	got = ClassifyNfo([]byte(pad + "http://tracker.example/end"))
	if got.Kind != NfoText || len(got.Links) != 1 {
		t.Fatalf("link after 1 MiB of text: %+v", got)
	}
}

func nfoScanner(t *testing.T, overlay string) *Scanner {
	t.Helper()
	var p *policy.Profile
	var err error
	if overlay == "" {
		p, err = policy.Load("homelab")
	} else {
		p, err = policy.Parse([]byte(overlay))
	}
	if err != nil {
		t.Fatal(err)
	}
	return &Scanner{Profile: p}
}

func writeNfo(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func findingsOf(fr *report.FileResult, code string) []report.Finding {
	var out []report.Finding
	for _, f := range fr.Findings {
		if f.Code == code {
			out = append(out, f)
		}
	}
	return out
}

func TestNfoFindingsPerProfile(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"movie.nfo":   kodiMovie,
		"tvshow.nfo":  kodiTvshow,
		"episode.nfo": kodiEpisode,
		"url.nfo":     kodiURL,
		"mixed.nfo":   kodiMixed,
		"badplot.nfo": kodiBadPlot,
		"broken.nfo":  kodiBroken,
		"plain.nfo":   scenePlain,
		"links.nfo":   sceneLinks,
		"imdb.nfo":    sceneImdb,
		"sample.nfo":  "nfo text\n",
	}
	for n, c := range files {
		writeNfo(t, dir, n, c)
	}
	type want struct {
		verdict report.Severity
		code    string
		message string
		links   bool
	}
	wants := map[string]want{
		"movie.nfo":   {report.Pass, CodeNfoKodi, "Kodi movie nfo", false},
		"tvshow.nfo":  {report.Pass, CodeNfoKodi, "Kodi tvshow nfo", false},
		"episode.nfo": {report.Pass, CodeNfoKodi, "Kodi episodedetails nfo", false},
		"url.nfo":     {report.Pass, CodeNfoKodi, "Kodi URL nfo (themoviedb.org)", false},
		"mixed.nfo":   {report.Pass, CodeNfoKodi, "Kodi movie nfo with a scraper URL (themoviedb.org)", false},
		"badplot.nfo": {report.Warn, CodeNfoKodi, "Kodi movie nfo", true},
		"broken.nfo":  {report.Pass, CodeNfoText, "plain text nfo; Kodi ignores it", false},
		"plain.nfo":   {report.Pass, CodeNfoText, "plain text nfo; Kodi ignores it", false},
		"links.nfo":   {report.Warn, CodeNfoText, "plain text nfo; Kodi ignores it", true},
		"imdb.nfo":    {report.Pass, CodeNfoKodi, "text nfo with a scraper URL (imdb.com); Kodi uses it as a URL nfo", false},
		"sample.nfo":  {report.Pass, CodeNfoText, "plain text nfo; Kodi ignores it", false},
	}

	for _, tc := range []struct {
		name    string
		overlay string
		linkSev report.Severity
	}{
		{"homelab", "", report.Warn},
		{"links fail", "[metadata]\nlinks = \"fail\"\n", report.Fail},
	} {
		s := nfoScanner(t, tc.overlay)
		for name, w := range wants {
			fr := &report.FileResult{Path: filepath.Join(dir, name), Info: map[string]string{}}
			s.checkNfo(fr, fr.Path)
			kind := findingsOf(fr, w.code)
			if len(kind) != 1 || kind[0].Severity != report.Pass || kind[0].Message != w.message {
				t.Errorf("%s %s: kind findings %+v, want one %s PASS %q", tc.name, name, fr.Findings, w.code, w.message)
			}
			if w.code == CodeNfoKodi && fr.Has(CodeNfoText) || w.code == CodeNfoText && fr.Has(CodeNfoKodi) {
				t.Errorf("%s %s: both nfo codes present: %+v", tc.name, name, fr.Findings)
			}
			links := findingsOf(fr, CodeLinkInSidecar)
			wantVerdict := w.verdict
			if w.links {
				if len(links) != 1 || links[0].Severity != tc.linkSev {
					t.Errorf("%s %s: link findings %+v, want one at %s", tc.name, name, links, tc.linkSev)
				} else if !strings.HasPrefix(links[0].Message, "1 link(s) in nfo outside Kodi artwork and scraper fields: ") || links[0].Detail == "" {
					t.Errorf("%s %s: link message %q detail %q", tc.name, name, links[0].Message, links[0].Detail)
				}
				wantVerdict = tc.linkSev
			} else if len(links) != 0 {
				t.Errorf("%s %s: unexpected link findings %+v", tc.name, name, links)
			}
			if fr.Verdict != wantVerdict {
				t.Errorf("%s %s: verdict %s, want %s (%+v)", tc.name, name, fr.Verdict, wantVerdict, fr.Findings)
			}
		}
	}

	// The message and detail formats for several links.
	s := nfoScanner(t, "")
	p := writeNfo(t, dir, "two.nfo", "<movie><plot>http://tracker.example/1</plot><outline>http://tracker.example/2</outline></movie>")
	fr := &report.FileResult{Path: p, Info: map[string]string{}}
	s.checkNfo(fr, p)
	links := findingsOf(fr, CodeLinkInSidecar)
	if len(links) != 1 {
		t.Fatalf("two links: %+v", fr.Findings)
	}
	wantMsg := "2 link(s) in nfo outside Kodi artwork and scraper fields: movie/plot: http://tracker.example/1; movie/outline: http://tracker.example/2"
	if links[0].Message != wantMsg {
		t.Errorf("message %q, want %q", links[0].Message, wantMsg)
	}
	if links[0].Detail != "movie/plot: http://tracker.example/1\nmovie/outline: http://tracker.example/2" {
		t.Errorf("detail %q", links[0].Detail)
	}
}

// TestNfoThroughScanFile runs the whole sidecar path: SIDECAR_OK first, then
// the nfo findings, and the 1 MiB read limit applied by checkNfo.
func TestNfoThroughScanFile(t *testing.T) {
	dir := t.TempDir()
	s := nfoScanner(t, "")
	ctx := context.Background()

	p := writeNfo(t, dir, "badplot.nfo", kodiBadPlot)
	r := s.ScanFile(ctx, p, dir)
	if r.File.Verdict != report.Warn || !r.File.Has(CodeSidecarOK) || !r.File.Has(CodeNfoKodi) || !r.File.Has(CodeLinkInSidecar) {
		t.Fatalf("badplot through ScanFile: %+v", r.File)
	}
	if r.File.Findings[0].Code != CodeSidecarOK {
		t.Fatalf("SIDECAR_OK must come first: %+v", r.File.Findings)
	}

	// A 2 MiB file whose only link sits after the first MiB: only the first
	// MiB is read, so the link is not seen and the file is plain text.
	big := make([]byte, 2<<20)
	for i := range big {
		big[i] = 'x'
		if i%80 == 79 {
			big[i] = '\n'
		}
	}
	copy(big[nfoReadLimit+100:], []byte("\nhttp://tracker.example/late\n"))
	bp := filepath.Join(dir, "big.nfo")
	if err := os.WriteFile(bp, big, 0o644); err != nil {
		t.Fatal(err)
	}
	r = s.ScanFile(ctx, bp, dir)
	if r.File.Verdict != report.Pass || !r.File.Has(CodeNfoText) || r.File.Has(CodeLinkInSidecar) {
		t.Fatalf("2 MiB nfo: %+v", r.File)
	}

	// The same link inside the first MiB is seen.
	copy(big[nfoReadLimit-100:], []byte("\nhttp://tracker.example/early\n"))
	if err := os.WriteFile(bp, big, 0o644); err != nil {
		t.Fatal(err)
	}
	r = s.ScanFile(ctx, bp, dir)
	if r.File.Verdict != report.Warn || !r.File.Has(CodeLinkInSidecar) {
		t.Fatalf("2 MiB nfo with an early link: %+v", r.File)
	}
	for _, f := range findingsOf(&r.File, CodeLinkInSidecar) {
		if strings.Contains(f.Message, "late") {
			t.Fatalf("link past the read limit was read: %q", f.Message)
		}
	}

	// Other allowed text sidecars are not scanned for links in 0.3.
	tp := writeNfo(t, dir, "links.txt", "see https://example.com/a and http://example.org/b\n")
	r = s.ScanFile(ctx, tp, dir)
	if r.File.Verdict != report.Pass || len(r.File.Findings) != 1 || r.File.Findings[0].Code != CodeSidecarOK {
		t.Fatalf("links.txt: %+v", r.File)
	}
}

// TestNfoBlockedOrDangerousIsNotInspected: a blocked .nfo and an HTML .nfo
// are BLOCK before the classifier runs, so hostile content never reaches
// it and no nfo finding is added.
func TestNfoBlockedOrDangerousIsNotInspected(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	p := writeNfo(t, dir, "x.nfo", kodiBadPlot)

	strict := nfoScanner(t, "[sidecars]\nallow = []\nblock = [\"nfo\"]\n")
	r := strict.ScanFile(ctx, p, dir)
	if r.File.Verdict != report.Block || !r.File.Has(CodeSidecarBlocked) || r.File.Has(CodeNfoKodi) || r.File.Has(CodeNfoText) || r.File.Has(CodeLinkInSidecar) {
		t.Fatalf("blocked nfo: %+v", r.File)
	}

	s := nfoScanner(t, "")
	h := writeNfo(t, dir, "h.nfo", "<!DOCTYPE html><html><body><a href=\"http://tracker.example/h\">x</a></body></html>")
	r = s.ScanFile(ctx, h, dir)
	if r.File.Verdict != report.Block || !r.File.Has(CodeDangerous) || r.File.Has(CodeNfoKodi) || r.File.Has(CodeNfoText) || r.File.Has(CodeLinkInSidecar) {
		t.Fatalf("html nfo: %+v", r.File)
	}

	// A PE header under an .nfo name is dangerous content, not an nfo.
	e := writeNfo(t, dir, "e.nfo", "MZ\x90\x00"+strings.Repeat("\x00", 64)+"This program cannot be run in DOS mode")
	r = s.ScanFile(ctx, e, dir)
	if r.File.Verdict != report.Block || !r.File.Has(CodeDangerous) || r.File.Has(CodeNfoText) {
		t.Fatalf("executable nfo: %+v", r.File)
	}

	// A bidi name is blocked before the content is opened.
	b := writeNfo(t, dir, "movie‮nfo.nfo", kodiBadPlot)
	r = s.ScanFile(ctx, b, dir)
	if r.File.Verdict != report.Block || !r.File.Has(CodeBidiName) || r.File.Has(CodeNfoKodi) {
		t.Fatalf("bidi nfo name: %+v", r.File)
	}
}

// TestCheckNfoRefusesSymlinkAndNonRegular: the reader never follows a
// symlink and never opens anything that is not a regular file, even when
// called directly, and ScanFile reports a symlinked .nfo as SYMLINK only.
func TestCheckNfoRefusesSymlinkAndNonRegular(t *testing.T) {
	dir := t.TempDir()
	s := nfoScanner(t, "")
	target := writeNfo(t, dir, "real.nfo", kodiBadPlot)

	// A directory named like an nfo.
	d := filepath.Join(dir, "dir.nfo")
	if err := os.Mkdir(d, 0o755); err != nil {
		t.Fatal(err)
	}
	fr := &report.FileResult{Path: d, Info: map[string]string{}}
	s.checkNfo(fr, d)
	if !fr.Has(CodeUnreadable) || fr.Has(CodeNfoText) || fr.Has(CodeNfoKodi) || fr.Verdict != report.Fail {
		t.Fatalf("directory: %+v", fr.Findings)
	}

	// A missing path.
	fr = &report.FileResult{Path: filepath.Join(dir, "missing.nfo"), Info: map[string]string{}}
	s.checkNfo(fr, fr.Path)
	if !fr.Has(CodeUnreadable) || fr.Has(CodeNfoText) {
		t.Fatalf("missing: %+v", fr.Findings)
	}

	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs a privilege on windows")
	}
	link := filepath.Join(dir, "link.nfo")
	if err := os.Symlink(target, link); err != nil {
		t.Skip("symlinks not supported here:", err)
	}
	fr = &report.FileResult{Path: link, Info: map[string]string{}}
	s.checkNfo(fr, link)
	if !fr.Has(CodeUnreadable) || fr.Has(CodeNfoKodi) || fr.Has(CodeLinkInSidecar) {
		t.Fatalf("symlink through checkNfo: %+v", fr.Findings)
	}
	r := s.ScanFile(context.Background(), link, dir)
	if len(r.File.Findings) != 1 || r.File.Findings[0].Code != CodeSymlink || r.File.Verdict != report.Warn {
		t.Fatalf("symlink through ScanFile: %+v", r.File.Findings)
	}

	// A symlink escaping the tree to a file the scan must never read.
	out := t.TempDir()
	secret := writeNfo(t, out, "secret.nfo", "http://tracker.example/secret")
	esc := filepath.Join(dir, "escape.nfo")
	if err := os.Symlink(secret, esc); err != nil {
		t.Fatal(err)
	}
	fr = &report.FileResult{Path: esc, Info: map[string]string{}}
	s.checkNfo(fr, esc)
	for _, f := range fr.Findings {
		if strings.Contains(f.Message, "secret") {
			t.Fatalf("symlink target was read: %+v", fr.Findings)
		}
	}
	if fr.Has(CodeLinkInSidecar) {
		t.Fatalf("symlink target was classified: %+v", fr.Findings)
	}
}

// TestNfoNeverModified: scanning an nfo, whatever it holds, leaves its
// bytes, mode and modification time alone.
func TestNfoNeverModified(t *testing.T) {
	dir := t.TempDir()
	s := nfoScanner(t, "[metadata]\nlinks = \"fail\"\n")
	for name, content := range map[string]string{
		"bad.nfo":   kodiBadPlot,
		"scene.nfo": sceneLinks,
		"xxe.nfo":   `<!DOCTYPE movie [<!ENTITY x SYSTEM "file:///etc/hostname">]><movie><plot>&x;</plot></movie>`,
	} {
		p := writeNfo(t, dir, name, content)
		if err := os.Chmod(p, 0o640); err != nil {
			t.Fatal(err)
		}
		old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Stat(p)
		s.ScanFile(context.Background(), p, dir)
		after, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(p)
		if string(got) != content || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || after.Mode().Perm() != before.Mode().Perm() {
			t.Fatalf("%s was modified by scan", name)
		}
	}
}

// TestNfoHardLinkIsReadInPlace: a hard link is a regular file and is read
// like any other; the link count is reported by ScanFile, not by checkNfo.
func TestNfoHardLinkIsReadInPlace(t *testing.T) {
	dir := t.TempDir()
	s := nfoScanner(t, "")
	a := writeNfo(t, dir, "a.nfo", sceneLinks)
	b := filepath.Join(dir, "b.nfo")
	if err := os.Link(a, b); err != nil {
		t.Skip("hard links not supported here:", err)
	}
	r := s.ScanFile(context.Background(), b, dir)
	if r.File.Verdict != report.Warn || !r.File.Has(CodeHardlinked) || !r.File.Has(CodeLinkInSidecar) {
		t.Fatalf("hard-linked nfo: %+v", r.File)
	}
}

func TestScraperHost(t *testing.T) {
	yes := map[string]string{
		"https://www.themoviedb.org/movie/11":        "themoviedb.org",
		"themoviedb.org/movie/11":                    "themoviedb.org",
		"www.thetvdb.com/series/x":                   "thetvdb.com",
		"https://api.themoviedb.org/3/movie/11":      "themoviedb.org",
		"https://www.imdb.com/title/tt0120616/":      "imdb.com",
		"imdb.com/title/tt0120616":                   "imdb.com",
		"https://musicbrainz.org/artist/abc":         "musicbrainz.org",
		"HTTPS://WWW.THEMOVIEDB.ORG/MOVIE/11":        "themoviedb.org",
		"https://www.themoviedb.org.:443/movie/11":   "themoviedb.org",
		"https://user:pw@www.themoviedb.org/movie/1": "themoviedb.org",
	}
	for in, want := range yes {
		if got, ok := scraperHost(in); !ok || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, ok, want)
		}
	}
	no := []string{
		"", "http://", "https://themoviedb.org.evil.example/movie/11", "https://evil.example/themoviedb.org",
		"https://themoviedb.org@evil.example/", "https://evil.example/?x=themoviedb.org", "https://evil.example#themoviedb.org",
		"https://www.imdb.com/", "https://www.imdb.com/list/x", "https://www.imdb.com/TITLE/tt1", "http://xthemoviedb.org/",
		"https://themoviedb.orgx/", "tt0120616", "https://themoviedb​.org/movie/11", "https://evil.example\\@themoviedb.org/",
		"http://[::1]/themoviedb.org", "https://themoviedb.org.evil.example:443/", "https://theмoviedb.org/movie/11",
	}
	for _, in := range no {
		if got, ok := scraperHost(in); ok {
			t.Errorf("%q: accepted as %q", in, got)
		}
	}
}
