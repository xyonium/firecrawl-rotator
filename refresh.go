package main

import (
	"net/http"
	"sync"
	"time"
)

// lowRefreshThreshold is the predicted-credit level below which a key is
// refreshed more often (every lowRefreshInterval) instead of only on switch or
// daily. Matches the user's "< 100 -> every 5 minutes" rule.
const (
	lowRefreshThreshold = 100
	lowRefreshInterval  = 5 * time.Minute
	dailyRefresh        = 24 * time.Hour
)

// Refresher applies the on-demand credit-refresh strategy to a KeyPool:
//   - refresh the key we just switched AWAY from and the one we switched TO
//     (on rotation),
//   - refresh any key whose PREDICTED remaining has dropped below
//     lowRefreshThreshold, throttled to once per lowRefreshInterval,
//   - refresh every key once per day as a catch-all.
// It never blocks the request path: refreshes run in their own goroutine.
type Refresher struct {
	pool      *KeyPool
	client    *http.Client
	cfg       Config
	keys      []string
	log       *logger

	mu             sync.Mutex
	lastLow        []time.Time // last refresh-at when predicted was < lowRefreshThreshold
	lastDaily      []time.Time // last daily refresh-at per key
}

func NewRefresher(pool *KeyPool, client *http.Client, cfg Config, log *logger) *Refresher {
	n := len(pool.Snapshot().Keys)
	return &Refresher{
		pool:     pool,
		client:   client,
		cfg:      cfg,
		keys:     cfg.APIKeys,
		log:      log,
		lastLow:  make([]time.Time, n),
		lastDaily: make([]time.Time, n),
	}
}

// OnSwitch refreshes the key we rotated off (fromIdx) and onto (toIdx) in the
// background. Called by the rotator whenever it advances to a different key.
func (r *Refresher) OnSwitch(fromIdx, toIdx int) {
	if r == nil {
		return
	}
	go func() {
		if fromIdx >= 0 && fromIdx < len(r.keys) {
			r.refreshOne(fromIdx)
		}
		if toIdx >= 0 && toIdx < len(r.keys) && toIdx != fromIdx {
			r.refreshOne(toIdx)
		}
	}()
}

// MaybeRefreshLow refreshes a key in the background if its PREDICTED remaining
// is below lowRefreshThreshold and it hasn't been refreshed in
// lowRefreshInterval. Called by the rotator after each successful response.
func (r *Refresher) MaybeRefreshLow(idx int) {
	if r == nil || idx < 0 || idx >= len(r.keys) {
		return
	}
	// Check predicted balance without holding the refresher lock.
	predicted := r.pool.Snapshot().Keys[idx].RemainingCredits // -1 = unmeasured
	if predicted < 0 || predicted >= lowRefreshThreshold {
		return
	}
	r.mu.Lock()
	last := r.lastLow[idx]
	if !last.IsZero() && time.Since(last) < lowRefreshInterval {
		r.mu.Unlock()
		return
	}
	r.lastLow[idx] = time.Now()
	r.mu.Unlock()

	go r.refreshOne(idx)
}

// DailyRefresh refreshes every key whose last daily refresh is older than
// dailyRefresh (or never). Intended to be called by a periodic ticker.
func (r *Refresher) DailyRefresh() {
	if r == nil {
		return
	}
	now := time.Now()
	for i := range r.keys {
		r.mu.Lock()
		last := r.lastDaily[i]
		if !last.IsZero() && now.Sub(last) < dailyRefresh {
			r.mu.Unlock()
			continue
		}
		r.lastDaily[i] = now
		r.mu.Unlock()
		go r.refreshOne(i)
	}
}

// RefreshAll force-refreshes every key now (used at startup warm-up).
func (r *Refresher) RefreshAll() {
	if r == nil {
		return
	}
	var wg sync.WaitGroup
	for i := range r.keys {
		wg.Add(1)
		r.mu.Lock()
		r.lastDaily[i] = time.Now()
		r.mu.Unlock()
		go func(i int) {
			defer wg.Done()
			r.refreshOne(i)
		}(i)
	}
	wg.Wait()
}

// refreshOne fetches one key's live usage and applies it. Updates lastDaily so
// the daily loop treats it as freshly refreshed.
func (r *Refresher) refreshOne(idx int) {
	if idx < 0 || idx >= len(r.keys) {
		return
	}
	got := refreshKey(r.pool, r.client, r.cfg, idx, r.keys[idx])
	if got >= 0 {
		r.log.debug("refreshed credits", "key", idx, "remaining", got)
		r.mu.Lock()
		r.lastDaily[idx] = time.Now()
		r.mu.Unlock()
	}
}
