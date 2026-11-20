// Command dowse-compare serves compare.dowse.dev: one query (or page) sent to
// Dowse, Exa, Tavily, Perplexity and Context.dev at once through their public
// APIs, every result graded blind by the same judge, and every run saved in a
// public list with a downloadable record anyone can check.
package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"dowse-compare/internal/access"
)

func main() {
	cfg := loadConfig()
	if cfg.DowseAPIKey == "" {
		log.Printf("DOWSE_API_KEY is not set: the Dowse columns will show as not connected")
	}

	secret := []byte(cfg.SessionSecret)
	if len(secret) == 0 {
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			log.Fatal(err)
		}
	}
	// Turnstile: a human check before fresh comparisons, when both keys are set.
	var sessions *access.Sessions
	var turnstile *access.Turnstile
	if cfg.TurnstileSecret != "" && !strings.EqualFold(cfg.TurnstileSecret, "off") && cfg.TurnstileSiteKey != "" {
		sessions = access.NewSessions(secret, time.Hour)
		turnstile = access.NewTurnstile(cfg.TurnstileSecret, &http.Client{Timeout: 5 * time.Second})
		log.Printf("Turnstile human check on")
	} else {
		log.Printf("Turnstile not set: comparisons are not behind a human check")
	}

	store, err := openCompareStore(cfg.CompareDB)
	if err != nil {
		log.Fatalf("open %s: %v", cfg.CompareDB, err)
	}
	defer store.Close()
	svc := newCompareService(cfg, sessions, store)
	sessionLimiter := access.NewIPLimiter(10, 200)

	mux := http.NewServeMux()
	mux.HandleFunc("/", serveComparePage)
	mux.HandleFunc("/api/compare", svc.handler)
	mux.HandleFunc("/api/compare/history", svc.historyHandler)
	mux.HandleFunc("/api/compare/leaderboard", svc.leaderboardHandler)
	mux.HandleFunc("/api/compare/run", svc.runHandler)
	mux.HandleFunc("/api/compare/extract", svc.extractHandler)
	mux.HandleFunc("/api/compare/config", svc.configHandler)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	// The page's human check: the public site key, then a token for a session cookie.
	mux.HandleFunc("/api/config", func(w http.ResponseWriter, r *http.Request) {
		key := ""
		if sessions != nil {
			key = cfg.TurnstileSiteKey
		}
		writeJSON(w, http.StatusOK, map[string]any{"turnstile_site_key": key})
	})
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "use POST"})
			return
		}
		if sessions == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if ok, _ := sessionLimiter.Allow(clientIP(r), time.Now()); !ok {
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "too many checks"})
			return
		}
		var body struct {
			Token string `json:"token"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body)
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		if err := turnstile.Verify(ctx, body.Token, clientIP(r)); err != nil {
			log.Printf("turnstile verify failed: %v", err)
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "turnstile failed"})
			return
		}
		sessions.Issue(w, r.Host, clientIP(r), time.Now())
		w.WriteHeader(http.StatusNoContent)
	})

	srv := &http.Server{Addr: "127.0.0.1:" + cfg.Port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for now := range t.C {
			sessionLimiter.Sweep(now)
		}
	}()
	go func() {
		log.Printf("dowse-compare on http://%s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	// Finish running comparisons (up to the advanced timeout) before exiting.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}
