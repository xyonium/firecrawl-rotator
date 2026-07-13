package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

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
			// next URL points to this server so rewriteNext can match upstreamHost
			next := "https://" + r.Host + "/v2/x/next"
			_, _ = w.Write([]byte(`{"success":true,"data":[],"next":"` + next + `"}`))
		default:
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
		}
	}))
}

func TestRotator_RotatesOn402(t *testing.T) {
	fake := newFakeBackend(t, "fc-bad", "fc-good")
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-bad", "fc-good"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

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
	if i, _ := pool.Current(); i != 1 {
		t.Fatalf("cursor = %d, want 1 after rotating off bad key", i)
	}
}

func TestRotator_AllFailCap(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":"Insufficient credits"}`))
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a", "fc-b"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// Both keys get credit-disabled, so the pool reports no usable key -> 503.
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (all keys credit-disabled)", rec.Code)
	}
	if pool.AnyUsable() {
		t.Fatal("expected all keys disabled after 402 from every key")
	}
}

func TestRotator_5xxNoRotate(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a", "fc-b"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if i, _ := pool.Current(); i != 0 {
		t.Fatalf("cursor = %d, want 0 (5xx must not rotate)", i)
	}
}

func TestRotator_BodyCapPassthrough(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 1000)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402) // would normally rotate
		_, _ = w.Write(big)
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a", "fc-b"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 10, // tiny cap: 1000-byte body exceeds it
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if i, _ := pool.Current(); i != 0 {
		t.Fatalf("cursor = %d, want 0 (over-cap body must not rotate)", i)
	}
}

func TestRotator_SuccessNoRotate(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"data":[]}`))
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a", "fc-b"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{"query":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if i, _ := pool.Current(); i != 0 {
		t.Fatalf("cursor = %d, want 0 (no rotation on success)", i)
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
			_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
		}
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a", "fc-b"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{"query":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (should have rotated to good key)", rec.Code)
	}
	if i, _ := pool.Current(); i != 1 {
		t.Fatalf("cursor = %d, want 1 after rotating off 429'd key", i)
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
			_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
		}
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a", "fc-b"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{"query":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (should have rotated on body denylist match)", rec.Code)
	}
	if i, _ := pool.Current(); i != 1 {
		t.Fatalf("cursor = %d, want 1 after rotating off body-denylisted key", i)
	}
}

func TestRotator_NextRewriteUsesRequestHost(t *testing.T) {
	// Backend returns a next URL pointing to itself (== upstreamHost).
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"next":"http://` + r.Host + `/v2/x/next"}`))
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "", // <-- DEFAULT: unset, must derive from req.Host
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	req.Host = "rotator.example:9999" // the address the caller used
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	// The next URL must be rewritten to use req.Host, NOT the upstream host.
	if !bytes.Contains(body, []byte("http://rotator.example:9999/v2/x/next")) {
		t.Fatalf("expected next rewritten to req.Host 'rotator.example:9999'; got %s", body)
	}
	// And it must NOT contain the upstream host in the next URL.
	if bytes.Contains(body, []byte(httptestURLHost(fake.URL)+"/v2/x/next")) {
		t.Fatalf("next URL was NOT rewritten - still points at upstream host; got %s", body)
	}
}

// TestRotator_SuccessBodyWithDenylistWords is the regression for the
// production bug: a SUCCESSFUL scrape whose content mentions "rate limit" /
// "payment required" / "credits" must NOT be treated as a key rejection. The
// upstream must be hit exactly once (no rotation, no retry), and the good
// response must be passed through. This is the case that caused duplicate
// requests and credit burn in production.
func TestRotator_SuccessBodyWithDenylistWords(t *testing.T) {
	hits := 0
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"success":true,"data":[{"markdown":"Firecrawl pricing page: payment required for Growth. Rate limit 500/min. Insufficient credits on the free plan."}]}`))
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:       []string{"fc-a", "fc-b", "fc-c"},
		Upstream:      fake.URL,
		UpstreamHost:  httptestURLHost(fake.URL),
		MaxPasses:     2,
		MaxBodyBytes:  16 * 1024 * 1024,
		ProxyBaseURL:  "http://rotator.test",
		CreditResetDay: 1,
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/scrape", bytes.NewReader([]byte(`{"url":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (success body must not rotate)", rec.Code)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1 (no rotation/retry on success)", hits)
	}
	if !pool.AnyUsable() {
		t.Fatal("no key should be disabled by a success response")
	}
}

// TestRotator_402DisablesKey: a genuine 402 credit-exhaustion disables the key
// and rotates to the next; once all keys are disabled the pool returns 503.
func TestRotator_402DisablesKey(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"success":false,"error":"Insufficient credits"}`))
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:       []string{"fc-a", "fc-b"},
		Upstream:      fake.URL,
		UpstreamHost:  httptestURLHost(fake.URL),
		MaxPasses:     2,
		MaxBodyBytes:  16 * 1024 * 1024,
		ProxyBaseURL:  "http://rotator.test",
		CreditResetDay: 1,
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (all keys credit-disabled)", rec.Code)
	}
	if pool.AnyUsable() {
		t.Fatal("expected all keys disabled after 402 from every key")
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
