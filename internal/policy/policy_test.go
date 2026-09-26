package policy

import (
	"strings"
	"testing"

	"github.com/amuxify/amuxify/internal/probe"
)

func mustLoad(t *testing.T, name string) *Profile {
	t.Helper()
	p, err := Load(name)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return p
}

func TestBuiltinsParse(t *testing.T) {
	for _, n := range Names() {
		mustLoad(t, n)
	}
	if len(Names()) != 4 {
		t.Fatalf("expected 4 built-ins, got %v", Names())
	}
}

func TestParseOverridesOnlyStatedKeys(t *testing.T) {
	p, err := Parse([]byte("[languages]\nkeep=[\"jpn\",\"en\"]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Chapters.Keep || p.Attachments.Fonts != "keep_if_text_subs" {
		t.Fatalf("defaults not inherited: %+v", p)
	}
	if !p.LanguageMatches("eng", "") || !p.LanguageMatches("ja", "") || p.LanguageMatches("ger", "") {
		t.Fatal("language matching wrong")
	}
	if _, err := Parse([]byte("[bogus]\nx=1\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := Parse([]byte("[verify]\ntier=\"sometimes\"\n")); err == nil {
		t.Fatal("bad enum accepted")
	}
}

func TestCanonical(t *testing.T) {
	cases := map[string]string{"en": "eng", "ENG": "eng", "deu": "ger", "pt-BR": "por", "": "und", "xx": "xx", "zh-Hans": "chi"}
	for in, want := range cases {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q)=%q want %q", in, got, want)
		}
	}
}

func sample() *probe.MediaInfo {
	return &probe.MediaInfo{Container: "matroska",
		Streams: []probe.Stream{
			{Index: 0, Type: "video", Codec: "hevc"},
			{Index: 1, Type: "audio", Language: "jpn", Default: true},
			{Index: 2, Type: "audio", Language: "eng"},
			{Index: 3, Type: "audio", Language: "eng", Title: "Director's Commentary"},
			{Index: 4, Type: "audio", Language: "und"},
			{Index: 5, Type: "subtitle", Language: "eng", TextSubtitle: true},
			{Index: 6, Type: "subtitle", Language: "eng", Forced: true, TextSubtitle: true},
			{Index: 7, Type: "subtitle", Language: "ger", TextSubtitle: true},
			{Index: 8, Type: "subtitle", Language: "eng", HearingImp: true, TextSubtitle: true},
		},
		Attachments: []probe.Attachment{
			{ID: 1, FileName: "font.ttf", MimeType: "application/x-truetype-font"},
			{ID: 2, FileName: "cover.jpg", MimeType: "image/jpeg"},
			{ID: 3, FileName: "readme.exe", MimeType: "application/octet-stream"},
		},
	}
}

