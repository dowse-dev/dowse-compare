package main

// The comparison page (compare.dowse.dev): one query goes to Dowse and to the
// other search APIs at once, each column streams in as its provider answers,
// and then Jev scores every result blind (provider names hidden, one shuffled
// pool of unique URLs) and each provider gets an nDCG@10 score from those grades.
//
// Every fresh comparison spends one call from each provider's free tier, so
// answers are cached by query, fresh ones are limited per IP and per day, and
// the Turnstile session gate applies when it is on.

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"dowse-compare/internal/access"
	"dowse-compare/internal/fusion"
	"dowse-compare/internal/judge"
	"dowse-compare/internal/models"
)

const (
	compareResults     = 10               // results asked from each provider
	compareMaxQuery    = 300              // characters
	compareProviderMax = 20 * time.Second // per provider call
	compareAdvancedMax = 40 * time.Second // Dowse advanced plans extra searches first
	compareJudgeMax    = 30 * time.Second
	compareCacheTTL    = 24 * time.Hour
	compareHistoryMax  = 100 // runs in the public list
	compareMaxRunning  = 4   // fresh comparisons at once, for everyone together
)

// compareProvider is one column of the table.
type compareProvider struct {
	ID   string
	Name string
	Mode string // the setting used, shown under the name
	// Timeout overrides compareProviderMax for a slower mode (0 = the default).
	Timeout time.Duration
	// Search returns the results and what the call costs at list price (USD).
	Search compareSearch
}

// compareSearch runs one query: results, list-price cost in USD, error.
type compareSearch func(ctx context.Context, query string, n int) ([]models.SearchResult, float64, error)

// List prices per query (checked 2026-11-19 on each pricing page), at the
// entry paid tier, for the settings the page uses (10 results):
//   - Dowse basic: 2 credits; the $10 pack has 13,000 credits.
//   - Exa auto + highlights: Exa reports the exact cost in costDollars
//     ($0.007 on 2026-11-19, highlights included); $7/1k is the fallback.
//   - Tavily basic: 1 credit at $0.008 pay as you go.
//   - Perplexity Search API (standard, not Fast): $5/1k requests.
//   - Context.dev web search: 1 credit per 10 results; $20/month for 10,000
//     credits on the Developer plan. The response says the credits used.
const (
	priceDowseBasic       = 2 * 10.0 / 13000
	priceDowseAdvanced    = 4 * 10.0 / 13000
	priceExaFallback      = 0.007
	priceTavilyBasic      = 0.008
	pricePerplexity       = 0.005
	priceContextDevCredit = 20.0 / 10000
)

// compareScorer grades search results (judge.Client; faked in tests). Request
// is the exact request GradeResults sends, kept so anyone can repeat it.
type compareScorer interface {
	GradeResults(ctx context.Context, today, query string, results []judge.Result) ([]judge.Grade, float64, error)
	Request(today, query string, results []judge.Result) judge.Request
	Endpoint() string
}

// compareResult is one row cell as the page shows it.
type compareResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
}

// compareColumn is one provider's answer. Status is ok, error, timeout or
// unavailable (no key). Errors are never passed through: they can carry
// account details.
type compareColumn struct {
	ID      string          `json:"id"`
	Status  string          `json:"status"`
	Ms      int64           `json:"ms"`
	CostUSD float64         `json:"cost_usd,omitempty"` // this query at list price
	Results []compareResult `json:"results"`
}

// compareScore is one provider's result: its score and Jev's grade (0-1) per result.
type compareScore struct {
	Score  *float64  `json:"score"`  // nDCG@10 x 100; nil = no results to score
	Useful int       `json:"useful"` // results Jev rated relevant (raw >= 2 of 3)
	Grades []float64 `json:"grades"` // per result, in the provider's order
}

type compareVerdict struct {
	Scores map[string]compareScore `json:"scores"`
	Winner string                  `json:"winner,omitempty"`
	Pool   int                     `json:"pool"` // unique results judged
}

// compareRun is a finished comparison. ID and At are set by the store.
type compareRun struct {
	ID      int64           `json:"id,omitempty"`
	At      time.Time       `json:"at,omitempty"`
	Query   string          `json:"query"`
	Columns []compareColumn `json:"columns"`
	Verdict *compareVerdict `json:"verdict"`
	// Judge is what the judge was asked and answered, for the downloadable
	// copy of the run (left out of the page's own answers: it is large).
	Judge *compareJudgeRecord `json:"judge,omitempty"`
}

