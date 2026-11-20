// Package judge asks Jev (TypeSafe's System One model on OpenRouter) to grade
// search results and page texts. It is the comparison page's own judge: its
// prompts are written for judging and published here, so anyone can repeat a
// grading with their own OpenRouter key.
package judge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// Endpoint is OpenRouter's System One API.
const Endpoint = "https://openrouter.ai/api/v1/systemone"

// DefaultModel is the judge model.
const DefaultModel = "~typesafe/jev-latest"

// The judge's prompts. They are written for this page and are public.
const (
	relevanceInstructions = "Grade how well this result serves someone who ran the search query: does it answer or directly fulfil it, " +
		"is the source specific and trustworthy for this topic, and is it current when the query asks about something recent? " +
		"Judge the page the result points to, using its title, address and snippet."
	spamInstructions = "Is this result spam, an SEO content farm, placeholder text, or clickbait with little real substance?"
)

// relevanceLevels are the 0-3 grades the relevance question chooses from.
var relevanceLevels = []string{"irrelevant", "slightly_relevant", "relevant", "highly_relevant"}

// Question is one System One question.
type Question struct {
	Type         string   `json:"type"` // "score" or "noul" (a yes/no probability)
	Instructions string   `json:"instructions"`
	Criteria     []string `json:"criteria,omitempty"`
}

// Request is the body sent to Endpoint.
type Request struct {
	Model     string              `json:"model"`
	State     string              `json:"state"`
	Questions map[string]Question `json:"questions"`
}

// Result is one search result to grade.
type Result struct {
	Title, URL, Snippet string
}

// Grade is the judge's answer for one result.
type Grade struct {
	Relevance  float64 // 0-3
	Confidence float64 // 0-1
	Spam       float64 // probability it is spam or clickbait, 0-1
	Score      float64 // 0-1, see Score
	OK         bool    // false when the judge gave no answer for it
}

// Score turns the answers into one 0-1 grade: relevance, a little less when
// the judge is unsure, and much less when it is likely spam.
func Score(relevance, confidence, spam float64) float64 {
	r := math.Max(0, math.Min(1, relevance/3)) * (0.7 + 0.3*confidence)
	return math.Round(r*math.Max(0.1, 1-0.75*spam)*10000) / 10000
}

// Client calls the judge.
type Client struct {
	key, model string
	hc         *http.Client
}

// New returns a judge client; an empty model means DefaultModel.
func New(key, model string) *Client {
	if model == "" {
		model = DefaultModel
	}
	return &Client{key: key, model: model, hc: &http.Client{Timeout: 40 * time.Second}}
}

// Endpoint is where the client sends requests.
func (c *Client) Endpoint() string { return Endpoint }

// Request builds the exact request that grades results for query: today's
// date, the query, every result by number, and per result a relevance
// question and a spam question.
func (c *Client) Request(today, query string, results []Result) Request {
	var b strings.Builder
	if today != "" {
		fmt.Fprintf(&b, "Today's date: %s.\n\n", today)
	}
	fmt.Fprintf(&b, "Search query: %q\n\nGrade each result below on its own, by its number.\n\n", query)
	qs := make(map[string]Question, 2*len(results))
	for i, r := range results {
		n := i + 1
		snippet := []rune(r.Snippet)
		if len(snippet) > 600 {
			snippet = snippet[:600]
		}
		fmt.Fprintf(&b, "[%d] Title: %q\n    URL: %q\n    Snippet: %q\n\n", n, r.Title, r.URL, string(snippet))
		qs[fmt.Sprintf("relevance_%d", n)] = Question{Type: "score", Instructions: fmt.Sprintf("Result [%d]: %s", n, relevanceInstructions), Criteria: relevanceLevels}
		qs[fmt.Sprintf("spam_%d", n)] = Question{Type: "noul", Instructions: fmt.Sprintf("Result [%d]: %s", n, spamInstructions)}
	}
	return Request{Model: c.model, State: b.String(), Questions: qs}
}

// GradeResults sends Request(today, query, results) and returns one grade per
// result (in order) and the call's cost.
func (c *Client) GradeResults(ctx context.Context, today, query string, results []Result) ([]Grade, float64, error) {
	answers, cost, err := c.Ask(ctx, c.Request(today, query, results))
	if err != nil {
		return nil, cost, err
	}
	grades := make([]Grade, len(results))
	got := 0
	for i := range results {
		n := i + 1
		var sc struct {
			Score      float64 `json:"score"`
			Confidence float64 `json:"confidence"`
		}
		raw, ok := answers[fmt.Sprintf("relevance_%d", n)]
		if !ok || json.Unmarshal(raw, &sc) != nil {
			continue
		}
		var sp struct {
			Noul float64 `json:"noul"`
		}
		if raw, ok := answers[fmt.Sprintf("spam_%d", n)]; ok {
			_ = json.Unmarshal(raw, &sp)
		}
		g := Grade{Relevance: sc.Score, Confidence: sc.Confidence, Spam: sp.Noul, OK: true}
		g.Score = Score(g.Relevance, g.Confidence, g.Spam)
		grades[i] = g
		got++
	}
	if got == 0 {
		return nil, cost, fmt.Errorf("the judge answered none of %d results", len(results))
	}
	return grades, cost, nil
}

// Ask sends one request (any state and questions) and returns the raw answers
// by question name and the call's cost in USD.
func (c *Client) Ask(ctx context.Context, req Request) (map[string]json.RawMessage, float64, error) {
	if req.Model == "" {
		req.Model = c.model
	}
	body, _ := json.Marshal(req)
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, Endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	hr.Header.Set("Authorization", "Bearer "+c.key)
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(hr)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, 0, fmt.Errorf("judge: status %d", resp.StatusCode)
	}
	var out struct {
		Answers map[string]json.RawMessage `json:"answers"`
		Usage   struct {
			Cost float64 `json:"cost"`
		} `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, 0, fmt.Errorf("judge: bad answer: %w", err)
	}
	if out.Error != nil {
		return nil, out.Usage.Cost, fmt.Errorf("judge: %s", out.Error.Message)
	}
	return out.Answers, out.Usage.Cost, nil
}
