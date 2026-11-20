package main

// The extract side of the comparison page: one URL (and an optional question)
// goes to every provider's extract API at once, each fetching the page live,
// and Jev then answers yes/no questions about each text without knowing which
// provider produced it: is it the page's real content (not a wall, bot check
// or error page), and does it answer the question. The page shows previews
// only, never whole pages.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"dowse-compare/internal/fusion"
	"dowse-compare/internal/judge"
)

const (
	compareExtractMax      = 45 * time.Second // per provider call
	compareExtractCacheTTL = 6 * time.Hour    // pages change faster than search results
	compareMaxQuestion     = 200
	comparePreviewChars    = 1500
	// compareJudgeTextChars is how much of each text the judge reads (samples,
	// not the whole page): enough to tell real content from a wall, at about
	// 1,100 tokens per API.
	compareJudgeTextChars = 4500
)

// Extract list prices per page (checked 2026-11-19), entry paid tier:
//   - Dowse basic extract: 2 credits per page read alone; $10 pack = 13,000 credits.
//   - Exa contents (text): $1/1k pages; Exa reports the exact cost.
//   - Tavily extract basic: 1 credit per 5 successful URLs at $0.008 a credit;
//     the reported usage is used when present.
//   - Perplexity Context API: $1/1k URLs; it reports the cost.
//   - Context.dev scrape: 1 credit, $20/10,000 on the Developer plan.
const (
	priceDowseExtract = 2 * 10.0 / 13000
	priceExaContents  = 0.001
	// Tavily bills 1 credit ($0.008) per started group of 5 successful pages,
	// so one page read alone costs a whole credit.
	priceTavilyExtractPage = 0.008
	pricePerplexityContext = 0.001
)

// compareExtractOutcome is what one provider's extract call returns. Reason is
// a short public description of a failure; raw errors stay in the log.
type compareExtractOutcome struct {
	Text    string
	CostUSD float64
	Reason  string
}

type compareExtractor struct {
	ID, Name, Mode string
	Extract        func(ctx context.Context, url string) (compareExtractOutcome, error)
}

// compareExtractColumn is one provider's answer as the page shows it.
type compareExtractColumn struct {
	ID      string  `json:"id"`
	Status  string  `json:"status"` // ok, failed, timeout, unavailable
	Reason  string  `json:"reason,omitempty"`
	Ms      int64   `json:"ms"`
	CostUSD float64 `json:"cost_usd,omitempty"`
	Chars   int     `json:"chars"`
	Preview string  `json:"preview"`
	text    string  // whole text, for the judge only
}

// compareExtractCheck is Jev's verdict on one provider's text.
type compareExtractCheck struct {
	Real    bool     `json:"real"`
	RealP   float64  `json:"real_p"`
	Answers *bool    `json:"answers,omitempty"` // nil when no question was asked
	AnswerP *float64 `json:"answer_p,omitempty"`
}

type compareExtractVerdict struct {
	Checks   map[string]compareExtractCheck `json:"checks"`
	Fastest  string                         `json:"fastest,omitempty"`  // fastest provider that passed every check
	Cheapest string                         `json:"cheapest,omitempty"` // cheapest provider that passed every check
	Passed   int                            `json:"passed"`
}

type compareExtractRun struct {
	ID       int64                  `json:"id,omitempty"`
	At       time.Time              `json:"at,omitempty"`
	URL      string                 `json:"url"`
	Question string                 `json:"question,omitempty"`
	Columns  []compareExtractColumn `json:"columns"`
	Verdict  *compareExtractVerdict `json:"verdict"`
}

// compareExtractKey is the cache key: the URL and the question, case-folded.
func compareExtractKey(u, q string) string {
	return "extract|" + strings.TrimSpace(u) + "|" + compareCacheKey(q)
}

