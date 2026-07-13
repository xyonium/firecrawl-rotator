package main

import (
	"sync"
	"time"
)

type keyStat struct {
	Success int `json:"success"`
	Pay402  int `json:"402"`
	Rate429 int `json:"429"`
	Auth    int `json:"auth"`
	Retries int `json:"retries"`
}

type keySnapshot struct {
	Index        int       `json:"index"`
	Last4        string    `json:"last4"`
	Stats        keyStat   `json:"stats"`
	Disabled     bool      `json:"disabled"`
	DisabledUntil time.Time `json:"disabledUntil,omitempty"`
}

type PoolSnapshot struct {
	PoolSize     int           `json:"poolSize"`
	CurrentIndex int           `json:"currentIndex"`
	Keys         []keySnapshot `json:"keys"`
}

type KeyPool struct {
	mu     sync.Mutex
	keys   []string
	cursor int
	stats  []keyStat
	// disabled[i] is true when key i is credit-exhausted until resetDay.
	// Rate-limit (429) and auth (401/403) do NOT disable - they are transient
	// or global and disabling would take a good key offline.
	disabled       []bool
	disabledUntil  []time.Time
}

func NewKeyPool(keys []string) *KeyPool {
	return &KeyPool{
		keys:          keys,
		stats:         make([]keyStat, len(keys)),
		disabled:      make([]bool, len(keys)),
		disabledUntil: make([]time.Time, len(keys)),
	}
}

// Current returns the next usable (non-disabled) key starting from the cursor,
// advancing the cursor past any disabled keys. Returns (-1,"") if every key is
// disabled. Like Advance, it locks independently of the upstream call - this
// keeps rotation approximate round-robin and avoids serializing all requests.
func (p *KeyPool) Current() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.currentLocked()
}

func (p *KeyPool) currentLocked() (int, string) {
	n := len(p.keys)
	for i := 0; i < n; i++ {
		idx := (p.cursor + i) % n
		if !p.disabled[idx] {
			p.cursor = idx
			return idx, p.keys[idx]
		}
	}
	return -1, ""
}

// Advance moves the cursor to the next key and returns it, skipping disabled
// keys. Returns (-1,"") if every key is disabled.
func (p *KeyPool) Advance() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cursor = (p.cursor + 1) % len(p.keys)
	return p.currentLocked()
}

func (p *KeyPool) RecordSuccess(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index >= 0 && index < len(p.stats) {
		p.stats[index].Success++
	}
}

func (p *KeyPool) RecordRejection(index int, kind string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	switch kind {
	case "402":
		p.stats[index].Pay402++
	case "429":
		p.stats[index].Rate429++
	case "auth":
		p.stats[index].Auth++
	case "retry":
		p.stats[index].Retries++
	default:
		panic("unknown rejection kind: " + kind)
	}
}

// Disable marks a key as credit-exhausted until t. A disabled key is skipped by
// Current/Advance. Use only for genuine credit exhaustion; never for 429/401/403.
func (p *KeyPool) Disable(index int, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if index < 0 || index >= len(p.keys) {
		return
	}
	p.disabled[index] = true
	p.disabledUntil[index] = until
}

// AnyUsable reports whether at least one key is not disabled. /healthz uses
// this to return 503 when the whole pool is exhausted.
func (p *KeyPool) AnyUsable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, d := range p.disabled {
		if !d {
			return true
		}
	}
	return false
}

// ReenableDue re-enables every disabled key whose disabledUntil is at or before
// now. Returns the count re-enabled. Because each key stores its own reset
// instant (fetched from that account's billing period), keys on different
// billing anniversaries come back online independently.
func (p *KeyPool) ReenableDue(now time.Time) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for i := range p.disabled {
		if p.disabled[i] && !p.disabledUntil[i].IsZero() && !p.disabledUntil[i].After(now) {
			p.disabled[i] = false
			p.disabledUntil[i] = time.Time{}
			n++
		}
	}
	return n
}

func maskKey(k string) string {
	if len(k) <= 4 {
		return k
	}
	return k[len(k)-4:]
}

func (p *KeyPool) Snapshot() PoolSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	keys := make([]keySnapshot, len(p.keys))
	for i, k := range p.keys {
		ks := keySnapshot{Index: i, Last4: maskKey(k), Stats: p.stats[i], Disabled: p.disabled[i]}
		if p.disabled[i] {
			ks.DisabledUntil = p.disabledUntil[i]
		}
		keys[i] = ks
	}
	return PoolSnapshot{PoolSize: len(p.keys), CurrentIndex: p.cursor, Keys: keys}
}
