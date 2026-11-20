// Package fusion has the URL keys the comparison app uses to tell copies of one page apart.
package fusion

import (
	"net/url"
	"regexp"
	"strings"
)

// NormalizeURL lowercases scheme/host, strips tracking params, trailing slash and fragment.
func NormalizeURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)

	q := u.Query()
	for param := range q {
		p := strings.ToLower(param)
		if strings.HasPrefix(p, "utm_") || p == "ref" || p == "fbclid" || p == "gclid" {
			q.Del(param)
		}
	}
	u.RawQuery = q.Encode()
	u.Path = strings.TrimRight(u.Path, "/")
	u.Fragment = ""
	return u.String()
}

// SamePageKey is NormalizeURL with the differences that never make a different
// page folded away: http and https, a leading "www.", and a leading language or
// country part of the path (/en-us/, /de/, /pt_BR/). Search uses it to drop
// copies of one page before ranking: theguardian.com and www.theguardian.com,
// or support.apple.com/en-us/100100 and /en-al/100100.
func SamePageKey(raw string) string {
	n := NormalizeURL(raw)
	u, err := url.Parse(n)
	if err != nil || u.Host == "" {
		return n
	}
	host := strings.TrimPrefix(u.Host, "www.")
	path := u.EscapedPath()
	if rest, ok := cutLocaleSegment(path); ok {
		path = rest
	}
	key := host + path
	if u.RawQuery != "" {
		key += "?" + u.RawQuery
	}
	return key
}

// localeSegment matches a language, or a language and region: en, de, en-us,
// pt_br, zh-hans. Two letters alone count only for common languages (below),
// so /go/ or /js/ sections of a docs site are left alone.
var localeSegment = regexp.MustCompile(`^[a-z]{2}[-_]([a-z]{2}|[a-z]{4}|[0-9]{3})$`)

var localeLanguages = map[string]bool{
	"en": true, "de": true, "fr": true, "es": true, "it": true, "pt": true, "nl": true, "sv": true, "da": true,
	"fi": true, "no": true, "nb": true, "pl": true, "cs": true, "sk": true, "hu": true, "ro": true, "bg": true,
	"el": true, "tr": true, "ru": true, "uk": true, "ar": true, "he": true, "fa": true, "hi": true, "th": true,
	"vi": true, "id": true, "ms": true, "ja": true, "ko": true, "zh": true, "hr": true, "sl": true, "sr": true,
	"lt": true, "lv": true, "et": true,
}

// cutLocaleSegment drops a leading locale segment from path, returning the
// rest (which keeps its leading slash) and whether one was dropped.
func cutLocaleSegment(path string) (string, bool) {
	trimmed := strings.TrimPrefix(path, "/")
	seg, rest, found := strings.Cut(trimmed, "/")
	s := strings.ToLower(seg)
	if !(localeSegment.MatchString(s) || localeLanguages[s]) {
		return path, false
	}
	if !found {
		return "", true
	}
	return "/" + rest, true
}
