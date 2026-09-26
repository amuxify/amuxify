package scan

import (
	"regexp"
	"strings"
)

// linkRe finds URLs and bare domain names in free text. It is deliberately
// broad: a false positive costs a WARN line, a false negative costs a
// tracking link in someone's library.
var linkRe = regexp.MustCompile(`(?i)(?:https?://|ftp://|www\.)[^\s"'<>]+|\b[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*\.(?:com|net|org|info|biz|io|to|cc|tv|me|ru|xyz|site|online|club|top|link|sh|app|dev|co|us|uk|de|fr|nl|se|eu|pw|ws|su|in|cn|tk|ml|ga|cf|gq|stream|live|watch|movie|video|download|torrent|zone|space)\b`)

// FindLinks returns the distinct link-like tokens in text, capped.
func FindLinks(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range linkRe.FindAllString(text, -1) {
		m = strings.TrimRight(m, ".,;:)]}")
		if isFalsePositive(m) {
			continue
		}
		l := strings.ToLower(m)
		if seen[l] {
			continue
		}
		seen[l] = true
		out = append(out, m)
		if len(out) >= 20 {
			break
		}
	}
	return out
}

// isFalsePositive drops tokens that are clearly not links: pure version
// numbers and the odd codec name.
func isFalsePositive(s string) bool {
	l := strings.ToLower(s)
	switch {
	case strings.HasPrefix(l, "http"), strings.HasPrefix(l, "www."), strings.HasPrefix(l, "ftp"):
		return false
	}
	// "5.1", "7.1" style tokens never survive the TLD check, but "x264.us"
	// style false hits are rare enough to accept as WARN.
	return false
}