func compareExtractors(cfg *Config, hc *http.Client, pools map[string]*compareKeyPool) []compareExtractor {
	unavailable := func(context.Context, string) (compareExtractOutcome, error) {
		return compareExtractOutcome{}, errCompareUnavailable
	}
	pooled := func(pool *compareKeyPool, f func(ctx context.Context, key, u string) (compareExtractOutcome, error)) func(context.Context, string) (compareExtractOutcome, error) {
		if pool.empty() {
			return unavailable
		}
		return func(ctx context.Context, u string) (compareExtractOutcome, error) {
			var o compareExtractOutcome
			err := pool.do(ctx, func(key string) error {
				var err error
				o, err = f(ctx, key, u)
				return err
			})
			return o, err
		}
	}
	dowse := pooled(pools["dowse"], func(ctx context.Context, key, u string) (compareExtractOutcome, error) {
		return extractDowse(ctx, hc, cfg.DowseAPIBase, key, u)
	})
	return []compareExtractor{
		{ID: "dowse", Name: "Dowse", Mode: "basic extract", Extract: dowse},
		{ID: "exa", Name: "Exa", Mode: "contents, live", Extract: pooled(pools["exa"], func(ctx context.Context, key, u string) (compareExtractOutcome, error) {
			return extractExa(ctx, hc, key, u)
		})},
		{ID: "tavily", Name: "Tavily", Mode: "basic extract", Extract: pooled(pools["tavily"], func(ctx context.Context, key, u string) (compareExtractOutcome, error) {
			return extractTavily(ctx, hc, key, u)
		})},
		{ID: "perplexity", Name: "Perplexity", Mode: "Context API, live", Extract: pooled(pools["perplexity"], func(ctx context.Context, key, u string) (compareExtractOutcome, error) {
			return extractPerplexity(ctx, hc, key, u)
		})},
		{ID: "contextdev", Name: "Context.dev", Mode: "scrape, live", Extract: pooled(pools["contextdev"], func(ctx context.Context, key, u string) (compareExtractOutcome, error) {
			return extractContextDev(ctx, hc, key, u)
		})},
		{ID: "parallel", Name: "Parallel", Mode: "extract, full page", Extract: pooled(pools["parallel"], func(ctx context.Context, key, u string) (compareExtractOutcome, error) {
			return extractParallel(ctx, hc, key, u)
		})},
		{ID: "keenable", Name: "Keenable", Mode: "fetch, live", Extract: pooled(pools["keenable"], func(ctx context.Context, key, u string) (compareExtractOutcome, error) {
			return extractKeenable(ctx, hc, key, u)
		})},
	}
}

// extractExa reads the page live (maxAgeHours 0) as text.
func extractExa(ctx context.Context, hc *http.Client, key, u string) (compareExtractOutcome, error) {
	var out struct {
		Results []struct {
			Text string `json:"text"`
		} `json:"results"`
		Statuses []struct {
			Status string `json:"status"`
			Error  *struct {
				Tag            string `json:"tag"`
				HTTPStatusCode int    `json:"httpStatusCode"`
			} `json:"error"`
		} `json:"statuses"`
		CostDollars struct {
			Total float64 `json:"total"`
		} `json:"costDollars"`
	}
	body := map[string]any{"urls": []string{u}, "text": true, "maxAgeHours": 0, "livecrawlTimeout": 30000}
	if err := comparePost(ctx, hc, "https://api.exa.ai/contents", map[string]string{"x-api-key": key}, body, &out); err != nil {
		return compareExtractOutcome{}, err
	}
	o := compareExtractOutcome{CostUSD: out.CostDollars.Total}
	if len(out.Results) > 0 && strings.TrimSpace(out.Results[0].Text) != "" {
		o.Text = out.Results[0].Text
		if o.CostUSD <= 0 {
			o.CostUSD = priceExaContents
		}
		return o, nil
	}
	o.Reason = "could not read the page"
	if len(out.Statuses) > 0 && out.Statuses[0].Error != nil {
		o.Reason = failureReason(out.Statuses[0].Error.Tag, out.Statuses[0].Error.HTTPStatusCode)
	}
	return o, nil
}

// extractTavily runs basic depth with markdown, Tavily's cheapest extract.
func extractTavily(ctx context.Context, hc *http.Client, key, u string) (compareExtractOutcome, error) {
	var out struct {
		Results []struct {
			RawContent string `json:"raw_content"`
		} `json:"results"`
		FailedResults []struct {
			Error string `json:"error"`
		} `json:"failed_results"`
		Usage struct {
			Credits float64 `json:"credits"`
		} `json:"usage"`
	}
	body := map[string]any{"urls": []string{u}, "extract_depth": "basic", "format": "markdown", "include_usage": true, "timeout": 40}
	if err := comparePost(ctx, hc, "https://api.tavily.com/extract", map[string]string{"Authorization": "Bearer " + key}, body, &out); err != nil {
		return compareExtractOutcome{}, err
	}
	if len(out.Results) > 0 && strings.TrimSpace(out.Results[0].RawContent) != "" {
		cost := priceTavilyExtractPage
		if out.Usage.Credits > 0 {
			cost = out.Usage.Credits * priceTavilyBasic
		}
		return compareExtractOutcome{Text: out.Results[0].RawContent, CostUSD: cost}, nil
	}
	o := compareExtractOutcome{Reason: "could not read the page"}
	if len(out.FailedResults) > 0 {
		o.Reason = failureReason(out.FailedResults[0].Error, 0)
	}
	return o, nil
}

