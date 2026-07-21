package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHealthz_OK(t *testing.T) {
	pool := NewKeyPool([]string{"fc-a"})
	profiles := []*Profile{{Name: "firecrawl", pool: pool}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	healthzHandler(profiles)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealthz_UnhealthyEmptyPool(t *testing.T) {
	pool := NewKeyPool(nil)
	profiles := []*Profile{{Name: "firecrawl", pool: pool}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	healthzHandler(profiles)(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestStatus_ShapeAndMasking(t *testing.T) {
	pool := NewKeyPool([]string{"fc-abcdef1234"})
	pool.RecordRejection(0, "exhausted")
	profiles := []*Profile{{Name: "firecrawl", pool: pool}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	statusHandler(profiles)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var body struct {
		Profiles map[string]PoolSnapshot `json:"profiles"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	fc, ok := body.Profiles["firecrawl"]
	if !ok {
		t.Fatal("profiles missing firecrawl key")
	}
	if fc.PoolSize != 1 {
		t.Fatalf("PoolSize = %d, want 1", fc.PoolSize)
	}
	if fc.Keys[0].Last4 != "1234" {
		t.Fatalf("Last4 = %q, want 1234", fc.Keys[0].Last4)
	}
	if fc.Keys[0].Stats.Exhausted != 1 {
		t.Fatalf("Exhausted = %d, want 1", fc.Keys[0].Stats.Exhausted)
	}
}

func TestHealthz_anyProfileUsable(t *testing.T) {
	exhausted := NewKeyPool([]string{"fc-a"})
	exhausted.SetThresholds(10, 2)
	exhausted.SetCredits(0, 0) // below stop threshold
	healthy := NewKeyPool([]string{"tvly-a"})
	healthy.SetThresholds(10, 2)
	healthy.SetCredits(0, 50)

	profiles := []*Profile{
		{Name: "firecrawl", pool: exhausted},
		{Name: "tavily", pool: healthy},
	}
	rec := httptest.NewRecorder()
	healthzHandler(profiles)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz = %d, want 200 (tavily still usable)", rec.Code)
	}

	// Both exhausted -> 503.
	healthy.SetCredits(0, 0)
	rec = httptest.NewRecorder()
	healthzHandler(profiles)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 503 {
		t.Fatalf("healthz = %d, want 503 (all profiles exhausted)", rec.Code)
	}
}

func TestStatus_multiProfile(t *testing.T) {
	fc := NewKeyPool([]string{"fc-a"})
	fc.SetThresholds(10, 2)
	tv := NewKeyPool([]string{"tvly-a", "tvly-b"})
	tv.SetThresholds(10, 2)
	profiles := []*Profile{
		{Name: "firecrawl", pool: fc},
		{Name: "tavily", pool: tv},
	}
	rec := httptest.NewRecorder()
	statusHandler(profiles)(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Profiles map[string]PoolSnapshot `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Profiles["firecrawl"].PoolSize != 1 {
		t.Fatalf("firecrawl poolSize = %d, want 1", body.Profiles["firecrawl"].PoolSize)
	}
	if body.Profiles["tavily"].PoolSize != 2 {
		t.Fatalf("tavily poolSize = %d, want 2", body.Profiles["tavily"].PoolSize)
	}
}