// compareJudgeRecord lets anyone repeat the judging: the exact request sent
// (POST it to Endpoint with an OpenRouter key) and every pool result's answer.
type compareJudgeRecord struct {
	Model    string             `json:"model"`
	Endpoint string             `json:"endpoint"`
	Request  *judge.Request     `json:"request,omitempty"`
	Pool     []compareJudgeItem `json:"pool"`
}

// compareJudgeItem is one pool result as numbered in the request, with Jev's
// answers: relevance 0-3, its confidence, the spam probability, and the 0-1
// grade made from them (judge.Score).
type compareJudgeItem struct {
	N          int     `json:"n"`
	URL        string  `json:"url"`
	Title      string  `json:"title"`
	Relevance  float64 `json:"relevance_0_3"`
	Confidence float64 `json:"confidence"`
	Spam       float64 `json:"spam_probability"`
	Grade      float64 `json:"grade"`
}

type compareService struct {
	providers  []compareProvider
	extractors []compareExtractor
	scorer     compareScorer
	judgeAsk   compareAsker // Jev's yes/no questions for the extract checks
	sessions   *access.Sessions

	ipLimit  *access.IPLimiter
	running  *access.Gate
	dailyCap int

	// Clicks on search results (read that page with every extract API).
	clickLimit    *access.IPLimiter
	clickRunning  *access.Gate
	clickDailyCap int
	clickDay      string
	clickCount    int

	// store keeps finished runs (public list, links, 24-hour cache); nil = none.
	store *compareStore
	// extractOn shows the Extract tab to everyone; off, only local requests see it.
	extractOn bool
	// pools are each provider's keys (shared by search and extract).
	pools map[string]*compareKeyPool

	mu       sync.Mutex
	day      string
	dayCount int

	now func() time.Time
}

func newCompareService(cfg *Config, sessions *access.Sessions, store *compareStore) *compareService {
	hc := &http.Client{Timeout: compareProviderMax}
	pools := compareKeyPools(cfg)
	s := &compareService{
		providers:     compareProviders(cfg, hc, pools),
		extractors:    compareExtractors(cfg, &http.Client{Timeout: compareExtractMax}, pools),
		pools:         pools,
		sessions:      sessions,
		ipLimit:       access.NewIPLimiter(3, cfg.ComparePerIPDay),
		running:       access.NewGate(compareMaxRunning),
		dailyCap:      cfg.CompareDailyCap,
		clickLimit:    access.NewIPLimiter(6, cfg.CompareClickPerIPDay),
		clickRunning:  access.NewGate(compareMaxRunning),
		clickDailyCap: cfg.CompareClickDailyCap,
		store:         store,
		extractOn:     cfg.CompareExtract,
		now:           time.Now,
	}
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for now := range t.C {
			s.ipLimit.Sweep(now)
			s.clickLimit.Sweep(now)
		}
	}()
	if cfg.OpenRouterAPIKey != "" {
		j := judge.New(cfg.OpenRouterAPIKey, cfg.JevModel)
		s.scorer = j
		s.judgeAsk = j
	}
	return s
}

//go:embed web/compare.html
var compareHTML []byte

// serveComparePage serves the page at "/"; every other unknown path is a 404.
func serveComparePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(compareHTML)
}

// compareProviders lists the columns in page order. Dowse runs basic search
// in-process (no credits); the others use their own free-tier keys. A provider
// without a key still gets a column, marked unavailable.
// compareKeyPools reads every provider's keys: NAME, NAME_2 ... NAME_9.
func compareKeyPools(cfg *Config) map[string]*compareKeyPool {
	pools := map[string]*compareKeyPool{
		"dowse":      newCompareKeyPool("dowse", compareKeysFromEnv("DOWSE_API_KEY", cfg.DowseAPIKey)),
		"exa":        newCompareKeyPool("exa", compareKeysFromEnv("EXA_API_KEY", cfg.ExaAPIKey)),
		"tavily":     newCompareKeyPool("tavily", compareKeysFromEnv("TAVILY_API_KEY", cfg.TavilyAPIKey)),
		"perplexity": newCompareKeyPool("perplexity", compareKeysFromEnv("PERPLEXITY_API_KEY", cfg.PerplexityAPIKey)),
		"contextdev": newCompareKeyPool("contextdev", compareKeysFromEnv("CONTEXT_DEV_API_KEY", cfg.ContextDevAPIKey)),
		"parallel":   newCompareKeyPool("parallel", compareKeysFromEnv("PARALLEL_API_KEY", cfg.ParallelAPIKey)),
		"keenable":   newCompareKeyPool("keenable", compareKeysFromEnv("KEENABLE_API_KEY", cfg.KeenableAPIKey)),
	}
	for name, p := range pools {
		log.Printf("compare: %s has %d key(s)", name, len(p.keys))
	}
	return pools
}

