package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestDowseThroughTheAPI(t *testing.T) {
	var gotAuth, gotDepth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if d, ok := body["search_depth"].(string); ok {
			gotDepth = d
		}
		switch r.URL.Path {
		case "/v1/search":
			w.Write([]byte(`{"results":[{"title":"A","url":"https://a.example/","content":"snippet"}],"usage":{"credits":4}}`))
		case "/v1/extract":
			w.Write([]byte(`{"results":[{"content":"page text"}],"usage":{"credits":2}}`))
		}
	}))
	defer srv.Close()

	rs, cost, err := compareDowseSearch(context.Background(), srv.Client(), srv.URL, "key-1", "advanced", "q", 10)
	if err != nil || len(rs) != 1 || rs[0].URL != "https://a.example/" {
		t.Fatalf("search: %v %+v", err, rs)
	}
	if gotAuth != "Bearer key-1" || gotDepth != "advanced" || gotPath != "/v1/search" {
		t.Fatalf("request: auth %q depth %q path %q", gotAuth, gotDepth, gotPath)
	}
	if math.Abs(cost-4*10.0/13000) > 1e-9 {
		t.Fatalf("cost %v, want 4 credits at the $10 pack", cost)
	}

	o, err := extractDowse(context.Background(), srv.Client(), srv.URL, "key-1", "https://a.example/")
	if err != nil || o.Text != "page text" || math.Abs(o.CostUSD-2*10.0/13000) > 1e-9 {
		t.Fatalf("extract: %v %+v", err, o)
	}
}

func TestDowseOutOfCreditMovesToNextKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer empty" {
			w.WriteHeader(http.StatusPaymentRequired)
			w.Write([]byte(`{"error":"insufficient credits"}`))
			return
		}
		w.Write([]byte(`{"results":[{"title":"A","url":"https://a.example/"}],"usage":{"credits":2}}`))
	}))
	defer srv.Close()
	pools := map[string]*compareKeyPool{"dowse": newCompareKeyPool("dowse", []string{"empty", "full"})}
	ps := compareProviders(&Config{DowseAPIBase: srv.URL}, srv.Client(), pools)
	rs, _, err := ps[0].Search(context.Background(), "q", 10)
	if err != nil || len(rs) != 1 {
		t.Fatalf("second key should answer: %v %+v", err, rs)
	}
}

func TestParallelAndKeenableParsing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch {
		case r.Header.Get("x-api-key") == "pk":
			if body["objective"] != "q" || len(body["search_queries"].([]any)) != 1 {
				t.Errorf("parallel body %v", body)
			}
			w.Write([]byte(`{"results":[{"title":"P","url":"https://p.example/","excerpts":["one","two"]}]}`))
		case r.Header.Get("X-API-Key") == "kk":
			w.Write([]byte(`{"results":[{"title":"K","url":"https://k.example/","snippet":"","description":"desc"}]}`))
		}
	}))
	defer srv.Close()
	tr := &http.Client{Transport: rewrite{srv.URL}}

	rs, cost, err := compareParallel(context.Background(), tr, "pk", "q", 10)
	if err != nil || len(rs) != 1 || rs[0].Content != "one two" || cost != priceParallelAdvanced {
		t.Fatalf("parallel: %v %+v %v", err, rs, cost)
	}
	rs, cost, err = compareKeenable(context.Background(), tr, "kk", "q", 10)
	if err != nil || len(rs) != 1 || rs[0].Content != "desc" || cost != priceKeenablePro {
		t.Fatalf("keenable: %v %+v %v", err, rs, cost)
	}
}

// rewrite sends every request to the test server, keeping the path.
type rewrite struct{ base string }

func (r rewrite) RoundTrip(req *http.Request) (*http.Response, error) {
	u, _ := url.Parse(r.base + req.URL.Path)
	req.URL = u
	req.Host = u.Host
	return http.DefaultTransport.RoundTrip(req)
}
