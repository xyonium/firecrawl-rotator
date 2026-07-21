package main

import "testing"

func TestShouldRotate_StatusSet(t *testing.T) {
	p := &Profile{Name: "firecrawl"}
	for _, code := range []int{402, 429, 401} {
		ok, _ := p.shouldRotate(code, []byte(`{}`))
		if !ok {
			t.Errorf("status %d: expected rotate, got false", code)
		}
	}
}

func TestShouldRotate_403DoesNotRotate(t *testing.T) {
	p := &Profile{Name: "firecrawl"}
	// 403 is transient (edge/WAF), handled by shouldRetry, NOT shouldRotate.
	if ok, _ := p.shouldRotate(403, []byte(`{}`)); ok {
		t.Error("403 should not rotate (transient, retried via shouldRetry)")
	}
}

func TestShouldRetry_Transient(t *testing.T) {
	for _, code := range []int{403, 408, 500, 502, 503, 504} {
		if !shouldRetry(code, false) {
			t.Errorf("status %d: expected retryable, got false", code)
		}
	}
	if !shouldRetry(0, true) {
		t.Error("network error should be retryable")
	}
}

func TestShouldRetry_NonTransient(t *testing.T) {
	for _, code := range []int{200, 400, 401, 402, 404, 429} {
		if shouldRetry(code, false) {
			t.Errorf("status %d: expected NOT retryable, got true", code)
		}
	}
}

func TestShouldRotate_NegativeStatus(t *testing.T) {
	p := &Profile{Name: "firecrawl"}
	for _, code := range []int{200, 404, 500, 502, 301} {
		ok, _ := p.shouldRotate(code, []byte(`{"success":true}`))
		if ok {
			t.Errorf("status %d: expected no rotate, got true", code)
		}
	}
}

func TestShouldRotate_BodyPatterns(t *testing.T) {
	p := &Profile{Name: "firecrawl"}
	cases := [][]byte{
		[]byte(`{"success":false,"error":"Insufficient credits"}`),
		[]byte(`{"success":false,"error":"You have exceeded your rate limit"}`),
		[]byte(`{"success":false,"message":"Payment required to continue"}`),
		[]byte(`{"success":false,"error":"Unauthorized access"}`),
		[]byte(`{"success":false,"error":"forbidden"}`),
		[]byte(`{"success":false,"error":"rate limit reached"}`),
	}
	for _, body := range cases {
		// 200 + success:false body that matches -> rotate
		ok, reason := p.shouldRotate(200, body)
		if !ok {
			t.Errorf("body %q: expected rotate, got false", body)
		}
		if reason == "" {
			t.Errorf("body %q: expected non-empty reason", body)
		}
	}
}

func TestShouldRotate_NoMatch(t *testing.T) {
	p := &Profile{Name: "firecrawl"}
	ok, _ := p.shouldRotate(200, []byte(`{"success":true,"data":[]}`))
	if ok {
		t.Error("clean 200 body: expected no rotate")
	}
	ok, _ = p.shouldRotate(404, []byte(`{"error":"not found"}`))
	if ok {
		t.Error("404 not-found body: expected no rotate")
	}
}

// TestShouldRotate_SuccessBodyWithDenylistWords is the regression for the
// production bug: a SUCCESSFUL scrape whose content happens to mention
// "rate limit" / "payment required" / "credits" must NOT be treated as a key
// rejection. Previously the denylist scanned the whole body and rotated on
// good responses, causing duplicate requests and credit burn.
func TestShouldRotate_SuccessBodyWithDenylistWords(t *testing.T) {
	p := &Profile{Name: "firecrawl"}
	cases := [][]byte{
		// success:true envelope, scraped content mentions rejection words
		[]byte(`{"success":true,"data":[{"markdown":"Firecrawl pricing: payment required for the Growth plan. Rate limit is 500/min."}]}`),
		[]byte(`{"success":true,"data":{"markdown":"You have exceeded your plan credits if you upgrade - see payment required."}}`),
		// no success field at all but status 200 and no failure envelope: don't rotate
		[]byte(`{"data":"insufficient credits explained in scraped docs about rate limit"}`),
		[]byte(`<html><body>Unauthorized access is forbidden. 402 payment required.</body></html>`),
	}
	for _, body := range cases {
		ok, _ := p.shouldRotate(200, body)
		if ok {
			t.Errorf("success body with denylist words must NOT rotate; got true for: %s", body)
		}
	}
}

func TestIsCreditExhausted(t *testing.T) {
	p := &Profile{Name: "firecrawl"}
	// 402 -> exhausted (disable)
	if !p.isCreditExhausted(402, []byte(`{}`)) {
		t.Error("402 should be credit-exhausted")
	}
	// 200 + success:false insufficient credits -> exhausted
	if !p.isCreditExhausted(200, []byte(`{"success":false,"error":"Insufficient credits"}`)) {
		t.Error("insufficient credits body should be exhausted")
	}
	// 200 + success:false payment required -> exhausted
	if !p.isCreditExhausted(200, []byte(`{"success":false,"error":"Payment required"}`)) {
		t.Error("payment required body should be exhausted")
	}
	// 429 rate limit -> NOT exhausted (transient)
	if p.isCreditExhausted(429, []byte(`{"success":false,"error":"rate limit"}`)) {
		t.Error("429 rate limit must NOT disable key (transient)")
	}
	// 401/403 auth -> NOT exhausted (global/bad key, not credits)
	if p.isCreditExhausted(401, []byte(`{}`)) {
		t.Error("401 must NOT disable key")
	}
	if p.isCreditExhausted(403, []byte(`{}`)) {
		t.Error("403 must NOT disable key")
	}
	// success:true mentioning credits -> NOT exhausted (scraped content)
	if p.isCreditExhausted(200, []byte(`{"success":true,"data":"payment required credits"}`)) {
		t.Error("success body must never be exhausted")
	}
}
