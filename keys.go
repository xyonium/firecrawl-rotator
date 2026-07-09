package main

import "sync"

type keyStat struct {
	Success int `json:"success"`
	Pay402  int `json:"402"`
	Rate429 int `json:"429"`
	Auth    int `json:"auth"`
	Retries int `json:"retries"`
}

type keySnapshot struct {
	Index int     `json:"index"`
	Last4 string  `json:"last4"`
	Stats keyStat `json:"stats"`
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
}

func NewKeyPool(keys []string) *KeyPool {
	return &KeyPool{
		keys:  keys,
		stats: make([]keyStat, len(keys)),
	}
}

func (p *KeyPool) Current() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cursor, p.keys[p.cursor]
}

func (p *KeyPool) Advance() (int, string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cursor = (p.cursor + 1) % len(p.keys)
	return p.cursor, p.keys[p.cursor]
}

func (p *KeyPool) RecordSuccess(index int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stats[index].Success++
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
		keys[i] = keySnapshot{Index: i, Last4: maskKey(k), Stats: p.stats[i]}
	}
	return PoolSnapshot{PoolSize: len(p.keys), CurrentIndex: p.cursor, Keys: keys}
}