// pooledSearch runs f with the provider's keys in turn (see compareKeyPool.do).
func pooledSearch(pool *compareKeyPool, f func(ctx context.Context, key, q string, n int) ([]models.SearchResult, float64, error)) compareSearch {
	if pool.empty() {
		return keyed("", nil)
	}
	return func(ctx context.Context, q string, n int) ([]models.SearchResult, float64, error) {
		var rs []models.SearchResult
		var cost float64
		err := pool.do(ctx, func(key string) error {
			var err error
			rs, cost, err = f(ctx, key, q, n)
			return err
		})
		return rs, cost, err
	}
}

func compareProviders(cfg *Config, hc *http.Client, pools map[string]*compareKeyPool) []compareProvider {
	dowse := func(depth string) compareSearch {
		return pooledSearch(pools["dowse"], func(ctx context.Context, key, q string, n int) ([]models.SearchResult, float64, error) {
			return compareDowseSearch(ctx, hc, cfg.DowseAPIBase, key, depth, q, n)
		})
	}
	return []compareProvider{
		{ID: "dowse", Name: "Dowse", Mode: "basic search", Search: dowse("basic")},
		{ID: "dowse-adv", Name: "Dowse advanced", Mode: "advanced search", Timeout: compareAdvancedMax, Search: dowse("advanced")},
		{ID: "exa", Name: "Exa", Mode: "auto search", Search: pooledSearch(pools["exa"], func(ctx context.Context, key, q string, n int) ([]models.SearchResult, float64, error) {
			return compareExa(ctx, hc, key, q, n)
		})},
		{ID: "tavily", Name: "Tavily", Mode: "basic search", Search: pooledSearch(pools["tavily"], func(ctx context.Context, key, q string, n int) ([]models.SearchResult, float64, error) {
			return compareTavily(ctx, hc, key, q, n)
		})},
		{ID: "perplexity", Name: "Perplexity", Mode: "Search API", Search: pooledSearch(pools["perplexity"], func(ctx context.Context, key, q string, n int) ([]models.SearchResult, float64, error) {
			return comparePerplexity(ctx, hc, key, q, n)
		})},
		{ID: "contextdev", Name: "Context.dev", Mode: "web search", Search: pooledSearch(pools["contextdev"], func(ctx context.Context, key, q string, n int) ([]models.SearchResult, float64, error) {
			return compareContextDev(ctx, hc, key, q, n)
		})},
		{ID: "parallel", Name: "Parallel", Mode: "default (advanced)", Search: pooledSearch(pools["parallel"], func(ctx context.Context, key, q string, n int) ([]models.SearchResult, float64, error) {
			return compareParallel(ctx, hc, key, q, n)
		})},
		{ID: "keenable", Name: "Keenable", Mode: "default (pro)", Search: pooledSearch(pools["keenable"], func(ctx context.Context, key, q string, n int) ([]models.SearchResult, float64, error) {
			return compareKeenable(ctx, hc, key, q, n)
		})},
	}
}

var errCompareUnavailable = fmt.Errorf("provider not configured")

func keyed(key string, f compareSearch) compareSearch {
	if key == "" {
		return func(context.Context, string, int) ([]models.SearchResult, float64, error) {
			return nil, 0, errCompareUnavailable
		}
	}
	return f
}

// compareExa runs type auto, Exa's default, with highlights as the snippet.
func compareExa(ctx context.Context, hc *http.Client, key, q string, n int) ([]models.SearchResult, float64, error) {
	var out struct {
		Results []struct {
			Title      string   `json:"title"`
			URL        string   `json:"url"`
			Highlights []string `json:"highlights"`
		} `json:"results"`
		CostDollars struct {
			Total float64 `json:"total"`
		} `json:"costDollars"`
	}
	body := map[string]any{"query": q, "numResults": n, "type": "auto", "contents": map[string]any{"highlights": true}}
	if err := comparePost(ctx, hc, "https://api.exa.ai/search", map[string]string{"x-api-key": key}, body, &out); err != nil {
		return nil, 0, err
	}
	rs := make([]models.SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		rs = append(rs, models.SearchResult{Title: r.Title, URL: r.URL, Content: strings.Join(r.Highlights, " ")})
	}
	cost := out.CostDollars.Total
	if cost <= 0 {
		cost = priceExaFallback
	}
	return rs, cost, nil
}