func keptIdx(d *Decision, typ string) []int {
	var out []int
	for _, a := range d.KeptOf(typ) {
		out = append(out, a.Stream.Index)
	}
	return out
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestHomelabKeepsEverythingButNeverPrompts(t *testing.T) {
	d := mustLoad(t, "homelab").Decide(sample(), Options{})
	if !eq(keptIdx(d, "audio"), []int{1, 2, 3, 4}) {
		t.Fatalf("audio kept %v", keptIdx(d, "audio"))
	}
	if !eq(keptIdx(d, "subtitle"), []int{5, 6, 7, 8}) {
		t.Fatalf("subs kept %v", keptIdx(d, "subtitle"))
	}
	if !d.KeepChapters || !d.Attachments[0].Keep || !d.Attachments[1].Keep || !d.Attachments[2].Block {
		t.Fatalf("attachments/chapters wrong: %+v", d.Attachments)
	}
	// Existing default (jpn) is preserved when no original language is known.
	for _, a := range d.KeptOf("audio") {
		if a.Default != (a.Stream.Index == 1) {
			t.Fatalf("default flag wrong on %d", a.Stream.Index)
		}
	}
}

func TestHomelabPrefersOriginalLanguageFromHook(t *testing.T) {
	d := mustLoad(t, "homelab").Decide(sample(), Options{OriginalLanguage: "English"})
	// "English" is not a code; Canonical leaves it alone so no match, jpn stays.
	if d.KeptOf("audio")[0].Default != true {
		t.Fatal("expected jpn default retained when original language unknown code")
	}
	d = mustLoad(t, "homelab").Decide(sample(), Options{OriginalLanguage: "en"})
	for _, a := range d.KeptOf("audio") {
		if a.Default != (a.Stream.Index == 2) {
			t.Fatalf("expected eng #2 default, got default on %d=%v", a.Stream.Index, a.Default)
		}
	}
}

func TestArchiveMatchesLegacyPolicy(t *testing.T) {
	d := mustLoad(t, "archive").Decide(sample(), Options{})
	if !eq(keptIdx(d, "audio"), []int{2}) {
		t.Fatalf("archive audio kept %v, want [2]", keptIdx(d, "audio"))
	}
	if !eq(keptIdx(d, "subtitle"), []int{5, 6, 8}) {
		t.Fatalf("archive subs kept %v", keptIdx(d, "subtitle"))
	}
	if d.KeepChapters || d.Attachments[0].Keep || d.Attachments[1].Keep {
		t.Fatal("archive must drop chapters and attachments")
	}
	if !d.StripTitle || !d.StripTags || !d.Tracks[2].ClearTitle {
		t.Fatal("archive must strip titles")
	}
}

func TestAudioFallbackNeverSilent(t *testing.T) {
	m := sample()
	m.Streams = m.Streams[:2] // video + jpn audio only
	d := mustLoad(t, "archive").Decide(m, Options{})
	if len(d.KeptOf("audio")) != 1 {
		t.Fatal("expected fallback to keep the only audio")
	}
	found := false
	for _, f := range d.Findings {
		if f.Code == CodeAudioFallback {
			found = true
		}
	}
	if !found {
		t.Fatal("expected AUDIO_FALLBACK finding")
	}
}

func TestUndAssume(t *testing.T) {
	p, err := Parse([]byte("[languages]\nkeep=[\"eng\"]\nund=\"assume:en\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := p.Decide(sample(), Options{})
	var und TrackAction
	for _, a := range d.Tracks {
		if a.Stream.Index == 4 {
			und = a
		}
	}
	if !und.Keep || und.SetLanguage != "eng" {
		t.Fatalf("und track: %+v", und)
	}
}

func TestCommentaryRegexWordBounded(t *testing.T) {
	if commentaryRe.MatchString("Director's Cut") || commentaryRe.MatchString("Cast") {
		t.Fatal("false positive")
	}
	if !commentaryRe.MatchString("Audio Commentary with the director") || !commentaryRe.MatchString("commentaries") {
		t.Fatal("false negative")
	}
}

func TestSDHDuplicates(t *testing.T) {
	p, err := Parse([]byte("[subtitles]\ndrop_sdh_duplicates=true\n"))
	if err != nil {
		t.Fatal(err)
	}
	d := p.Decide(sample(), Options{})
	if eq(keptIdx(d, "subtitle"), []int{5, 6, 7, 8}) {
		t.Fatal("SDH duplicate should be dropped")
	}
	if !eq(keptIdx(d, "subtitle"), []int{5, 6, 7}) {
		t.Fatalf("got %v", keptIdx(d, "subtitle"))
	}
}

// Hook language: every spelling a Sonarr or Radarr hook may hand over maps to
// the 639-2/B form the aliases table defines.
func TestCanonicalAcceptsThreeLetterCodes(t *testing.T) {
	cases := map[string]string{
		"eng":   "eng",
		"jpn":   "jpn",
		"ger":   "ger",
		"deu":   "ger",
		"en-US": "eng",
		"en":    "eng",
		"EN":    "eng",
		"ENG":   "eng",
		"pt-BR": "por",
		"pt_BR": "por",
		"zh":    "chi",
		"zho":   "chi",
		"ja":    "jpn",
		"de":    "ger",
		" en ":  "eng",
		"":      "und",
		"und":   "und",
	}
	for in, want := range cases {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q want %q", in, got, want)
		}
	}
}

// Hostile language strings from a hook never panic and never widen to a
// language the string does not spell. A value that is not a known code comes
// back lower-cased and unmatched, so it selects nothing.
func TestCanonicalRejectsHostileInput(t *testing.T) {
	cases := map[string]string{
		"-eng":             "-eng",
		"_eng":             "_eng",
		"eng;rm -rf /":     "eng;rm ", // cut at the first separator, still no match
		"$(id)":            "$(id)",
		"`id`":             "`id`",
		"eng\x00":          "eng\x00",
		"\u202eeng":        "\u202eeng",
		"e":                "e",
		"--":               "--",
		"../../etc/passwd": "../../etc/passwd",
		"eng\n":            "eng", // surrounding whitespace is the one thing trimmed
		"en-\x00":          "eng", // primary subtag "en" before the separator
	}
	for in, want := range cases {
		if got := Canonical(in); got != want {
			t.Errorf("Canonical(%q) = %q want %q", in, got, want)
		}
	}
	long := strings.Repeat("a", 100000)
	if got := Canonical(long); got != long {
		t.Errorf("long input altered")
	}
	// None of the unmatched forms may satisfy a keep list of eng.
	p := mustLoad(t, "archive")
	for _, in := range []string{"-eng", "_eng", "eng;rm -rf /", "eng\x00", "\u202eeng", "e"} {
		if p.LanguageMatches(in, "") {
			t.Errorf("%q matched the archive keep list", in)
		}
	}
}

// Exactly one kept audio track carries the default flag, whatever the input
// flags say and whatever the hook says the original language is.
func TestDecideAudioDefaultIsUnique(t *testing.T) {
	defaults := func(d *Decision) []int {
		var out []int
		for _, a := range d.KeptOf("audio") {
			if a.Default {
				out = append(out, a.Stream.Index)
			}
		}
		return out
	}
	for _, name := range Names() {
		p := mustLoad(t, name)
		for _, orig := range []string{"", "en", "eng", "jpn", "ger", "xx", "$(id)", "‮"} {
			// Every audio track flagged default.
			m := sample()
			for i := range m.Streams {
				if m.Streams[i].Type == "audio" {
					m.Streams[i].Default = true
				}
			}
			if got := defaults(p.Decide(m, Options{OriginalLanguage: orig})); len(got) != 1 {
				t.Errorf("%s orig=%q all-default input: defaults on %v", name, orig, got)
			}
			// No audio track flagged default.
			m = sample()
			for i := range m.Streams {
				m.Streams[i].Default = false
			}
			if got := defaults(p.Decide(m, Options{OriginalLanguage: orig})); len(got) != 1 {
				t.Errorf("%s orig=%q no-default input: defaults on %v", name, orig, got)
			}
			// Two eng tracks where only the commentary is flagged default.
			m = sample()
			for i := range m.Streams {
				m.Streams[i].Default = m.Streams[i].Index == 3
			}
			d := p.Decide(m, Options{OriginalLanguage: orig})
			if got := defaults(d); len(got) != 1 {
				t.Errorf("%s orig=%q commentary-default input: defaults on %v", name, orig, got)
			}
		}
	}
	// The hook's original language wins when a kept track carries it.
	d := mustLoad(t, "homelab").Decide(sample(), Options{OriginalLanguage: "deu"})
	if got := defaults(d); len(got) != 1 || got[0] != 1 {
		// No German audio in the sample, so the existing jpn default stays.
		t.Errorf("no matching audio: defaults on %v, want [1]", got)
	}
	d = mustLoad(t, "homelab").Decide(sample(), Options{OriginalLanguage: "en-US"})
	if got := defaults(d); len(got) != 1 || got[0] != 2 {
		t.Errorf("en-US: defaults on %v, want [2]", got)
	}
}

// A profile overlay is data: unknown keys, wrong types and hostile values are
// rejected rather than silently accepted.
func TestParseRejectsHostileOverlays(t *testing.T) {
	bad := []string{
		"[bogus]\nx=1\n",
		"[languages]\nkeep=1\n",
		"[safety]\nhardlinks=\"$(rm -rf /)\"\n",
		"[safety]\nexec_permissions=\"ignore-me\"\n",
		"[verify]\ntier=\"nonexistent\"\n",
		"[safety]\nclamav=\"yes please\"\n",
		"not toml at all = = =\n",
		"[languages]\nkeep=[\"eng\"\n",
	}
	for _, raw := range bad {
		if _, err := Parse([]byte(raw)); err == nil {
			t.Errorf("overlay accepted: %q", raw)
		}
	}
	// Large but valid input is fine and does not hang.
	big := "[languages]\nkeep=[" + strings.Repeat("\"eng\",", 5000) + "\"jpn\"]\n"
	if _, err := Parse([]byte(big)); err != nil {
		t.Errorf("large overlay rejected: %v", err)
	}
}

// Loading a profile by path never follows the name into the built-ins and a
// missing path is an error, not a silent fallback to homelab.
func TestLoadMissingPathIsAnError(t *testing.T) {
	for _, name := range []string{"/nonexistent/profile.toml", "./no-such-profile.toml", "../../../etc/passwd", "homelab.toml.bak"} {
		if _, err := Load(name); err == nil {
			t.Errorf("Load(%q) succeeded", name)
		}
	}
}
