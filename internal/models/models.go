// Package models holds the result type the providers return.
package models

// SearchResult is one search result as a provider returned it.
type SearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"` // the provider's snippet
}