// extractPerplexity uses the Context API with a live fetch (max_age_seconds 0).
func extractPerplexity(ctx context.Context, hc *http.Client, key, u string) (compareExtractOutcome, error) {
	var out struct {
		Results []struct {
			Markdown string `json:"markdown"`
			Status   struct {
				Type       string `json:"type"`
				StatusCode int    `json:"status_code"`
			} `json:"status"`
		} `json:"results"`
		Usage struct {
			Cost struct {
				TotalCost float64 `json:"total_cost"`
			} `json:"cost"`
		} `json:"usage"`
	}
	body := map[string]any{"urls": []string{u}, "formats": []string{"markdown"}, "max_age_seconds": 0, "timeout_ms": 40000}
	if err := comparePost(ctx, hc, "https://api.perplexity.ai/v1/context", map[string]string{"Authorization": "Bearer " + key}, body, &out); err != nil {
		return compareExtractOutcome{}, err
	}
	if len(out.Results) == 0 {
		return compareExtractOutcome{Reason: "could not read the page"}, nil
	}
	r := out.Results[0]
	if r.Status.Type == "ok" && strings.TrimSpace(r.Markdown) != "" {
		cost := out.Usage.Cost.TotalCost
		if cost <= 0 {
			cost = pricePerplexityContext
		}
		return compareExtractOutcome{Text: r.Markdown, CostUSD: cost}, nil
	}
	return compareExtractOutcome{Reason: failureReason(r.Status.Type, r.Status.StatusCode)}, nil
}

