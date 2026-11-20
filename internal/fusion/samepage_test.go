package fusion

import "testing"

func TestSamePageKey(t *testing.T) {
	same := [][2]string{
		{"https://theguardian.com/science/2026/nov/14/x", "https://www.theguardian.com/science/2026/nov/14/x"},
		{"http://example.com/a", "https://example.com/a/"},
		{"https://support.apple.com/en-us/100100", "https://support.apple.com/en-al/100100"},
		{"https://support.apple.com/en-us/100100", "https://support.apple.com/en-vn/100100"},
		{"https://docs.example.com/de/guide", "https://docs.example.com/fr/guide"},
		{"https://example.com/pt_BR/page", "https://example.com/en-GB/page"},
		{"https://example.com/zh-hans/page", "https://example.com/page"},
		{"https://example.com/a?utm_source=x", "https://www.example.com/a"},
	}
	for _, p := range same {
		if SamePageKey(p[0]) != SamePageKey(p[1]) {
			t.Errorf("want same page: %s and %s (%q vs %q)", p[0], p[1], SamePageKey(p[0]), SamePageKey(p[1]))
		}
	}
	different := [][2]string{
		{"https://docs.example.com/go/install", "https://docs.example.com/js/install"},
		{"https://example.com/a", "https://example.com/b"},
		{"https://blog.example.com/a", "https://example.com/a"},
		{"https://example.com/a?id=1", "https://example.com/a?id=2"},
		{"https://example.com/news/en-us", "https://example.com/news/de-de"}, // only a leading segment folds
		{"https://example.com/us/page", "https://example.com/page"},          // "us" is not a language
	}
	for _, p := range different {
		if SamePageKey(p[0]) == SamePageKey(p[1]) {
			t.Errorf("want different pages: %s and %s (%q)", p[0], p[1], SamePageKey(p[0]))
		}
	}
	if SamePageKey("") != "" {
		t.Error("empty URL should give an empty key")
	}
}