// compareTavily runs basic depth (1 credit), Tavily's default.
func compareTavily(ctx context.Context, hc *http.Client, key, q string, n int) ([]models.SearchResult, float64, error) {
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	body := map[string]any{"query": q, "max_results": n, "search_depth": "basic"}
	if err := comparePost(ctx, hc, "https://api.tavily.com/search", map[string]string{"Authorization": "Bearer " + key}, body, &out); err != nil {
		return nil, 0, err
	}
	rs := make([]models.SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		rs = append(rs, models.SearchResult{Title: r.Title, URL: r.URL, Content: r.Content})
	}
	return rs, priceTavilyBasic, nil
}

// comparePerplexity uses Perplexity's Search API (ranked results), not Sonar's
// written answers, so it is results against results.
func comparePerplexity(ctx context.Context, hc *http.Client, key, q string, n int) ([]models.SearchResult, float64, error) {
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
		} `json:"results"`
	}
	body := map[string]any{"query": q, "max_results": n}
	if err := comparePost(ctx, hc, "https://api.perplexity.ai/search", map[string]string{"Authorization": "Bearer " + key}, body, &out); err != nil {
		return nil, 0, err
	}
	rs := make([]models.SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		rs = append(rs, models.SearchResult{Title: r.Title, URL: r.URL, Content: r.Snippet})
	}
	return rs, pricePerplexity, nil
}

// compareContextDev uses Context.dev web search (10 results is its smallest request).
func compareContextDev(ctx context.Context, hc *http.Client, key, q string, n int) ([]models.SearchResult, float64, error) {
	var out struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Description string `json:"description"`
		} `json:"results"`
		KeyMetadata struct {
			CreditsConsumed float64 `json:"credits_consumed"`
		} `json:"key_metadata"`
	}
	body := map[string]any{"query": q, "numResults": n}
	if err := comparePost(ctx, hc, "https://api.context.dev/v1/web/search", map[string]string{"Authorization": "Bearer " + key}, body, &out); err != nil {
		return nil, 0, err
	}
	rs := make([]models.SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		rs = append(rs, models.SearchResult{Title: r.Title, URL: r.URL, Content: r.Description})
	}
	credits := out.KeyMetadata.CreditsConsumed
	if credits <= 0 {
		credits = 1
	}
	return rs, credits * priceContextDevCredit, nil
}

func comparePost(ctx context.Context, hc *http.Client, u string, hdr map[string]string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(b)))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &compareHTTPError{Status: resp.StatusCode, Body: strings.Join(strings.Fields(string(body)), " ")}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// compareGet is comparePost for GET APIs.
func compareGet(ctx context.Context, hc *http.Client, u string, hdr map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return &compareHTTPError{Status: resp.StatusCode, Body: strings.Join(strings.Fields(string(body)), " ")}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(out)
}

// compareCacheKey folds case and spacing, so "Foo  bar" and "foo bar" share an answer.
func compareCacheKey(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}

// cached is the newest run of q from the last compareCacheTTL, or nil.
func (s *compareService) cached(q string) *compareRun {
	if s.store == nil {
		return nil
	}
	run, err := s.store.latest(q, s.now().Add(-compareCacheTTL))
	if err != nil {
		log.Printf("compare: cache read: %v", err)
	}
	return run
}

func (s *compareService) save(run *compareRun) {
	if s.store == nil {
		return
	}
	if err := s.store.save(run, s.now()); err != nil {
		log.Printf("compare: save: %v", err)
	}
}

// takeClick counts one fresh result click against its daily cap.
func (s *compareService) takeClick() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := s.now().UTC().Format("2006-01-02")
	if day != s.clickDay {
		s.clickDay, s.clickCount = day, 0
	}
	if s.clickDailyCap > 0 && s.clickCount >= s.clickDailyCap {
		return false
	}
	s.clickCount++
	return true
}

// takeDaily counts one fresh comparison against the daily cap.
func (s *compareService) takeDaily() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := s.now().UTC().Format("2006-01-02")
	if day != s.day {
		s.day, s.dayCount = day, 0
	}
	if s.dailyCap > 0 && s.dayCount >= s.dailyCap {
		return false
	}
	s.dayCount++
	return true
}

