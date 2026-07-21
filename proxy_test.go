package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// testRotator builds a rotator with a single firecrawl profile from cfg, with
// credit thresholds set and a nil refresher.
func testRotator(cfg Config, pool *KeyPool, client *http.Client) *rotator {
	pool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	p := &Profile{
		Name:           "firecrawl",
		Upstream:       cfg.Upstream,
		UpstreamHost:   cfg.UpstreamHost,
		CreditResetDay: cfg.CreditResetDay,
		RewriteNext:    true,
		pool:           pool,
	}
	return newRotator(cfg, []*Profile{p}, client, newLogger("info"))
}

// fakeBackend returns 402 for key "fc-bad" and 200 for "fc-good".
func newFakeBackend(t *testing.T, badKey, goodKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch auth {
		case "Bearer " + badKey:
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"success":false,"error":"Insufficient credits"}`))
		case "Bearer " + goodKey:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			next := "https://" + r.Host + "/v2/x/next"
			_, _ = w.Write([]byte(`{"success":true,"data":[],"next":"` + next + `"}`))
		default:
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
		}
	}))
}

func cfgFor(fake *httptest.Server) Config {
	return Config{
		APIKeys:            []string{"fc-a", "fc-b"},
		Upstream:           fake.URL,
		UpstreamHost:       httptestURLHost(fake.URL),
		MaxPasses:          2,
		MaxBodyBytes:       16 * 1024 * 1024,
		ProxyBaseURL:       "http://rotator.test",
		LowCreditThreshold: 10,
		StopCreditThreshold: 2,
	}
}

// withCredits sets every key's remainingCredits to "plenty" so selection works.
func withCredits(pool *KeyPool) {
	for i := range pool.Snapshot().Keys {
		pool.SetCredits(i, 1000)
	}
}

func TestRotator_RotatesOn402(t *testing.T) {
	fake := newFakeBackend(t, "fc-bad", "fc-good")
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-bad", "fc-good"}
	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{"query":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (should have rotated to good key)", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !bytes.Contains(body, []byte("http://rotator.test/v2/x/next")) {
		t.Fatalf("expected next URL rewritten to proxy base; got %s", body)
	}
}

func TestRotator_AllFailCap(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":"Insufficient credits"}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Both keys get credit-disabled, so 503.
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (all keys credit-disabled)", rec.Code)
	}
}

func TestRotator_5xxNoRotateButRetryThen502(t *testing.T) {
	// 502 is transient -> backoff retries same key, then rotates; with only bad
	// keys it exhausts and returns the last status (502) verbatim.
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	// Make backoff fast so the test doesn't sleep ~15s: override the schedule.
	orig := backoffSchedule
	backoffSchedule = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { backoffSchedule = orig }()

	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502 (last transient status surfaced)", rec.Code)
	}
}

func TestRotator_BodyCapPassthrough(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 1000)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402) // would normally rotate
		_, _ = w.Write(big)
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.MaxBodyBytes = 10
	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 402 {
		t.Fatalf("status = %d, want 402 (over-cap passed through)", rec.Code)
	}
}

func TestRotator_SuccessNoRotate(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{"query":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestRotator_RotatesOn429(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch auth {
		case "Bearer fc-a":
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"error":"Rate limited"}`))
		case "Bearer fc-b":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
		default:
			w.WriteHeader(401)
		}
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{"query":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (should have rotated to good key)", rec.Code)
	}
}

func TestRotator_RotatesOnBodyDenylist(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch auth {
		case "Bearer fc-a":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"success":false,"error":"Insufficient credits"}`))
		case "Bearer fc-b":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"success":true}`))
		default:
			w.WriteHeader(401)
		}
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{"query":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (should have rotated on body denylist match)", rec.Code)
	}
}

func TestRotator_NextRewriteUsesRequestHost(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"next":"http://` + r.Host + `/v2/x/next"}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a"}
	cfg.ProxyBaseURL = "" // must derive from req.Host
	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	req.Host = "rotator.example:9999"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !bytes.Contains(body, []byte("http://rotator.example:9999/v2/x/next")) {
		t.Fatalf("expected next rewritten to req.Host; got %s", body)
	}
	if bytes.Contains(body, []byte(httptestURLHost(fake.URL)+"/v2/x/next")) {
		t.Fatalf("next URL was NOT rewritten - still points at upstream host; got %s", body)
	}
}

