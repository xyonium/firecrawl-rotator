package main

import (
	"encoding/json"
	"net/http"
	"time"
)

// fetchReset queries a key's own billing period to find when its credits reset.
// The /v2/team/credit-usage endpoint is read-only and consumes no credits, so
// it works even after the key is exhausted. Returns the fallback instant when
// the call fails or the response is unparseable.
func fetchReset(client *http.Client, upstream, key string, fallback time.Time) time.Time {
	const timeout = 5 * time.Second
	c := client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	} else {
		c = &http.Client{Transport: c.Transport, Timeout: timeout}
	}
	req, err := http.NewRequest(http.MethodGet, upstream+"/v2/team/credit-usage", nil)
	if err != nil {
		return fallback
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return fallback
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fallback
	}
	var env struct {
		Success bool `json:"success"`
		Data    struct {
			BillingPeriodEnd string `json:"billingPeriodEnd"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fallback
	}
	if env.Data.BillingPeriodEnd == "" {
		return fallback
	}
	t, err := time.Parse(time.RFC3339, env.Data.BillingPeriodEnd)
	if err != nil {
		return fallback
	}
	return t
}

// disableUntilReset disables key index in the pool, first trying to read the
// key's real billing-period end (so accounts on different anniversaries reset
// independently) and falling back to the configured reset day-of-month.
func disableUntilReset(pool *KeyPool, client *http.Client, cfg Config, index int, key string, now time.Time) {
	fallback := fallbackReset(now, cfg.CreditResetDay)
	reset := fetchReset(client, cfg.Upstream, key, fallback)
	// Sanity: if the fetched end is in the past or implausibly far, use fallback.
	if reset.Before(now) || reset.After(now.AddDate(1, 0, 0)) {
		reset = fallback
	}
	pool.Disable(index, reset)
}
