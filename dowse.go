package main

// Dowse is called through its public API with an API key, exactly like the
// other providers, so its times and results are what a customer gets.

import (
	"context"
	"net/http"
	"strings"

	"dowse-compare/internal/models"
)

// priceDowseCredit is one credit at the entry pack: $10 for 13,000 credits.
const priceDowseCredit = 10.0 / 13000

// compareDowseSearch runs POST /v1/search at depth (basic or advanced). The
// cost is the credits the API charged at the entry pack price.
func compareDowseSearch(ctx context.Context, hc *http.Client, base, key, depth, q string, n int) ([]models.SearchResult, float64, error) {
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
		Usage struct {
			Credits float64 `json:"credits"`
		} `json:"usage"`
	}
	body := map[string]any{"query": q, "max_results": n, "search_depth": depth}
	if err := comparePost(ctx, hc, base+"/v1/search", map[string]string{"Authorization": "Bearer " + key}, body, &out); err != nil {
		return nil, 0, err
	}
	rs := make([]models.SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		rs = append(rs, models.SearchResult{Title: r.Title, URL: r.URL, Content: r.Content})
	}
	return rs, out.Usage.Credits * priceDowseCredit, nil
}

// extractDowse runs POST /v1/extract for one page (markdown, whole page: the defaults).
func extractDowse(ctx context.Context, hc *http.Client, base, key, u string) (compareExtractOutcome, error) {
	var out struct {
		Results []struct {
			Content       string `json:"content"`
			ContentStatus string `json:"content_status"`
		} `json:"results"`
		FailedResults []struct {
			Error string `json:"error"`
		} `json:"failed_results"`
		Usage struct {
			Credits float64 `json:"credits"`
		} `json:"usage"`
	}
	body := map[string]any{"urls": []string{u}}
	if err := comparePost(ctx, hc, base+"/v1/extract", map[string]string{"Authorization": "Bearer " + key}, body, &out); err != nil {
		return compareExtractOutcome{}, err
	}
	if len(out.Results) > 0 && strings.TrimSpace(out.Results[0].Content) != "" {
		return compareExtractOutcome{Text: out.Results[0].Content, CostUSD: out.Usage.Credits * priceDowseCredit}, nil
	}
	o := compareExtractOutcome{Reason: "could not read the page"}
	if len(out.FailedResults) > 0 {
		o.Reason = failureReason(out.FailedResults[0].Error, 0)
	}
	return o, nil
}