// TestRotator_SuccessBodyWithDenylistWords: a successful scrape whose content
// mentions "rate limit"/"payment required" must NOT rotate (the original bug).
func TestRotator_SuccessBodyWithDenylistWords(t *testing.T) {
	hits := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"data":[{"markdown":"payment required. Rate limit. Insufficient credits."}]}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a", "fc-b", "fc-c"}
	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/scrape", bytes.NewReader([]byte(`{"url":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (success body must not rotate)", rec.Code)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no rotation/retry on success)", hits)
	}
}

// TestRotator_402DisablesKey: a genuine 402 disables the key and rotates; once
// all keys are disabled the pool returns 503.
func TestRotator_402DisablesKey(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"success":false,"error":"Insufficient credits"}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (all keys credit-disabled)", rec.Code)
	}
}

// TestRotator_403BacksOffThenRotates: a 403 is transient, retried with backoff
// on the same key, and (when all keys 403) surfaces the last status verbatim.
// Crucially, a 403 must NOT disable the key (it is not a credit problem).
func TestRotator_403BacksOffThenSurfaces(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	orig := backoffSchedule
	backoffSchedule = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { backoffSchedule = orig }()

	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// 403 is transient -> after backoff+rotation exhaustion, last 403 surfaced.
	if rec.Code != 403 {
		t.Fatalf("status = %d, want 403 (transient, surfaced after backoff)", rec.Code)
	}
	// No key should be disabled by a 403.
	snap := pool.Snapshot()
	for i, k := range snap.Keys {
		if k.Disabled {
			t.Fatalf("key %d was disabled by a 403 (must not be)", i)
		}
	}
}

// TestRotator_403RecoversOnRetry: a 403 that succeeds on retry returns 200 and
// does NOT rotate to another key (same key, backoff succeeded).
func TestRotator_403RecoversOnRetry(t *testing.T) {
	hits := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits <= 2 {
			w.WriteHeader(403) // first two attempts 403
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a"} // single key: must recover on SAME key
	orig := backoffSchedule
	backoffSchedule = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}
	defer func() { backoffSchedule = orig }()

	pool := NewKeyPool(cfg.APIKeys)
	withCredits(pool)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/scrape", bytes.NewReader([]byte(`{"url":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (should recover after backoff retry)", rec.Code)
	}
	if hits != 3 {
		t.Fatalf("upstream hits = %d, want 3 (two 403s then success on same key)", hits)
	}
}

// TestRotator_PicksHighestCreditKey: the key with the most credits is used.
func TestRotator_PicksHighestCreditKey(t *testing.T) {
	seen := make(map[string]int)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen[r.Header.Get("Authorization")]++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a", "fc-b", "fc-c"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetThresholds(10, 2)
	pool.SetCredits(0, 50)
	pool.SetCredits(1, 200) // highest
	pool.SetCredits(2, 30)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/scrape", bytes.NewReader([]byte(`{"url":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if seen["Bearer fc-b"] != 1 || len(seen) != 1 {
		t.Fatalf("expected only fc-b (highest credits) used; got %v", seen)
	}
}

// TestRotator_StopsBelowThreshold: when every key is below the stop threshold,
// the request is refused with 503 without calling upstream.
func TestRotator_StopsBelowThreshold(t *testing.T) {
	hits := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(200)
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetThresholds(10, 2)
	pool.SetCredits(0, 1) // below stop=2
	pool.SetCredits(1, 0)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/scrape", bytes.NewReader([]byte(`{"url":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (all below stop threshold)", rec.Code)
	}
	if hits != 0 {
		t.Fatalf("upstream hits = %d, want 0 (must not call upstream when stopped)", hits)
	}
}

// TestRotator_DecrementsCreditsOnSuccess: a success with creditsUsed decrements
// the key's predicted balance.
func TestRotator_DecrementsCreditsOnSuccess(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"creditsUsed":3}`))
	}))
	defer fake.Close()

	cfg := cfgFor(fake)
	cfg.APIKeys = []string{"fc-a"}
	pool := NewKeyPool(cfg.APIKeys)
	pool.SetThresholds(10, 2)
	pool.SetCredits(0, 100)
	r := testRotator(cfg, pool, &http.Client{})

	req := httptest.NewRequest("POST", "/v2/scrape", bytes.NewReader([]byte(`{"url":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rc := pool.Snapshot().Keys[0].RemainingCredits; rc != 97 {
		t.Fatalf("remaining = %d, want 97 (100 - creditsUsed 3)", rc)
	}
}

// httptestURLHost extracts host:port from a httptest.URL for upstreamHost matching.
func httptestURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}
