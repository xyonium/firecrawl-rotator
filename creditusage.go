package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// usage holds a key's live credit balance and billing reset instant, both read
// from one GET /v2/team/credit-usage call. ok is false when the call failed or
// the response was unparseable.
type usage struct {
	remaining int64
	periodEnd time.Time
	ok        bool
}

// usageBackoff is the retry schedule for TRANSIENT credit-usage failures only
// (network errors, 408, 5xx). Permanent failures (404/401/403/400) are not
// retried - they usually mean the key's account can't access this endpoint.
var usageBackoff = []time.Duration{
	500 * time.Millisecond,
	1 * time.Second,
	2 * time.Second,
}

// usageRetryable reports whether a non-200 status is worth retrying.
func usageRetryable(status int) bool {
	switch status {
	case 408, 500, 502, 503, 504:
		return true
	}
	return false
}

// fetchUsage queries a key's own billing period: remaining credits and reset
// instant. The /v2/team/credit-usage endpoint is read-only and consumes no
// credits, so it works even after the key is exhausted. Retries transient
// failures (network, 408, 5xx) with backoff; permanent failures (404/401/403)
// return immediately. log may be nil.
func fetchUsage(client *http.Client, upstream, key string, log *logger) usage {
	const timeout = 5 * time.Second
	c := client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	} else {
		c = &http.Client{Transport: c.Transport, Timeout: timeout}
	}

	var lastReason string
	for attempt := 0; attempt <= len(usageBackoff); attempt++ {
		u, reason := fetchUsageOnce(c, upstream, key)
		if u.ok {
			return u
		}
		lastReason = reason
		// Retry only transient reasons. fetchUsageOnce returns reason prefixed
		// "status:" for non-200 and "net:" for network errors; we retry those
		// (status that is retryable, or any net error) but not permanent status.
		if !shouldRetryUsage(reason) || attempt >= len(usageBackoff) {
			break
		}
		time.Sleep(usageBackoff[attempt])
	}
	if log != nil {
		log.warn("credit-usage fetch failed", "reason", lastReason, "masked", maskKey(key))
	}
	return usage{}
}

// shouldRetryUsage decides whether to retry based on the reason string emitted
// by fetchUsageOnce. "net:" always retries; "status:N" retries only for
// transient N (408/5xx); parse errors and permanent statuses don't.
func shouldRetryUsage(reason string) bool {
	if strings.HasPrefix(reason, "net:") {
		return true
	}
	if strings.HasPrefix(reason, "status:") {
		code, err := strconv.Atoi(strings.TrimPrefix(reason, "status:"))
		if err != nil {
			return false
		}
		return usageRetryable(code)
	}
	return false
}

// fetchUsageOnce performs a single credit-usage request. Returns the usage and
// a short reason string on failure ("net:...", "status:N", "parse:...",
// "nobody").
func fetchUsageOnce(c *http.Client, upstream, key string) (usage, string) {
	req, err := http.NewRequest(http.MethodGet, upstream+"/v2/team/credit-usage", nil)
	if err != nil {
		return usage{}, "build:" + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return usage{}, "net:" + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return usage{}, "status:" + strconv.Itoa(resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return usage{}, "read:" + err.Error()
	}
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			RemainingCredits int64  `json:"remainingCredits"`
			BillingPeriodEnd string `json:"billingPeriodEnd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return usage{}, "parse:" + err.Error()
	}
	u := usage{remaining: env.Data.RemainingCredits, ok: true}
	if env.Data.BillingPeriodEnd != "" {
		if t, err := time.Parse(time.RFC3339, env.Data.BillingPeriodEnd); err == nil {
			u.periodEnd = t
		}
	}
	return u, ""
}

// refreshKey fetches a key's live usage and applies it to the pool: sets the
// remaining credits, and (on a real reset instant) records the reset for the
// re-enable loop. Returns the fetched remaining credits (-1 if the call failed).
func refreshKey(pool *KeyPool, client *http.Client, cfg Config, index int, key string, log *logger) int64 {
	u := fetchUsage(client, cfg.Upstream, key, log)
	if !u.ok {
		return -1
	}
	pool.SetCredits(index, u.remaining)
	return u.remaining
}

// disableUntilReset disables key index in the pool, first trying to read the
// key's real billing-period end (so accounts on different anniversaries reset
// independently) and falling back to the configured reset day-of-month.
func disableUntilReset(pool *KeyPool, client *http.Client, cfg Config, index int, key string, now time.Time, log *logger) {
	fallback := fallbackReset(now, cfg.CreditResetDay)
	u := fetchUsage(client, cfg.Upstream, key, log)
	reset := fallback
	if u.ok && !u.periodEnd.IsZero() && !u.periodEnd.Before(now) && !u.periodEnd.After(now.AddDate(1, 0, 0)) {
		reset = u.periodEnd
	}
	pool.Disable(index, reset)
}
