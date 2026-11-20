package main

// The leaderboard: every provider's search scores across all saved
// comparisons, each query counted once (its latest run), so repeating one
// query cannot move the table.

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"sort"
	"time"
)

// compareLeaderRow is one provider's line in the leaderboard.
type compareLeaderRow struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Mode     string  `json:"mode"`
	Queries  int     `json:"queries"` // queries it was scored on
	Average  float64 `json:"average"`
	Median   float64 `json:"median"`
	Wins     int     `json:"wins"`
	MedianMs int64   `json:"median_ms"`
	PriceUSD float64 `json:"price_usd"` // per query, from its latest answer
	// Ranked is false while the provider has too few queries for its average
	// to be compared fairly (it joined later): see minRankedQueries.
	Ranked bool `json:"ranked"`
}

// minRankedQueries is how many queries a provider needs to be ranked: half
// of all queries in the table, and at least 5. A provider added later is
// listed below the ranked ones until then.
func minRankedQueries(total int) int {
	return max(5, (total+1)/2)
}

type compareLeaderboard struct {
	Queries int                `json:"queries"` // distinct queries in the table
	Rows    []compareLeaderRow `json:"rows"`
	At      time.Time          `json:"at"`
}

// searchRuns returns the newest visible run of every search query.
func (s *compareStore) searchRuns() ([]compareRun, error) {
	rows, err := s.db.Query(`
SELECT run FROM compare_runs
WHERE id IN (SELECT MAX(id) FROM compare_runs WHERE hidden = 0 AND kind = 'search' GROUP BY qkey)
ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []compareRun
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var run compareRun
		if json.Unmarshal([]byte(raw), &run) == nil && run.Verdict != nil {
			out = append(out, run)
		}
	}
	return out, rows.Err()
}

// leaderboard builds the table from runs (oldest first) for providers, in
// order of average score.
func leaderboard(runs []compareRun, providers []compareProvider) compareLeaderboard {
	type acc struct {
		scores []float64
		ms     []int64
		wins   int
		price  float64
	}
	accs := map[string]*acc{}
	for _, p := range providers {
		accs[p.ID] = &acc{}
	}
	lb := compareLeaderboard{Rows: []compareLeaderRow{}}
	for _, run := range runs {
		counted := false
		for id, sc := range run.Verdict.Scores {
			a, ok := accs[id]
			if !ok || sc.Score == nil {
				continue
			}
			a.scores = append(a.scores, *sc.Score)
			counted = true
		}
		if !counted {
			continue
		}
		lb.Queries++
		if a, ok := accs[run.Verdict.Winner]; ok {
			a.wins++
		}
		for _, c := range run.Columns {
			if a, ok := accs[c.ID]; ok && c.Status == "ok" {
				a.ms = append(a.ms, c.Ms)
				if c.CostUSD > 0 {
					a.price = c.CostUSD // runs are oldest first: the newest price wins
				}
			}
		}
	}
	for _, p := range providers {
		a := accs[p.ID]
		if len(a.scores) == 0 {
			continue
		}
		row := compareLeaderRow{ID: p.ID, Name: p.Name, Mode: p.Mode, Queries: len(a.scores), Wins: a.wins, PriceUSD: a.price}
		sum := 0.0
		for _, v := range a.scores {
			sum += v
		}
		row.Average = math.Round(sum/float64(len(a.scores))*10) / 10
		sort.Float64s(a.scores)
		row.Median = math.Round(medianF(a.scores)*10) / 10
		if len(a.ms) > 0 {
			sort.Slice(a.ms, func(i, j int) bool { return a.ms[i] < a.ms[j] })
			row.MedianMs = a.ms[len(a.ms)/2]
		}
		lb.Rows = append(lb.Rows, row)
	}
	need := minRankedQueries(lb.Queries)
	for i := range lb.Rows {
		lb.Rows[i].Ranked = lb.Rows[i].Queries >= need
	}
	sort.SliceStable(lb.Rows, func(i, j int) bool {
		if lb.Rows[i].Ranked != lb.Rows[j].Ranked {
			return lb.Rows[i].Ranked
		}
		return lb.Rows[i].Average > lb.Rows[j].Average
	})
	return lb
}

func medianF(sorted []float64) float64 {
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// leaderboardHandler serves GET /api/compare/leaderboard.
func (s *compareService) leaderboardHandler(w http.ResponseWriter, r *http.Request) {
	lb := compareLeaderboard{Rows: []compareLeaderRow{}}
	if s.store != nil {
		runs, err := s.store.searchRuns()
		if err != nil {
			log.Printf("compare: leaderboard: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "could not build the leaderboard"})
			return
		}
		lb = leaderboard(runs, s.providers)
	}
	lb.At = s.now().UTC().Truncate(time.Second)
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, lb)
}
