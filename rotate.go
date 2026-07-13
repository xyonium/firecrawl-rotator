package main

import (
	"encoding/json"
	"regexp"
	"strconv"
)

// rejectDenylist matches error phrases Firecrawl puts in a FAILED response's
// error/message fields (the envelope), never in scraped content. It is only
// consulted after we have confirmed the response is a Firecrawl failure
// envelope (success:false, or a 4xx status) - see shouldRotate.
var rejectDenylist = regexp.MustCompile(`(?i)(insufficient credits?|rate limit|exceeded|payment required|unauthorized|forbidden)`)

// firecrawlFailure reports whether body is a Firecrawl error envelope:
// {"success":false, ...} OR a non-success status. A {"success":true,...}
// response is NEVER a failure, even if its scraped data mentions "rate limit"
// or "payment required" - those are real scraped words, not key rejections.
func firecrawlFailure(status int, body []byte) bool {
	if status >= 400 && status < 500 {
		return true // 4xx: auth/credits/rate-limit envelope
	}
	var env struct {
		Success *bool `json:"success"`
	}
	if err := json.Unmarshal(body, &env); err == nil && env.Success != nil {
		return !*env.Success
	}
	return false
}

// shouldRotate returns true and a short reason if the response signals a
// key-level rejection worth rotating on. Crucially, a successful (status<400,
// success:true) response NEVER rotates, even if the scraped content happens to
// contain words like "rate limit" or "payment required".
func shouldRotate(status int, body []byte) (bool, string) {
	// Hard status codes that always mean "try another key".
	switch status {
	case 402, 429, 401, 403:
		return true, "status " + strconv.Itoa(status)
	}
	// Otherwise only treat it as a rejection if it is a Firecrawl failure
	// envelope AND the error text matches a known rejection phrase.
	if !firecrawlFailure(status, body) {
		return false, ""
	}
	if m := rejectDenylist.Find(body); m != nil {
		return true, "body:" + string(m)
	}
	return false, ""
}

// isCreditExhausted reports whether a rejection means the key's credits are
// genuinely gone until the billing cycle resets. Only true credit-exhaustion
// signals disable a key; rate-limit (429) and auth (401/403) are transient or
// global and must NOT disable (they'd take a good key offline).
func isCreditExhausted(status int, body []byte) bool {
	if status == 402 {
		return true
	}
	// 200 + success:false envelope whose error mentions credits/payment.
	if firecrawlFailure(status, body) {
		if m := regexp.MustCompile(`(?i)(insufficient credits?|payment required|exceeded)`).Find(body); m != nil {
			return true
		}
	}
	return false
}