// extractContextDev scrapes the page as markdown with its cache off (maxAgeMs 0).
func extractContextDev(ctx context.Context, hc *http.Client, key, u string) (compareExtractOutcome, error) {
	var out struct {
		Markdown    json.RawMessage `json:"markdown"`
		ErrorCode   string          `json:"error_code"`
		KeyMetadata struct {
			CreditsConsumed float64 `json:"credits_consumed"`
		} `json:"key_metadata"`
	}
	body := map[string]any{"url": u, "formats": map[string]any{"markdown": true}, "maxAgeMs": 0}
	if err := comparePost(ctx, hc, "https://api.context.dev/v1/web/scrape", map[string]string{"Authorization": "Bearer " + key}, body, &out); err != nil {
		return compareExtractOutcome{}, err
	}
	// markdown is either a string or {"data": "...", "error": "..."}.
	var text, mdErr string
	if json.Unmarshal(out.Markdown, &text) != nil {
		var md struct {
			Data  string `json:"data"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal(out.Markdown, &md)
		text, mdErr = md.Data, md.Error
	}
	if strings.TrimSpace(text) != "" {
		credits := out.KeyMetadata.CreditsConsumed
		if credits <= 0 {
			credits = 1
		}
		return compareExtractOutcome{Text: text, CostUSD: credits * priceContextDevCredit}, nil
	}
	reason := mdErr
	if reason == "" {
		reason = out.ErrorCode
	}
	return compareExtractOutcome{Reason: failureReason(reason, 0)}, nil
}

// failureReason turns a provider's failure tag and the site's HTTP status into
// a short public phrase. Unknown tags become a generic phrase: provider
// messages can carry account or internal details.
func failureReason(tag string, status int) string {
	t := strings.ToLower(tag)
	var r string
	switch {
	case strings.Contains(t, "bot") || strings.Contains(t, "captcha") || strings.Contains(t, "challenge"):
		r = "stopped by a bot check"
	case t == "blocked" || strings.Contains(t, "not allowed") || strings.Contains(t, "policy"):
		r = "refused by the provider"
	case strings.Contains(t, "timeout") || strings.Contains(t, "timed out"):
		r = "timed out"
	case strings.Contains(t, "no_text") || strings.Contains(t, "empty") || strings.Contains(t, "no content"):
		r = "no text on the page"
	default:
		r = "could not read the page"
	}
	if status >= 400 {
		r += fmt.Sprintf(" (HTTP %d)", status)
	}
	return r
}

func comparePreview(text string) string {
	if utf8.RuneCountInString(text) <= comparePreviewChars {
		return text
	}
	r := []rune(text)
	return string(r[:comparePreviewChars]) + "…"
}

// extractHandler serves GET /api/compare/extract?url=...&q=... as an event
// stream: start → column (one per provider) → verdict → done.
func (s *compareService) extractHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use GET"})
		return
	}
	u := strings.TrimSpace(r.URL.Query().Get("url"))
	question := strings.TrimSpace(r.URL.Query().Get("q"))
	// A click on a result of a saved search run is public: only pages that run
	// returned can be read this way, and the question is the run's query, so
	// the endpoint is no free reader for any URL. Anything else needs the
	// Extract tab, which is off for the public.
	clicked := r.URL.Query().Get("run") != ""
	if clicked {
		var id int64
		if _, err := fmt.Sscan(r.URL.Query().Get("run"), &id); err != nil || id <= 0 {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "comparison not found"})
			return
		}
		ru, rq, ok := s.resultOfRun(id, u)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "that page is not one of this comparison's results"})
			return
		}
		u, question = ru, rq
	} else if !s.extractAllowed(r) {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "endpoint not found"})
		return
	} else if !isLocalRequest(r) {
		// A pasted URL is checked for real content only: a question only comes
		// from the search run a clicked result belongs to.
		question = ""
	}
	if u != "" && !strings.Contains(u, "://") {
		u = "https://" + u
	}
	if u == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "paste a URL"})
		return
	}
	if msg := checkURL(u); msg != "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": msg})
		return
	}
	if utf8.RuneCountInString(question) > compareMaxQuestion {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("question too long (max %d characters)", compareMaxQuestion)})
		return
	}
	if !sessionAllowed(s.sessions, r, s.now()) {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "session required"})
		return
	}

	key := compareExtractKey(u, question)
	run := s.cachedExtract(key)
	// Clicks and pasted URLs share one set of limits.
	if run == nil {
		if ok, retry := s.clickLimit.Allow(clientIP(r), s.now()); !ok && !isLocalRequest(r) {
			w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(retry.Seconds()))))
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "You have checked many pages for now. Try again in a minute."})
			return
		}
		if !s.clickRunning.TryAcquire() {
			w.Header().Set("Retry-After", "5")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Busy right now. Try again in a few seconds."})
			return
		}
		defer s.clickRunning.Release()
		if !s.takeClick() {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Today's free page checks are used up. Pages checked earlier still open."})
			return
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	f, _ := w.(http.Flusher)
	out := &sseWriter{w: w, f: f}
	start := map[string]any{"url": u, "question": question, "providers": s.extractorInfos(), "cached": run != nil}
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

	run = s.runExtract(r.Context(), u, question, func(c compareExtractColumn) { out.send("column", c) })
	if run.Verdict != nil {
		out.send("verdict", run.Verdict)
	} else {
		out.send("verdict_error", map[string]any{"error": "The judge could not check these pages."})
	}
	if run.Verdict != nil && r.Context().Err() == nil && s.store != nil {
		if err := s.store.saveExtract(run, key, s.now()); err != nil {
			log.Printf("compare: save extract: %v", err)
		}
	}
	out.send("done", map[string]any{"id": run.ID})
}

func (s *compareService) extractorInfos() []compareProviderInfo {
	infos := make([]compareProviderInfo, len(s.extractors))
	for i, p := range s.extractors {
		infos[i] = compareProviderInfo{ID: p.ID, Name: p.Name, Mode: p.Mode}
	}
	return infos
}

func (s *compareService) cachedExtract(key string) *compareExtractRun {
	if s.store == nil {
		return nil
	}
	run, err := s.store.latestExtract(key, s.now().Add(-compareExtractCacheTTL))
	if err != nil {
		log.Printf("compare: extract cache read: %v", err)
	}
	return run
}

// runExtract calls every extractor in parallel, reports each column as it
// lands, then has Jev check every text that came back.
func (s *compareService) runExtract(ctx context.Context, u, question string, onColumn func(compareExtractColumn)) *compareExtractRun {
	run := &compareExtractRun{URL: u, Question: question, Columns: make([]compareExtractColumn, len(s.extractors))}
	var wg sync.WaitGroup
	for i, p := range s.extractors {
		wg.Add(1)
		go func(i int, p compareExtractor) {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, compareExtractMax)
			defer cancel()
			start := time.Now()
			o, err := p.Extract(pctx, u)
			col := compareExtractColumn{ID: p.ID, Status: "ok", Ms: time.Since(start).Milliseconds()}
			switch {
			case err == errCompareUnavailable:
				col.Status = "unavailable"
			case err == errCompareNoKeyLeft:
				col.Status, col.Reason = "limit", "out of free quota right now"
			case err == errCompareBusy:
				col.Status, col.Reason = "busy", "busy (rate limited), try again in a minute"
			case err != nil && pctx.Err() == context.DeadlineExceeded:
				col.Status, col.Reason = "timeout", "timed out"
			case err != nil:
				col.Status, col.Reason = "failed", "could not read the page"
				log.Printf("compare: extract %s failed: %v", p.ID, err)
			case strings.TrimSpace(o.Text) == "":
				col.Status, col.Reason = "failed", o.Reason
			default:
				col.text = o.Text
				col.Chars = utf8.RuneCountInString(o.Text)
				col.Preview = comparePreview(o.Text)
				col.CostUSD = math.Round(o.CostUSD*1e6) / 1e6
			}
			run.Columns[i] = col
			onColumn(col)
		}(i, p)
	}
	wg.Wait()

	if s.judgeAsk != nil && ctx.Err() == nil {
		jctx, cancel := context.WithTimeout(ctx, compareJudgeMax)
		defer cancel()
		run.Verdict = s.judgeExtract(jctx, u, question, run.Columns)
	}
	return run
}

// compareAsker is judge.Client's Ask (faked in tests).
type compareAsker interface {
	Ask(ctx context.Context, req judge.Request) (map[string]json.RawMessage, float64, error)
}

// judgeExtract asks Jev, in one call, about every text that came back. The
// texts are numbered in random order with no provider names.
func (s *compareService) judgeExtract(ctx context.Context, u, question string, cols []compareExtractColumn) *compareExtractVerdict {
	var idx []int
	for i, c := range cols {
		if c.Status == "ok" {
			idx = append(idx, i)
		}
	}
	v := &compareExtractVerdict{Checks: map[string]compareExtractCheck{}}
	if len(idx) == 0 {
		return v
	}
	rand.Shuffle(len(idx), func(a, b int) { idx[a], idx[b] = idx[b], idx[a] })

	var b strings.Builder
	fmt.Fprintf(&b, "Several web readers fetched the same page: %s\n", u)
	b.WriteString("Below is the text each one returned, numbered. Some may have got a login wall, a bot check or CAPTCHA, a cookie or consent notice, " +
		"a paywall teaser, an error page or an almost empty shell instead of the page. Menus, footers and sign-in links around real content are fine.\n")
	if question != "" {
		fmt.Fprintf(&b, "A user wants to answer this question from the page: %q\n", question)
	}
	b.WriteString("\n")
	questions := map[string]judge.Question{}
	for n, i := range idx {
		k := n + 1
		fmt.Fprintf(&b, "[%d] Length: %d characters\n    Text: %q\n\n", k, cols[i].Chars, judgeExcerpt(cols[i].text, question))
		questions[fmt.Sprintf("real_%d", k)] = judge.Question{Type: "noul", Instructions: fmt.Sprintf(
			"Text [%d] is the real content of the page (the article, product, listing, docs or other content the URL is for), not a wall, check, notice or error page", k)}
		if question != "" {
			questions[fmt.Sprintf("answers_%d", k)] = judge.Question{Type: "noul", Instructions: fmt.Sprintf(
				"Text [%d] contains the answer to the user's question", k)}
		}
	}

	answers, cost, err := s.judgeAsk.Ask(ctx, judge.Request{State: b.String(), Questions: questions})
	log.Printf("compare: extract judge: %d texts, %d chars, $%.4f", len(idx), b.Len(), cost)
	if err != nil {
		log.Printf("compare: extract judge failed: %v", err)
		return nil
	}
	read := func(name string) (float64, bool) {
		var a struct {
			Noul float64 `json:"noul"`
		}
		raw, ok := answers[name]
		if !ok || json.Unmarshal(raw, &a) != nil {
			return 0, false
		}
		return a.Noul, true
	}
	got := 0
	for n, i := range idx {
		k := n + 1
		p, ok := read(fmt.Sprintf("real_%d", k))
		if !ok {
			continue
		}
		got++
		c := compareExtractCheck{RealP: math.Round(p*100) / 100, Real: p >= 0.5}
		if question != "" {
			if ap, ok := read(fmt.Sprintf("answers_%d", k)); ok {
				ap = math.Round(ap*100) / 100
				yes := ap >= 0.5
				c.Answers, c.AnswerP = &yes, &ap
			}
		}
		v.Checks[cols[i].ID] = c
	}
	if got == 0 {
		log.Printf("compare: extract judge answered none of %d texts", len(idx))
		return nil
	}

	// Badges go only to providers that passed every check.
	var passed []compareExtractColumn
	for _, c := range cols {
		ch, ok := v.Checks[c.ID]
		if ok && ch.Real && (ch.Answers == nil || *ch.Answers) {
			passed = append(passed, c)
		}
	}
	v.Passed = len(passed)
	if len(passed) > 1 {
		sort.SliceStable(passed, func(a, b int) bool { return passed[a].Ms < passed[b].Ms })
		v.Fastest = passed[0].ID
		sort.SliceStable(passed, func(a, b int) bool { return passed[a].CostUSD < passed[b].CostUSD })
		if passed[0].CostUSD < passed[1].CostUSD {
			v.Cheapest = passed[0].ID
		}
	}
	return v
}

// judgeExcerpt is the part of a text the judge reads: the whole text when it
// is short; else, with a question, the start and the passages that share the
// question's rarer words (in page order), so an answer deep in a long page is
// still seen; without one, passages spread evenly over the whole text. Every
// provider's text is cut the same way.
func judgeExcerpt(text, question string) string {
	r := []rune(text)
	if len(r) <= compareJudgeTextChars {
		return text
	}
	const chunk = 1500
	head := string(r[:chunk])
	if question == "" {
		// No question: samples spread evenly over the whole text (start, middle
		// and end), so a whole page is judged by its content, not only by the
		// menu at its top and the footer at its bottom.
		n := compareJudgeTextChars / chunk
		var sb strings.Builder
		for i := 0; i < n; i++ {
			at := i * (len(r) - chunk) / (n - 1)
			if i > 0 {
				sb.WriteString("\n…\n")
			}
			sb.WriteString(string(r[at:min(at+chunk, len(r))]))
		}
		return sb.String()
	}
	var terms []string
	seen := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(question)) {
		w = strings.Trim(w, ".,;:!?\"'()")
		if utf8.RuneCountInString(w) > 2 && !judgeStopWords[w] && !seen[w] {
			seen[w] = true
			terms = append(terms, w)
		}
	}
	type part struct {
		at    int
		low   string
		score float64
	}
	var parts []part
	for at := chunk; at < len(r); at += chunk {
		end := min(at+chunk, len(r))
		parts = append(parts, part{at: at, low: strings.ToLower(string(r[at:end]))})
	}
	// A word found in few passages says more than one found everywhere (the
	// page's own name, say), so each word counts by how rare it is.
	for _, t := range terms {
		in := 0
		for _, p := range parts {
			if strings.Contains(p.low, t) {
				in++
			}
		}
		if in == 0 {
			continue
		}
		weight := math.Log(1 + float64(len(parts))/float64(in))
		for i := range parts {
			if n := strings.Count(parts[i].low, t); n > 0 {
				parts[i].score += weight * (1 + math.Log(float64(n)))
			}
		}
	}
	sort.SliceStable(parts, func(a, b int) bool { return parts[a].score > parts[b].score })
	keep := parts[:min(len(parts), compareJudgeTextChars/chunk-1)]
	sort.Slice(keep, func(a, b int) bool { return keep[a].at < keep[b].at })
	var sb strings.Builder
	sb.WriteString(head)
	for _, p := range keep {
		sb.WriteString("\n…\n")
		sb.WriteString(string(r[p.at:min(p.at+chunk, len(r))]))
	}
	return sb.String()
}

// judgeStopWords are question words that say nothing about where an answer is.
var judgeStopWords = map[string]bool{
	"what": true, "which": true, "who": true, "whom": true, "when": true, "where": true, "why": true, "how": true,
	"the": true, "and": true, "for": true, "are": true, "was": true, "were": true, "does": true, "did": true,
	"this": true, "that": true, "these": true, "those": true, "with": true, "from": true, "about": true, "under": true,
	"into": true, "its": true, "their": true, "there": true, "page": true, "any": true, "can": true, "you": true,
	"your": true, "much": true, "many": true, "has": true, "have": true, "had": true, "will": true, "would": true,
}

// resultOfRun finds u among the results of saved search run id. It returns the
// result's URL as the run stored it and the run's query.
func (s *compareService) resultOfRun(id int64, u string) (string, string, bool) {
	if s.store == nil || u == "" {
		return "", "", false
	}
	kind, raw, err := s.store.get(id)
	if err != nil || raw == nil || kind != compareKindSearch {
		return "", "", false
	}
	var run compareRun
	if json.Unmarshal(raw, &run) != nil {
		return "", "", false
	}
	want := fusion.SamePageKey(u)
	for _, c := range run.Columns {
		for _, res := range c.Results {
			if fusion.SamePageKey(res.URL) == want {
				return res.URL, run.Query, true
			}
		}
	}
	return "", "", false
}

// List prices for one page (checked 2026-11-20): Parallel Extract $1 per
// 1,000 URLs; Keenable bills its credits at $0.004 each and reports them.
const (
	priceParallelExtract = 0.001
	priceKeenableCredit  = 0.004
)

// extractParallel reads the page with Parallel's Extract API, whole page.
func extractParallel(ctx context.Context, hc *http.Client, key, u string) (compareExtractOutcome, error) {
	var out struct {
		Results []struct {
			FullContent string   `json:"full_content"`
			Excerpts    []string `json:"excerpts"`
		} `json:"results"`
		Errors []struct {
			ErrorType      string `json:"error_type"`
			HTTPStatusCode int    `json:"http_status_code"`
		} `json:"errors"`
	}
	body := map[string]any{"urls": []string{u}, "advanced_settings": map[string]any{"full_content": true}}
	if err := comparePost(ctx, hc, "https://api.parallel.ai/v1/extract", map[string]string{"x-api-key": key}, body, &out); err != nil {
		return compareExtractOutcome{}, err
	}
	if len(out.Results) > 0 {
		text := out.Results[0].FullContent
		if strings.TrimSpace(text) == "" {
			text = strings.Join(out.Results[0].Excerpts, "\n\n")
		}
		if strings.TrimSpace(text) != "" {
			return compareExtractOutcome{Text: text, CostUSD: priceParallelExtract}, nil
		}
	}
	o := compareExtractOutcome{Reason: "could not read the page"}
	if len(out.Errors) > 0 {
		o.Reason = failureReason(out.Errors[0].ErrorType, out.Errors[0].HTTPStatusCode)
	}
	return o, nil
}

// extractKeenable reads the page with Keenable's fetch, live (not its index copy).
// Its 403 means either a bad key or a site that blocked the fetch, so page
// errors are told apart from key errors by the body.
func extractKeenable(ctx context.Context, hc *http.Client, key, u string) (compareExtractOutcome, error) {
	var out struct {
		Content string `json:"content"`
		Usage   struct {
			Credits float64 `json:"credits"`
		} `json:"usage"`
	}
	// Its default cap is 50,000 characters; the others return whole pages.
	q := url.Values{"url": {u}, "live": {"true"}, "max_chars": {"200000"}}
	err := compareGet(ctx, hc, "https://api.keenable.ai/v1/fetch?"+q.Encode(), map[string]string{"X-API-Key": key}, &out)
	var he *compareHTTPError
	if errors.As(err, &he) && keenablePageError(he) {
		return compareExtractOutcome{Reason: failureReason(he.Body, he.Status)}, nil
	}
	if err != nil {
		return compareExtractOutcome{}, err
	}
	if strings.TrimSpace(out.Content) == "" {
		return compareExtractOutcome{Reason: "no text on the page"}, nil
	}
	cost := out.Usage.Credits * priceKeenableCredit
	if cost <= 0 {
		cost = priceKeenableCredit
	}
	return compareExtractOutcome{Text: out.Content, CostUSD: cost}, nil
}

// keenablePageError: the page, not the key, failed (blocked by the site, not
// found, behind a login, unreachable or too slow).
func keenablePageError(e *compareHTTPError) bool {
	switch e.Status {
	case 404, 422, 502, 504:
		return true
	case 403:
		b := strings.ToLower(e.Body)
		return strings.Contains(b, "upstream") || strings.Contains(b, "private") || strings.Contains(b, "internal")
	}
	return false
}
