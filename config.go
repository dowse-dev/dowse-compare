package main

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Config is everything the app reads from the environment (or a .env file).
type Config struct {
	Port string

	// DowseAPIKey is a Dowse API key: Dowse is called through its public API,
	// exactly like the other providers. DOWSE_API_KEY_2 ... are extra keys.
	DowseAPIKey  string
	DowseAPIBase string

	ExaAPIKey        string
	TavilyAPIKey     string
	PerplexityAPIKey string
	ContextDevAPIKey string
	ParallelAPIKey   string
	KeenableAPIKey   string

	// OpenRouterAPIKey pays for the judge (Jev on OpenRouter); JevModel picks the model.
	OpenRouterAPIKey string
	JevModel         string

	// CompareDailyCap is how many fresh comparisons run per UTC day for everyone
	// together; ComparePerIPDay how many one IP may run.
	CompareDailyCap int
	ComparePerIPDay int
	// Clicks on a search result read that page with every extract API; they
	// have their own limits (per UTC day for everyone, per IP per day).
	CompareClickDailyCap int
	CompareClickPerIPDay int
	// CompareDB is the SQLite file of finished comparisons (the public list).
	CompareDB string
	// CompareExtract shows the Extract tab to everyone; off, only local requests see it.
	CompareExtract bool

	// Turnstile gates fresh comparisons behind a human check when both are set.
	TurnstileSiteKey string
	TurnstileSecret  string
	// SessionSecret signs the session cookie; empty = random per start.
	SessionSecret string
}

func loadConfig() *Config {
	loadDotEnv(".env")
	return &Config{
		Port:                 env("PORT", "8130"),
		DowseAPIKey:          env("DOWSE_API_KEY", ""),
		DowseAPIBase:         strings.TrimRight(env("DOWSE_API_BASE", "https://api.dowse.dev"), "/"),
		ExaAPIKey:            env("EXA_API_KEY", ""),
		TavilyAPIKey:         env("TAVILY_API_KEY", ""),
		PerplexityAPIKey:     env("PERPLEXITY_API_KEY", ""),
		ContextDevAPIKey:     env("CONTEXT_DEV_API_KEY", ""),
		ParallelAPIKey:       env("PARALLEL_API_KEY", ""),
		KeenableAPIKey:       env("KEENABLE_API_KEY", ""),
		OpenRouterAPIKey:     env("OPEN_ROUTER_API_KEY", ""),
		JevModel:             env("JEV_MODEL", "~typesafe/jev-latest"),
		CompareDailyCap:      envInt("COMPARE_DAILY_CAP", 30),
		ComparePerIPDay:      envInt("COMPARE_PER_IP_DAY", 10),
		CompareDB:            env("COMPARE_DB", "data/compare.db"),
		CompareClickDailyCap: envInt("COMPARE_CLICK_DAILY_CAP", 150),
		CompareClickPerIPDay: envInt("COMPARE_CLICK_PER_IP_DAY", 40),
		CompareExtract:       envBool("COMPARE_EXTRACT", false),
		TurnstileSiteKey:     env("TURNSTILE_SITE_KEY", ""),
		TurnstileSecret:      env("TURNSTILE_SECRET", ""),
		SessionSecret:        env("SESSION_SECRET", ""),
	}
}

func env(k, def string) string {
	if v := strings.Trim(strings.TrimSpace(os.Getenv(k)), `"'`); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(env(k, "")); err == nil {
		return n
	}
	return def
}

func envBool(k string, def bool) bool {
	if b, err := strconv.ParseBool(env(k, "")); err == nil {
		return b
	}
	return def
}

// loadDotEnv sets variables from a KEY=value file without overriding ones already set.
func loadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, strings.Trim(strings.TrimSpace(v), `"'`))
		}
	}
}
