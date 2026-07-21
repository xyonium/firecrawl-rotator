package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestHealthz_OK(t *testing.T) {
	pool := NewKeyPool([]string{"fc-a"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	healthzHandler(pool)(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
}

func TestHealthz_UnhealthyEmptyPool(t *testing.T) {
	pool := NewKeyPool(nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/healthz", nil)
	healthzHandler(pool)(rec, req)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestStatus_ShapeAndMasking(t *testing.T) {
	pool := NewKeyPool([]string{"fc-abcdef1234"})
	pool.RecordRejection(0, "exhausted")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/status", nil)
	statusHandler(pool)(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var snap PoolSnapshot
	if err := json.NewDecoder(rec.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.PoolSize != 1 {
		t.Fatalf("PoolSize = %d, want 1", snap.PoolSize)
	}
	if snap.Keys[0].Last4 != "1234" {
		t.Fatalf("Last4 = %q, want 1234", snap.Keys[0].Last4)
	}
	if snap.Keys[0].Stats.Exhausted != 1 {
		t.Fatalf("Pay402 = %d, want 1", snap.Keys[0].Stats.Exhausted)
	}
}
