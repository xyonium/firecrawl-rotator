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

	if rec.Code != 402 {
		t.Fatalf("status = %d, want 402 (last error passed through)", rec.Code)
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

// httptestURLHost extracts host:port from a httptest.URL for upstreamHost matching.
func httptestURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}
