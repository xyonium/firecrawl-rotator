package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// usage holds a key's live credit balance and billing reset instant, both read
// from one GET /v2/team/credit-usage call. ok is false when the call failed or
// the response was unparseable.
type usage struct {
	remaining   int64
	periodEnd   time.Time
	ok          bool
}

// fetchUsage queries a key's own billing period: remaining credits and reset
// instant. The /v2/team/credit-usage endpoint is read-only and consumes no
// credits, so it works even after the key is exhausted. Returns ok=false when
// the call fails; callers apply a fallback.
func fetchUsage(client *http.Client, upstream, key string) usage {
	const timeout = 5 * time.Second
	c := client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	} else {
		c = &http.Client{Transport: c.Transport, Timeout: timeout}
	}
	req, err := http.NewRequest(http.MethodGet, upstream+"/v2/team/credit-usage", nil)
	if err != nil {
		return usage{}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return usage{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return usage{}
	}
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			RemainingCredits int64  `json:"remainingCredits"`
			BillingPeriodEnd string `json:"billingPeriodEnd"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return usage{}
	}
	u := usage{remaining: env.Data.RemainingCredits, ok: true}
	if env.Data.BillingPeriodEnd != "" {
		if t, err := time.Parse(time.RFC3339, env.Data.BillingPeriodEnd); err == nil {
			u.periodEnd = t
		}
	}
	return u
}

// refreshKey fetches a key's live usage and applies it to the pool: sets the
// remaining credits, and (on a real reset instant) records the reset for the
// re-enable loop. Returns the fetched remaining credits (-1 if the call failed).
func refreshKey(pool *KeyPool, client *http.Client, cfg Config, index int, key string) int64 {
	u := fetchUsage(client, cfg.Upstream, key)
	if !u.ok {
		return -1
	}
	pool.SetCredits(index, u.remaining)
	return u.remaining
}

// disableUntilReset disables key index in the pool, first trying to read the
// key's real billing-period end (so accounts on different anniversaries reset
// independently) and falling back to the configured reset day-of-month.
func disableUntilReset(pool *KeyPool, client *http.Client, cfg Config, index int, key string, now time.Time) {
	fallback := fallbackReset(now, cfg.CreditResetDay)
	u := fetchUsage(client, cfg.Upstream, key)
	reset := fallback
	if u.ok && !u.periodEnd.IsZero() && !u.periodEnd.Before(now) && !u.periodEnd.After(now.AddDate(1, 0, 0)) {
		reset = u.periodEnd
	}
	pool.Disable(index, reset)
}