// sseWriter writes server-sent events and flushes each one.
type sseWriter struct {
	w  http.ResponseWriter
	f  http.Flusher
	mu sync.Mutex
}

func (e *sseWriter) send(event string, v any) {
	b, _ := json.Marshal(v)
	e.mu.Lock()
	defer e.mu.Unlock()
	fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", event, b)
	if e.f != nil {
		e.f.Flush()
	}
}

type compareProviderInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Mode string `json:"mode"`
}

// handler serves GET /api/compare?q=... as an event stream:
// start {providers, cached} → column (one per provider, as each answers) →
// verdict → done. Refusals before the stream are plain JSON errors.
func (s *compareService) handler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use GET"})
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "type a query"})
		return
	}
	if len([]rune(q)) > compareMaxQuery {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("query too long (max %d characters)", compareMaxQuery)})
		return
	}
	if !sessionAllowed(s.sessions, r, s.now()) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "session required"})
		return
	}

	run := s.cached(q)
	if run == nil {
		// Requests made on the machine itself (local testing) skip the per-IP limit;
		// through Caddy and Cloudflare every request is limited.
		if ok, retry := s.ipLimit.Allow(clientIP(r), s.now()); !ok && !isLocalRequest(r) {
			w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(retry.Seconds()))))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "You have used your comparisons for now. Try an example query, or come back later."})
			return
		}
		if !s.running.TryAcquire() {
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Busy right now. Try again in a few seconds."})
			return
		}
		defer s.running.Release()
		if !s.takeDaily() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Today's free comparisons are used up. Example queries still work."})
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	f, _ := w.(http.Flusher)
	out := &sseWriter{w: w, f: f}

	infos := s.providerInfos()
	if run != nil {
		infos = infosIn(infos, run.Columns)
	}
	start := map[string]any{"query": q, "providers": infos, "cached": run != nil}
	if run != nil {
		start["at"] = run.At
	}
	out.send("start", start)

	if run != nil {
		for _, c := range run.Columns {
			out.send("column", c)
		}
		if run.Verdict != nil {
			out.send("verdict", run.Verdict)
		}
		out.send("done", map[string]any{"id": run.ID})
		return
	}

	run = s.run(r.Context(), q, func(c compareColumn) { out.send("column", c) })
	if run.Verdict != nil {
		out.send("verdict", run.Verdict)
	} else {
		out.send("verdict_error", map[string]any{"error": "The judge could not score this query."})
	}
	// Keep only complete runs, so a judge or provider hiccup is neither listed nor served for a day.
	if run.Verdict != nil && r.Context().Err() == nil {
		s.save(run)
	}
	out.send("done", map[string]any{"id": run.ID})
}

func (s *compareService) providerInfos() []compareProviderInfo {
	infos := make([]compareProviderInfo, len(s.providers))
	for i, p := range s.providers {
		infos[i] = compareProviderInfo{ID: p.ID, Name: p.Name, Mode: p.Mode}
	}
	return infos
}

// infosIn keeps the providers a stored run has a column for, so a run saved
// before a provider was added does not show it as still loading.
func infosIn(infos []compareProviderInfo, cols []compareColumn) []compareProviderInfo {
	have := map[string]bool{}
	for _, c := range cols {
		have[c.ID] = true
	}
	var out []compareProviderInfo
	for _, p := range infos {
		if have[p.ID] {
			out = append(out, p)
		}
	}
	return out
}

// extractAllowed reports whether r may see the Extract tab, its runs and its endpoint.
func (s *compareService) extractAllowed(r *http.Request) bool {
	return s.extractOn || isLocalRequest(r)
}

// configHandler serves GET /api/compare/config: which tabs the page shows.
// view=public previews the page as visitors see it (it can only hide tabs).
func (s *compareService) configHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-cache")
	extract := s.extractAllowed(r) && r.URL.Query().Get("view") != "public"
	writeJSON(w, http.StatusOK, map[string]any{"extract": extract})
}

// historyHandler serves GET /api/compare/history: the public list, newest first.
func (s *compareService) historyHandler(w http.ResponseWriter, r *http.Request) {
	items := []compareHistoryItem{}
	if s.store != nil {
		var err error
		if items, err = s.store.list(compareHistoryMax); err != nil {
			log.Printf("compare: history: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not load the list"})
			return
		}
	}
	// Pasted-URL checks are listed where the Extract tab shows; checks of a
	// clicked result (they carry the search's question) are not, so the list
	// stays a list of comparisons.
	showExtract := s.extractAllowed(r) && r.URL.Query().Get("view") != "public"
	kept := items[:0]
	for _, it := range items {
		if it.Kind != compareKindExtract || (showExtract && it.Question == "") {
			kept = append(kept, it)
		}
	}
	items = kept
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]any{"runs": items})
}

