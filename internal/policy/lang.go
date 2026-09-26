package policy

import "strings"

// aliases maps every known spelling of a language code to a canonical
// ISO 639-2/B code. Both 639-1 and 639-2/T forms are included so a user can
// write "en", "eng", "de", "ger" or "deu" and mean the same thing.
var aliases = map[string]string{}

func init() {
	groups := [][]string{
		{"eng", "en"}, {"jpn", "ja"}, {"ger", "deu", "de"}, {"fre", "fra", "fr"}, {"spa", "es"},
		{"ita", "it"}, {"por", "pt"}, {"rus", "ru"}, {"chi", "zho", "zh"}, {"kor", "ko"},
		{"dut", "nld", "nl"}, {"swe", "sv"}, {"nor", "no", "nob", "nno"}, {"dan", "da"}, {"fin", "fi"},
		{"pol", "pl"}, {"cze", "ces", "cs"}, {"hun", "hu"}, {"tur", "tr"}, {"ara", "ar"},
		{"heb", "he", "iw"}, {"hin", "hi"}, {"tha", "th"}, {"vie", "vi"}, {"ind", "id", "in"},
		{"gre", "ell", "el"}, {"rum", "ron", "ro"}, {"ukr", "uk"}, {"bul", "bg"}, {"hrv", "hr"},
		{"srp", "sr"}, {"slv", "sl"}, {"slo", "slk", "sk"}, {"tam", "ta"}, {"tel", "te"},
		{"ben", "bn"}, {"may", "msa", "ms"}, {"per", "fas", "fa"}, {"cat", "ca"}, {"baq", "eus", "eu"},
		{"glg", "gl"}, {"ice", "isl", "is"}, {"lit", "lt"}, {"lav", "lv"}, {"est", "et"},
		{"tgl", "fil", "tl"}, {"mal", "ml"}, {"kan", "kn"}, {"mar", "mr"}, {"urd", "ur"},
		{"pan", "pa"}, {"guj", "gu"}, {"lat", "la"}, {"afr", "af"}, {"alb", "sqi", "sq"},
		{"arm", "hye", "hy"}, {"aze", "az"}, {"bos", "bs"}, {"bur", "mya", "my"}, {"geo", "kat", "ka"},
		{"kaz", "kk"}, {"khm", "km"}, {"mac", "mkd", "mk"}, {"mon", "mn"}, {"nep", "ne"},
		{"sin", "si"}, {"swa", "sw"}, {"uzb", "uz"}, {"wel", "cym", "cy"}, {"gle", "ga"},
		{"mao", "mri", "mi"}, {"tib", "bod", "bo"}, {"yid", "yi"}, {"zul", "zu"}, {"und"},
		{"mul"}, {"zxx"}, {"mis"},
	}
	for _, g := range groups {
		for _, a := range g {
			aliases[a] = g[0]
		}
	}
}

// Canonical returns the ISO 639-2/B form of a code, or the lower-cased input
// when unknown. IETF tags like "pt-BR" reduce to their primary subtag.
func Canonical(code string) string {
	c := strings.ToLower(strings.TrimSpace(code))
	if c == "" {
		return "und"
	}
	if i := strings.IndexAny(c, "-_"); i > 0 {
		c = c[:i]
	}
	if a, ok := aliases[c]; ok {
		return a
	}
	return c
}

// IsUnd reports whether a code means "undetermined" or is missing.
func IsUnd(code string) bool {
	c := Canonical(code)
	return c == "und" || c == "" || c == "unknown"
}

// LanguageMatches reports whether a stream language (ISO code) or its IETF
// tag is in the keep list. "*" matches everything, including und.
func (p *Profile) LanguageMatches(lang, ietf string) bool {
	if p.KeepsAllLanguages() {
		return true
	}
	c := Canonical(lang)
	ci := ""
	if ietf != "" {
		ci = Canonical(ietf)
	}
	for _, k := range p.Languages.Keep {
		kc := Canonical(k)
		if kc == c || (ci != "" && kc == ci) {
			return true
		}
	}
	return false
}
