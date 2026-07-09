package main

import "testing"

func TestShouldRotate_StatusSet(t *testing.T) {
	for _, code := range []int{402, 429, 401, 403} {
		ok, _ := shouldRotate(code, []byte(`{}`))
		if !ok {
			t.Errorf("status %d: expected rotate, got false", code)
		}
	}
}

func TestShouldRotate_NegativeStatus(t *testing.T) {
	for _, code := range []int{200, 404, 500, 502, 301} {
		ok, _ := shouldRotate(code, []byte(`{"success":true}`))
		if ok {
			t.Errorf("status %d: expected no rotate, got true", code)
		}
	}
}

func TestShouldRotate_BodyPatterns(t *testing.T) {
	cases := [][]byte{
		[]byte(`{"error":"Insufficient credits"}`),
		[]byte(`{"error":"You have exceeded your rate limit"}`),
		[]byte(`{"message":"Payment required to continue"}`),
		[]byte(`{"error":"Unauthorized access"}`),
		[]byte(`{"error":"forbidden"}`),
		[]byte(`{"success":false,"error":"rate limit reached"}`),
	}
	for _, body := range cases {
		// 200 + success:false body that matches -> rotate
		ok, reason := shouldRotate(200, body)
		if !ok {
			t.Errorf("body %q: expected rotate, got false", body)
		}
		if reason == "" {
			t.Errorf("body %q: expected non-empty reason", body)
		}
	}
}

func TestShouldRotate_NoMatch(t *testing.T) {
	ok, _ := shouldRotate(200, []byte(`{"success":true,"data":[]}`))
	if ok {
		t.Error("clean 200 body: expected no rotate")
	}
	ok, _ = shouldRotate(404, []byte(`{"error":"not found"}`))
	if ok {
		t.Error("404 not-found body: expected no rotate")
	}
}