// runHandler serves GET /api/compare/run?id=N: one stored run, free to open.
func (s *compareService) runHandler(w http.ResponseWriter, r *http.Request) {
	var id int64
	if _, err := fmt.Sscan(r.URL.Query().Get("id"), &id); err != nil || id <= 0 || s.store == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "comparison not found"})
		return
	}
	kind, raw, err := s.store.get(id)
	if err != nil {
		log.Printf("compare: run %d: %v", id, err)
	}
	if raw == nil || (kind == compareKindExtract && !s.extractAllowed(r)) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "comparison not found"})
		return
	}
	providers := s.providerInfos()
	if kind == compareKindExtract {
		providers = s.extractorInfos()
	}
	if r.URL.Query().Get("download") != "" {
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="dowse-compare-run-%d.json"`, id))
		writeJSON(w, http.StatusOK, compareDownload(kind, raw, providers))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"kind": kind, "run": withoutJudge(raw), "providers": providers})
}

// run calls every provider in parallel, reports each column as it lands, then judges.
func (s *compareService) run(ctx context.Context, q string, onColumn func(compareColumn)) *compareRun {
	run := &compareRun{Query: q, Columns: make([]compareColumn, len(s.providers))}
	lists := make([][]models.SearchResult, len(s.providers))
	var wg sync.WaitGroup
	for i, p := range s.providers {
		wg.Add(1)
		go func(i int, p compareProvider) {
			defer wg.Done()
			limit := compareProviderMax
			if p.Timeout > 0 {
				limit = p.Timeout
			}
			pctx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()
			start := time.Now()
			res, cost, err := p.Search(pctx, q, compareResults)
			col := compareColumn{ID: p.ID, Status: "ok", Ms: time.Since(start).Milliseconds(), Results: []compareResult{}}
			switch {
			case err == errCompareUnavailable:
				col.Status = "unavailable"
			case err == errCompareNoKeyLeft:
				col.Status = "limit"
				log.Printf("compare: %s has no usable key left", p.ID)
			case err == errCompareBusy:
				col.Status = "busy"
				log.Printf("compare: %s is rate limiting every key", p.ID)
			case err != nil && pctx.Err() == context.DeadlineExceeded:
				col.Status = "timeout"
				log.Printf("compare: %s timed out after %dms", p.ID, col.Ms)
			case err != nil:
				col.Status = "error"
				log.Printf("compare: %s failed: %v", p.ID, err)
			default:
				col.CostUSD = math.Round(cost*1e6) / 1e6
				res = dedupeResults(res)
				if len(res) > compareResults {
					res = res[:compareResults]
				}
				lists[i] = res
				for _, it := range res {
					col.Results = append(col.Results, compareResult{Title: it.Title, URL: it.URL, Snippet: compareSnippet(it.Content)})
				}
			}
			run.Columns[i] = col
			onColumn(col)
		}(i, p)
	}
	wg.Wait()

	if s.scorer != nil && ctx.Err() == nil {
		jctx, cancel := context.WithTimeout(ctx, compareJudgeMax)
		defer cancel()
		run.Verdict, run.Judge = s.judge(jctx, q, lists)
	}
	return run
}

// dedupeResults keeps the first result for each URL.
func dedupeResults(rs []models.SearchResult) []models.SearchResult {
	seen := map[string]bool{}
	out := rs[:0:0]
	for _, r := range rs {
		k := fusion.NormalizeURL(r.URL)
		if r.URL == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, r)
	}
	return out
}

func compareSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 320 {
		return string(r[:320]) + "…"
	}
	return s
}

// judge scores the blind pool with Jev and turns the grades into per-provider
// nDCG@10. The pool has each URL once, with the longest snippet any provider
// gave, in random order, and no provider names: Jev sees results, not brands.
func (s *compareService) judge(ctx context.Context, q string, lists [][]models.SearchResult) (*compareVerdict, *compareJudgeRecord) {
	pool := map[string]models.SearchResult{}
	for _, list := range lists {
		for _, it := range list {
			k := fusion.SamePageKey(it.URL)
			if prev, ok := pool[k]; !ok || len(it.Content) > len(prev.Content) {
				pool[k] = models.SearchResult{Title: it.Title, URL: it.URL, Content: it.Content}
			}
		}
	}
	if len(pool) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(pool))
	for k := range pool {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	rand.Shuffle(len(keys), func(i, j int) { keys[i], keys[j] = keys[j], keys[i] })
	items := make([]judge.Result, len(keys))
	for i, k := range keys {
		items[i] = judge.Result{Title: pool[k].Title, URL: pool[k].URL, Snippet: pool[k].Content}
	}

	// Every provider is judged with the same date, so fresh pages can count
	// on queries about recent events.
	today := s.now().UTC().Format("2006-01-02")
	req := s.scorer.Request(today, q, items)
	rec := &compareJudgeRecord{Model: req.Model, Endpoint: s.scorer.Endpoint(), Request: &req}
	grades, cost, err := s.scorer.GradeResults(ctx, today, q, items)
	log.Printf("compare: search judge: %d results, $%.4f", len(items), cost)
	if err != nil {
		log.Printf("compare: judge failed: %v", err)
		return nil, nil
	}
	grade := map[string]float64{}
	raw := map[string]float64{}
	for i, it := range items {
		g := grades[i]
		k := fusion.SamePageKey(it.URL)
		if g.OK {
			grade[k], raw[k] = g.Score, g.Relevance
		}
		rec.Pool = append(rec.Pool, compareJudgeItem{N: i + 1, URL: it.URL, Title: it.Title,
			Relevance: g.Relevance, Confidence: g.Confidence, Spam: g.Spam, Grade: g.Score})
	}
	return verdictFromGrades(s.providers, lists, grade, raw), rec
}

// verdictFromGrades scores each provider's list by nDCG@10 against the ideal
// order of the whole pool: a provider loses points both for weak results and
// for missing strong ones that another provider found.
func verdictFromGrades(providers []compareProvider, lists [][]models.SearchResult, grade, raw map[string]float64) *compareVerdict {
	all := make([]float64, 0, len(grade))
	for _, g := range grade {
		all = append(all, g)
	}
	sort.Sort(sort.Reverse(sort.Float64Slice(all)))
	ideal := dcg(all, compareResults)

	v := &compareVerdict{Scores: map[string]compareScore{}, Pool: len(grade)}
	for i, p := range providers {
		list := lists[i]
		cs := compareScore{Grades: make([]float64, len(list))}
		for j, it := range list {
			k := fusion.SamePageKey(it.URL)
			cs.Grades[j] = math.Round(grade[k]*100) / 100
			if raw[k] >= 2 {
				cs.Useful++
			}
		}
		if len(list) > 0 && ideal > 0 {
			sc := math.Round(dcg(cs.Grades, compareResults)/ideal*1000) / 10
			cs.Score = &sc
		}
		v.Scores[p.ID] = cs
	}
	scores := map[string]float64{}
	for id, cs := range v.Scores {
		if cs.Score != nil {
			scores[id] = *cs.Score
		}
	}
	v.Winner = compareWinner(scores)
	return v
}

// compareCompany is the company a column belongs to: "dowse-adv" is Dowse.
func compareCompany(id string) string {
	c, _, _ := strings.Cut(id, "-")
	return c
}

// compareWinner is the top column when it leads every other company's best
// column by at least compareMinLead points (the judge varies a little from run
// to run, so a smaller lead is a tie). Two modes of one company never tie
// each other out: Dowse basic 89.7 and advanced 88.2 against Exa 82.9 is a
// Dowse win, shown on the higher Dowse column.
func compareWinner(scores map[string]float64) string {
	leader, best := "", -1.0
	for id, sc := range scores {
		if sc > best || (sc == best && id < leader) {
			leader, best = id, sc
		}
	}
	if leader == "" {
		return ""
	}
	rival := -1.0
	for id, sc := range scores {
		if compareCompany(id) != compareCompany(leader) && sc > rival {
			rival = sc
		}
	}
	if rival < 0 || best-rival >= compareMinLead {
		return leader
	}
	return ""
}

// compareMinLead is the smallest lead (score points) that names a winner.
const compareMinLead = 2.0

func dcg(gains []float64, k int) float64 {
	sum := 0.0
	for i, g := range gains {
		if i >= k {
			break
		}
		sum += g / math.Log2(float64(i)+2)
	}
	return sum
}

// withoutJudge drops the judge record from a run's JSON: the page does not
// need it, and it is the largest part.
func withoutJudge(raw json.RawMessage) json.RawMessage {
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return raw
	}
	delete(m, "judge")
	b, err := json.Marshal(m)
	if err != nil {
		return raw
	}
	return b
}

// compareDownload is the downloadable copy of a run: everything stored, plus
// how the numbers were made and how to repeat the judging.
func compareDownload(kind string, raw json.RawMessage, providers []compareProviderInfo) map[string]any {
	out := map[string]any{
		"kind":      kind,
		"providers": providers,
		"run":       raw,
	}
	if kind != compareKindSearch {
		return out
	}
	out["how_it_is_scored"] = []string{
		"Every provider got the same query and returned up to 10 results with its default settings.",
		"All results went into one pool: copies of a page merged, order shuffled, provider names removed (run.judge.request is that pool).",
		"The judge, Jev (TypeSafe's System One model on OpenRouter), answered for each pool result: relevance 0-3 with a confidence, and the probability it is spam or clickbait. Its prompt is in run.judge.request and in the code (internal/judge). Dowse also uses Jev to rank its own results, with a different prompt.",
		"grade = min(1, relevance/3) x (0.7 + 0.3 x confidence) x max(0.1, 1 - 0.75 x spam_probability). The page shows it x 100.",
		"Each provider's score = DCG of its own 10 grades in its own order, divided by the DCG of the best 10 grades in the whole pool, x 100 (nDCG@10). DCG = sum of grade / log2(position + 1).",
		"A lead under 2 points is a tie. Prices are list prices per query at each provider's entry paid tier.",
	}
	out["check_it_yourself"] = map[string]any{
		"what":    "Send run.judge.request as the JSON body to run.judge.endpoint with your own OpenRouter key, then compare the answers with run.judge.pool. The judge varies a little between runs, so expect small differences.",
		"command": "jq '.run.judge.request' dowse-compare-run-N.json | curl -s https://openrouter.ai/api/v1/systemone -H \"Authorization: Bearer $OPENROUTER_API_KEY\" -H 'Content-Type: application/json' -d @-",
		"note":    "Runs saved before 2026-11-20 have grades but no judge request.",
	}
	return out
}

// List prices (checked 2026-11-20): Parallel Search in its default mode,
// advanced, is $5 per 1,000 requests of 10 results; Keenable's default (pro)
// is $4 per 1,000 pay as you go, after 100,000 free requests a month.
const (
	priceParallelAdvanced = 0.005
	priceKeenablePro      = 0.004
)

// compareParallel runs Parallel's Search API in its default mode. Like every
// other API here it gets only the visitor's query, as the objective and as the
// one search query: Parallel's docs suggest writing a goal and 2-3 keyword
// queries instead, which no other provider is given.
func compareParallel(ctx context.Context, hc *http.Client, key, q string, n int) ([]models.SearchResult, float64, error) {
	var out struct {
		Results []struct {
			Title    string   `json:"title"`
			URL      string   `json:"url"`
			Excerpts []string `json:"excerpts"`
		} `json:"results"`
	}
	body := map[string]any{"objective": q, "search_queries": []string{q}}
	if err := comparePost(ctx, hc, "https://api.parallel.ai/v1/search", map[string]string{"x-api-key": key}, body, &out); err != nil {
		return nil, 0, err
	}
	rs := make([]models.SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		rs = append(rs, models.SearchResult{Title: r.Title, URL: r.URL, Content: strings.Join(r.Excerpts, " ")})
	}
	if len(rs) > n {
		rs = rs[:n]
	}
	return rs, priceParallelAdvanced, nil
}

// compareKeenable runs Keenable's search in its default mode.
func compareKeenable(ctx context.Context, hc *http.Client, key, q string, n int) ([]models.SearchResult, float64, error) {
	var out struct {
		Results []struct {
			Title       string `json:"title"`
			URL         string `json:"url"`
			Snippet     string `json:"snippet"`
			Description string `json:"description"`
		} `json:"results"`
	}
	body := map[string]any{"query": q, "max_results": n}
	if err := comparePost(ctx, hc, "https://api.keenable.ai/v1/search", map[string]string{"X-API-Key": key}, body, &out); err != nil {
		return nil, 0, err
	}
	rs := make([]models.SearchResult, 0, len(out.Results))
	for _, r := range out.Results {
		text := r.Snippet
		if text == "" {
			text = r.Description
		}
		rs = append(rs, models.SearchResult{Title: r.Title, URL: r.URL, Content: text})
	}
	return rs, priceKeenablePro, nil
}
