package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"dowse-compare/internal/models"
)

func TestCompareKeyPoolMovesToNextKey(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p := newCompareKeyPool("tavily", []string{"k1", "k2", "k3"})
	p.now = func() time.Time { return now }

	answers := map[string]error{
		"k1": &compareHTTPError{Status: 432, Body: "plan limit exceeded"},
		"k2": &compareHTTPError{Status: 429, Body: "too many requests"},
		"k3": nil,
	}
	var used []string
	call := func(key string) error { used = append(used, key); return answers[key] }

	if err := p.do(context.Background(), call); err != nil || strings.Join(used, ",") != "k1,k2,k3" {
		t.Fatalf("err %v, keys tried %v", err, used)
	}
	// k1 rests 6 h, k2 a few seconds: only k3 is used meanwhile.
	used = nil
	if err := p.do(context.Background(), call); err != nil || strings.Join(used, ",") != "k3" {
		t.Fatalf("second call: err %v, keys %v", err, used)
	}
	// After a few seconds k2 is back in the turns; after 6 h k1 is too.
	inTurns := func(key string) bool {
		used = nil
		for i := 0; i < 3; i++ {
			_ = p.do(context.Background(), call)
		}
		return strings.Contains(strings.Join(used, ","), key)
	}
	now = now.Add(10 * time.Second)
	answers["k2"] = nil
	if !inTurns("k2") || inTurns("k1") {
		t.Fatalf("after 10 s k2 should be back and k1 still resting: %v", used)
	}
	now = now.Add(7 * time.Hour)
	answers["k1"] = nil
	if !inTurns("k1") {
		t.Fatalf("out-of-credit key should be tried again after 6 h: %v", used)
	}
}

func TestCompareKeyPoolOtherErrorsDoNotSwitch(t *testing.T) {
	p := newCompareKeyPool("exa", []string{"k1", "k2"})
	var used []string
	err := p.do(context.Background(), func(key string) error { used = append(used, key); return &compareHTTPError{Status: 500, Body: "oops"} })
	if len(used) != 1 || err == nil || errors.Is(err, errCompareNoKeyLeft) {
		t.Fatalf("a server error is not the key's fault: keys %v err %v", used, err)
	}
	used = nil
	_ = p.do(context.Background(), func(key string) error { used = append(used, key); return errors.New("connection reset") })
	if len(used) != 1 {
		t.Fatalf("network errors should not switch keys: %v", used)
	}
}

func TestCompareKeyPoolAllOut(t *testing.T) {
	p := newCompareKeyPool("contextdev", []string{"a", "b"})
	err := p.do(context.Background(), func(string) error { return &compareHTTPError{Status: 401, Body: "USAGE_EXCEEDED"} })
	if !errors.Is(err, errCompareNoKeyLeft) {
		t.Fatalf("want no key left, got %v", err)
	}
	if total, usable := p.status(); total != 2 || usable != 0 {
		t.Fatalf("status %d/%d", usable, total)
	}
	if err := newCompareKeyPool("x", nil).do(context.Background(), func(string) error { return nil }); err != errCompareUnavailable {
		t.Fatalf("no keys at all should be unavailable: %v", err)
	}
}

func TestCompareKeysFromEnv(t *testing.T) {
	t.Setenv("TESTPROV_KEY_2", " two ")
	t.Setenv("TESTPROV_KEY_3", "")
	t.Setenv("TESTPROV_KEY_4", "one") // a repeat of the first
	t.Setenv("TESTPROV_KEY_5", `"five"`)
	got := compareKeysFromEnv("TESTPROV_KEY", "one")
	if strings.Join(got, ",") != "one,two,five" {
		t.Fatalf("keys %v", got)
	}
}

func TestCompareColumnShowsLimitWhenKeysRunOut(t *testing.T) {
	pool := newCompareKeyPool("exa", []string{"only"})
	out := compareProvider{ID: "exa", Name: "Exa", Search: pooledSearch(pool, func(ctx context.Context, key, q string, n int) ([]models.SearchResult, float64, error) {
		return nil, 0, &compareHTTPError{Status: 402, Body: "no credits"}
	})}
	s := newTestCompare(t, &fakeScorer{}, out, staticProvider("a", nil, "https://good.example/1"))
	w := doCompare(s, "q")
	if !strings.Contains(w.Body.String(), `"id":"exa","status":"limit"`) || strings.Contains(w.Body.String(), "no credits") {
		t.Fatalf("want a limit column without the provider's message:\n%s", w.Body.String())
	}
}

func TestCompareKeyPoolWaitsForRateLimit(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	p := newCompareKeyPool("perplexity", []string{"a", "b"})
	p.now = func() time.Time { return now }
	slept := time.Duration(0)
	p.sleep = func(_ context.Context, d time.Duration) bool { slept += d; now = now.Add(d); return true }

	calls := 0
	err := p.do(context.Background(), func(key string) error {
		calls++
		if calls <= 2 { // both keys rate limited once
			return &compareHTTPError{Status: 429, Body: "slow down"}
		}
		return nil
	})
	if err != nil || calls != 3 || slept == 0 {
		t.Fatalf("should wait for a key and succeed: err %v calls %d slept %v", err, calls, slept)
	}

	// Always rate limited: gives up as busy, not out of quota, within the wait budget.
	p2 := newCompareKeyPool("perplexity", []string{"a"})
	p2.now = p.now
	waited := time.Duration(0)
	p2.sleep = func(_ context.Context, d time.Duration) bool { waited += d; now = now.Add(d); return true }
	err = p2.do(context.Background(), func(string) error { return &compareHTTPError{Status: 429} })
	if !errors.Is(err, errCompareBusy) || waited > compareKeyMaxWait+time.Second {
		t.Fatalf("want busy within %v: err %v waited %v", compareKeyMaxWait, err, waited)
	}
}

func TestCompareKeyPoolTakesTurns(t *testing.T) {
	p := newCompareKeyPool("exa", []string{"k1", "k2", "k3"})
	var used []string
	for i := 0; i < 7; i++ {
		_ = p.do(context.Background(), func(key string) error { used = append(used, key); return nil })
	}
	if got := strings.Join(used, ","); got != "k1,k2,k3,k1,k2,k3,k1" {
		t.Fatalf("keys should take turns: %s", got)
	}
	// A resting key is skipped and the turns go on.
	p.rest(1, compareKeyRestQuota)
	used = nil
	for i := 0; i < 4; i++ {
		_ = p.do(context.Background(), func(key string) error { used = append(used, key); return nil })
	}
	if got := strings.Join(used, ","); got != "k3,k1,k3,k1" {
		t.Fatalf("resting k2 should be skipped: %s", got)
	}
}
