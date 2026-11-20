package judge

import (
	"strings"
	"testing"
)

func TestRequestNumbersEveryResult(t *testing.T) {
	req := New("", "").Request("2026-11-20", "best widget", []Result{
		{Title: "A", URL: "https://a.example/", Snippet: strings.Repeat("x", 700)},
		{Title: "B", URL: "https://b.example/"},
	})
	if req.Model != DefaultModel || len(req.Questions) != 4 {
		t.Fatalf("model %q, %d questions", req.Model, len(req.Questions))
	}
	for _, want := range []string{"Today's date: 2026-11-20.", `Search query: "best widget"`, "[1] Title: \"A\"", "[2] Title: \"B\""} {
		if !strings.Contains(req.State, want) {
			t.Fatalf("state is missing %q", want)
		}
	}
	if strings.Contains(req.State, strings.Repeat("x", 601)) {
		t.Fatal("snippets are cut at 600 characters")
	}
	if q := req.Questions["relevance_2"]; q.Type != "score" || len(q.Criteria) != 4 || !strings.HasPrefix(q.Instructions, "Result [2]") {
		t.Fatalf("relevance question: %+v", q)
	}
}

func TestScore(t *testing.T) {
	cases := []struct{ rel, conf, spam, want float64 }{
		{3, 1, 0, 1},
		{3, 0, 0, 0.7},
		{0, 1, 0, 0},
		{3, 1, 1, 0.25},
	}
	for _, c := range cases {
		if got := Score(c.rel, c.conf, c.spam); got != c.want {
			t.Errorf("Score(%v, %v, %v) = %v, want %v", c.rel, c.conf, c.spam, got, c.want)
		}
	}
}
