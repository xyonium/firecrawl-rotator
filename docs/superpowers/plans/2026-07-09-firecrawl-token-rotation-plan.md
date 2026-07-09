# Firecrawl Token Rotation Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `firecrawl-rotator`, a Go reverse proxy that sits between firecrawl-mcp and `api.firecrawl.dev`, injecting API keys from a pool and rotating on 402/429/401/403 (plus error-body matching), with crawl-`next` URL rewriting so pagination stays under rotation.

**Architecture:** A single Go HTTP server. Each incoming request is forwarded to the upstream with a key from a shared, mutex-guarded pool. Responses are buffered (capped) so the rotation decider can inspect status+body and, on a rejection signal, advance the cursor and retry with the next key (up to `MAX_PASSES` full pool passes). Before returning, a `next`-URL rewriter swaps absolute upstream URLs in `next` fields to point back at the proxy. `/healthz` and `/status` give observability; outbound egress can optionally go through a forward proxy.

**Tech Stack:** Go 1.22+ (stdlib only: `net/http`, `net/http/httputil`, `net/http/httptest`, `net/url`, `golang.org/x/net/http/httpproxy` is NOT needed - use `httpproxy` from `net/http`... see note). Actually: `httpproxy` lives at `golang.org/x/net/http/httpproxy`. To stay stdlib-only, use `http.ProxyFromEnvironment` for the system-var path and a manual `Transport.Proxy` func for `UPSTREAM_PROXY`. No external deps. Testing via `go test` + `httptest.Server`.

**Tooling note for the executor:** Go is **not installed** on the machine where this plan was written (`/home/eli/firecrawl-rotator`). The first task verifies/installs Go. If the executor environment already has `go >= 1.22`, skip the install step. All `go` commands assume the module dir is the cwd.

---

## File Structure

| File | Responsibility |
|------|----------------|
| `go.mod` | Module `firecrawl-rotator`, Go 1.22, no external deps. |
| `config.go` | Env parsing + validation. One `Config` struct, one `LoadConfig() (Config, error)`. |
| `config_test.go` | Tests for env parsing, defaults, required-field errors. |
| `keys.go` | `KeyPool`: load, `current()` with mutex, `advance()`, per-key stats, masking. |
| `keys_test.go` | Cursor advance/wrap, masking, stats bumping. |
| `rotate.go` | `shouldRotate(status int, body []byte) (bool, reason string)` - status set + body regex. |
| `rotate_test.go` | Decider truth table: each status, each body pattern, negatives. |
| `rewrite.go` | `rewriteNext(body []byte, proxyBase, upstreamHost string) ([]byte, bool)` + `paginationGuard(body) bool`. Strict `next`-field scope. |
| `rewrite_test.go` | Rewrite scope, foreign/relative/non-URL, pagination guard terminal vs non-terminal. |
| `proxy.go` | `proxyHandler` - forward, buffer+cap, rotate/retry loop, call rewriter, return. |
| `proxy_test.go` | Integration with `httptest.Server`: rotate-on-402 success, all-fail cap, 5xx-no-rotate, body-cap passthrough. |
| `server.go` | `healthzHandler`, `statusHandler`, structured `log` helper. |
| `server_test.go` | `/healthz` ok/unhealthy, `/status` shape + masking. |
| `main.go` | Wire it all: `LoadConfig`, build `KeyPool` + `http.Client`/`Transport`, register routes, `ListenAndServe`. |
| `main_test.go` | Optional smoke: start server on a random port, hit `/healthz`. |
| `Dockerfile` | Multi-stage: `golang:1.22` build -> `scratch` + binary + ca-certificates. |
| `docker-compose.yml` | Reference wiring: rotator + an mcpo/firecrawl-mcp service pointed at it. |
| `README.md` | What it is, env table, docker-compose snippet, dev commands. |
| `.gitignore` | `rotator` binary, `dist/`. |

**Decomposition rationale:** Each file is one responsibility and independently testable. `rotate.go` and `rewrite.go` are pure functions over bytes - trivially unit-testable with no HTTP. `proxy.go` is the only file that touches real HTTP and is tested via `httptest.Server`. `keys.go` is pure concurrency state. This keeps each file holdable in context at once.

---

## Task 0: Bootstrap repo and Go toolchain

**Files:**
- Create: `go.mod`
- Create: `.gitignore`
- Create: `README.md` (stub, expanded in Task 12)

- [ ] **Step 1: Verify Go is installed (>= 1.22)**

Run: `go version`
Expected: prints `go version go1.22.x` or newer. If `go: command not found`, install Go 1.22+ (e.g. `sudo apt install golang-1.22` or download from go.dev) and re-run. Do not proceed until `go version` succeeds with >= 1.22.

- [ ] **Step 2: Initialize the module**

Run: `go mod init firecrawl-rotator`
Expected: creates `go.mod` containing `module firecrawl-rotator` and `go 1.22` (or newer).

- [ ] **Step 3: Create `.gitignore`**

```text
/rotator
/dist/
*.test
*.out
```

- [ ] **Step 4: Create README stub**

```markdown
# firecrawl-rotator

A reverse proxy in front of firecrawl-mcp that rotates Firecrawl API keys on
credit exhaustion / rate-limit / rejection. See
`docs/superpowers/specs/2026-07-09-firecrawl-token-rotation-design.md` for the
design.

Usage docs added in a later task.
```

- [ ] **Step 5: Commit**

```bash
git add go.mod .gitignore README.md
git commit -m "chore: bootstrap go module"
```

---

## Task 1: Config - env parsing and validation

**Files:**
- Create: `config.go`
- Create: `config_test.go`

**Config struct shape** (defined in `config.go`, referenced by every later task):

```go
package main

type Config struct {
	APIKeys       []string
	Upstream      string // e.g. "https://api.firecrawl.dev"
	UpstreamHost  string // "api.firecrawl.dev" - derived from Upstream
	Port          string
	Host          string
	MaxPasses     int
	MaxBodyBytes  int64 // 0 = no cap
	ProxyBaseURL  string // empty -> derive from Host header at runtime
	UpstreamProxy string // empty -> use system env (HTTPS_PROXY etc.)
	LogLevel      string
}
```

- [ ] **Step 1: Write the failing tests**

`config_test.go`:

```go
package main

import (
	"os"
	"reflect"
	"testing"
)

func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

func TestLoadConfig_defaults(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a, fc-b ,, fc-c")
	for _, k := range []string{"UPSTREAM", "PORT", "HOST", "MAX_PASSES", "MAX_BODY_BYTES", "PROXY_BASE_URL", "UPSTREAM_PROXY", "LOG_LEVEL"} {
		t.Setenv(k, "")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		APIKeys:      []string{"fc-a", "fc-b", "fc-c"}, // trimmed, empties dropped
		Upstream:     "https://api.firecrawl.dev",
		UpstreamHost: "api.firecrawl.dev",
		Port:         "8788",
		Host:         "0.0.0.0",
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		LogLevel:     "info",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadConfig_emptyKeysErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", " , , ")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for empty key pool, got nil")
	}
}

func TestLoadConfig_badUpstreamErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM", "://no-scheme")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for unparseable upstream, got nil")
	}
}

func TestLoadConfig_badUpstreamProxyErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM_PROXY", "://bad")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for unparseable UPSTREAM_PROXY, got nil")
	}
}

func TestLoadConfig_maxBodyBytesZero(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("MAX_BODY_BYTES", "0")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxBodyBytes != 0 {
		t.Fatalf("MaxBodyBytes = %d, want 0", cfg.MaxBodyBytes)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL / build error - `LoadConfig` undefined.

- [ ] **Step 3: Write `config.go`**

```go
package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	APIKeys       []string
	Upstream      string
	UpstreamHost  string
	Port          string
	Host          string
	MaxPasses     int
	MaxBodyBytes  int64
	ProxyBaseURL  string
	UpstreamProxy string
	LogLevel      string
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %w", key, err)
	}
	return n, nil
}

func envInt64(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %w", key, err)
	}
	return n, nil
}

func parseKeys(raw string) []string {
	var out []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func LoadConfig() (Config, error) {
	keys := parseKeys(os.Getenv("FIRECRAWL_API_KEYS"))
	if len(keys) == 0 {
		return Config{}, fmt.Errorf("FIRECRAWL_API_KEYS is required and must contain at least one non-empty key")
	}

	upstream := envStr("UPSTREAM", "https://api.firecrawl.dev")
	u, err := url.Parse(upstream)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Config{}, fmt.Errorf("UPSTREAM %q is not a valid http(s) URL: %w", upstream, err)
	}

	maxPasses, err := envInt("MAX_PASSES", 2)
	if err != nil {
		return Config{}, err
	}
	if maxPasses < 1 {
		return Config{}, fmt.Errorf("MAX_PASSES must be >= 1, got %d", maxPasses)
	}

	maxBody, err := envInt64("MAX_BODY_BYTES", 16*1024*1024)
	if err != nil {
		return Config{}, err
	}
	if maxBody < 0 {
		return Config{}, fmt.Errorf("MAX_BODY_BYTES must be >= 0, got %d", maxBody)
	}

	proxyStr := strings.TrimSpace(os.Getenv("UPSTREAM_PROXY"))
	if proxyStr != "" {
		pu, err := url.Parse(proxyStr)
		if err != nil || pu.Host == "" {
			return Config{}, fmt.Errorf("UPSTREAM_PROXY %q is not a valid proxy URL: %w", proxyStr, err)
		}
		switch pu.Scheme {
		case "http", "https", "socks5":
		default:
			return Config{}, fmt.Errorf("UPSTREAM_PROXY scheme %q not supported (use http/https/socks5)", pu.Scheme)
		}
	}

	return Config{
		APIKeys:       keys,
		Upstream:      strings.TrimRight(upstream, "/"),
		UpstreamHost:  u.Host,
		Port:          envStr("PORT", "8788"),
		Host:          envStr("HOST", "0.0.0.0"),
		MaxPasses:     maxPasses,
		MaxBodyBytes:  maxBody,
		ProxyBaseURL:  strings.TrimSpace(os.Getenv("PROXY_BASE_URL")),
		UpstreamProxy: proxyStr,
		LogLevel:      envStr("LOG_LEVEL", "info"),
	}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS (all config tests green).

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat: config env parsing and validation"
```

---

## Task 2: KeyPool - cursor, advance, stats, masking

**Files:**
- Create: `keys.go`
- Create: `keys_test.go`

**KeyPool API** (used by `proxy.go` and `server.go`):

```go
type keyStat struct {
	Success int
	Pay402  int
	Rate429 int
	Auth    int // 401/403
	Retries int
}

type KeyPool struct { /* unexported fields */ }
func NewKeyPool(keys []string) *KeyPool
func (p *KeyPool) Current() (index int, key string)          // read current, no advance
func (p *KeyPool) Advance() (index int, key string)          // move cursor, return new current
func (p *KeyPool) RecordSuccess(index int)
func (p *KeyPool) RecordRejection(index int, kind string)    // kind: "402"|"429"|"auth"|"retry"
func (p *KeyPool) Snapshot() PoolSnapshot                     // for /status, masked
```

- [ ] **Step 1: Write the failing tests**

`keys_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

func TestKeyPool_CurrentAndAdvance(t *testing.T) {
	p := NewKeyPool([]string{"fc-a", "fc-b", "fc-c"})
	i, k := p.Current()
	if i != 0 || k != "fc-a" {
		t.Fatalf("Current() = (%d,%q), want (0,fc-a)", i, k)
	}
	i, k = p.Advance()
	if i != 1 || k != "fc-b" {
		t.Fatalf("Advance() = (%d,%q), want (1,fc-b)", i, k)
	}
	i, k = p.Advance()
	if i != 2 || k != "fc-c" {
		t.Fatalf("Advance() = (%d,%q), want (2,fc-c)", i, k)
	}
	// wraps mod N
	i, k = p.Advance()
	if i != 0 || k != "fc-a" {
		t.Fatalf("Advance() wrap = (%d,%q), want (0,fc-a)", i, k)
	}
}

func TestKeyPool_StatsAndMasking(t *testing.T) {
	p := NewKeyPool([]string{"fc-abcd1234", "fc-wxyz9876"})
	p.RecordRejection(0, "402")
	p.RecordRejection(0, "402")
	p.RecordRejection(1, "auth")
	p.RecordSuccess(0)

	snap := p.Snapshot()
	if snap.PoolSize != 2 {
		t.Fatalf("PoolSize = %d, want 2", snap.PoolSize)
	}
	if len(snap.Keys) != 2 {
		t.Fatalf("len(Keys) = %d, want 2", len(snap.Keys))
	}
	if snap.Keys[0].Stats.Pay402 != 2 {
		t.Fatalf("key0 Pay402 = %d, want 2", snap.Keys[0].Stats.Pay402)
	}
	if snap.Keys[1].Stats.Auth != 1 {
		t.Fatalf("key1 Auth = %d, want 1", snap.Keys[1].Stats.Auth)
	}
	if snap.Keys[0].Stats.Success != 1 {
		t.Fatalf("key0 Success = %d, want 1", snap.Keys[0].Stats.Success)
	}
	// masking: last 4 only, prefix hidden
	for _, k := range snap.Keys {
		if !strings.HasPrefix(k.Last4, "1234") && !strings.HasPrefix(k.Last4, "9876") {
			// Last4 should be exactly the last 4 chars
		}
		if k.Last4 != "1234" && k.Last4 != "9876" {
			t.Fatalf("Last4 = %q, want last 4 chars only", k.Last4)
		}
	}
}

func TestKeyPool_RecordRejectionBadKindPanics(t *testing.T) {
	p := NewKeyPool([]string{"fc-a"})
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on bad kind")
		}
	}()
	p.RecordRejection(0, "bogus")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL - `NewKeyPool` undefined.

- [ ] **Step 3: Write `keys.go`**

```go
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
	Index int      `json:"index"`
	Last4 string   `json:"last4"`
	Stats keyStat  `json:"stats"`
}

type PoolSnapshot struct {
	PoolSize     int           `json:"poolSize"`
	CurrentIndex int           `json:"currentIndex"`
	Keys         []keySnapshot `json:"keys"`
}

type KeyPool struct {
	mu       sync.Mutex
	keys     []string
	cursor   int
	stats    []keyStat
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add keys.go keys_test.go
git commit -m "feat: key pool with cursor, stats, masking"
```

---

## Task 3: Rotation decider - status set + body regex

**Files:**
- Create: `rotate.go`
- Create: `rotate_test.go`

**Decider API:**

```go
// shouldRotate returns true and a short reason if the response signals a
// key-level rejection worth rotating on. status is the HTTP status code;
// body is the raw (already-read) response body bytes.
func shouldRotate(status int, body []byte) (bool, string)
```

Rotation triggers when status is in {402, 429, 401, 403} OR the body matches the denylist regex (case-insensitive): `insufficient credits?`, `rate limit`, `exceeded`, `payment required`, `unauthorized`, `forbidden`.

- [ ] **Step 1: Write the failing tests**

`rotate_test.go`:

```go
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
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL - `shouldRotate` undefined.

- [ ] **Step 3: Write `rotate.go`**

```go
package main

import (
	"regexp"
	"strconv"
)

var bodyDenylist = regexp.MustCompile(`(?i)(insufficient credits?|rate limit|exceeded|payment required|unauthorized|forbidden)`)

// shouldRotate returns true and a short reason if the response signals a
// key-level rejection worth rotating on.
func shouldRotate(status int, body []byte) (bool, string) {
	switch status {
	case 402, 429, 401, 403:
		return true, "status " + strconv.Itoa(status)
	}
	if m := bodyDenylist.Find(body); m != nil {
		return true, "body:" + string(m)
	}
	return false, ""
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add rotate.go rotate_test.go
git commit -m "feat: rotation decider (status set + body denylist)"
```

---

## Task 4: next-URL rewriter + pagination guard

**Files:**
- Create: `rewrite.go`
- Create: `rewrite_test.go`

**API:**

```go
// rewriteNext rewrites absolute upstream URLs in fields literally named "next"
// to point at proxyBase. It returns the new body and a bool indicating whether
// any rewrite happened. Strict scope: only "next" fields, only absolute URLs
// whose host == upstreamHost.
func rewriteNext(body []byte, proxyBase, upstreamHost string) ([]byte, bool)

// paginationGuard reports whether a response looks like it has more data to
// fetch but carries no "next" key (a sign the pagination field was renamed).
// Returns true to mean "warn". Terminal pages (completed == total, no next)
// return false.
func paginationGuard(body []byte) bool
```

`proxyBase` is the scheme+host of the proxy as callers see it (e.g. `http://firecrawl-rotator:8788`), no trailing slash. The rewriter replaces the host of a matched `next` URL with this base, keeping path+query.

- [ ] **Step 1: Write the failing tests**

`rewrite_test.go`:

```go
package main

import "testing"

func TestRewriteNext_AbsoluteUpstream(t *testing.T) {
	in := []byte(`{"next":"https://api.firecrawl.dev/v2/crawl/abc/next?cursor=2","data":[]}`)
	out, changed := rewriteNext(in, "http://firecrawl-rotator:8788", "api.firecrawl.dev")
	if !changed {
		t.Fatal("expected changed=true")
	}
	want := `{"next":"http://firecrawl-rotator:8788/v2/crawl/abc/next?cursor=2","data":[]}`
	if string(out) != want {
		t.Fatalf("got %s, want %s", out, want)
	}
}

func TestRewriteNext_RelativeLeftAlone(t *testing.T) {
	in := []byte(`{"next":"/v2/crawl/abc/next","data":[]}`)
	out, changed := rewriteNext(in, "http://firecrawl-rotator:8788", "api.firecrawl.dev")
	if changed {
		t.Fatal("expected changed=false for relative next")
	}
	if string(out) != string(in) {
		t.Fatalf("relative next must be untouched, got %s", out)
	}
}

func TestRewriteNext_ForeignHostLeftAlone(t *testing.T) {
	in := []byte(`{"next":"https://example.com/foo","data":[]}`)
	out, changed := rewriteNext(in, "http://firecrawl-rotator:8788", "api.firecrawl.dev")
	if changed {
		t.Fatal("expected changed=false for foreign host")
	}
}

func TestRewriteNext_NonURLValueLeftAlone(t *testing.T) {
	in := []byte(`{"next":null,"data":[]}`)
	_, changed := rewriteNext(in, "http://firecrawl-rotator:8788", "api.firecrawl.dev")
	if changed {
		t.Fatal("expected changed=false for null next")
	}
}

func TestRewriteNext_HostInContentNotRewritten(t *testing.T) {
	// "url" field and scraped markdown mentioning the host must NOT change.
	in := []byte(`{"url":"https://api.firecrawl.dev/page","markdown":"see api.firecrawl.dev for docs"}`)
	out, changed := rewriteNext(in, "http://firecrawl-rotator:8788", "api.firecrawl.dev")
	if changed {
		t.Fatal("expected changed=false: host in non-next fields must not be rewritten")
	}
	if string(out) != string(in) {
		t.Fatalf("content corrupted: %s", out)
	}
}

func TestPaginationGuard_NonTerminalNoNext(t *testing.T) {
	// in-progress, more data, no next -> warn
	body := []byte(`{"status":"scraping","completed":3,"total":10,"data":[]}`)
	if !paginationGuard(body) {
		t.Fatal("expected guard=true (warn) for non-terminal no-next")
	}
}

func TestPaginationGuard_TerminalNoNext(t *testing.T) {
	// completed crawl, no next -> normal end, no warn
	body := []byte(`{"status":"completed","completed":10,"total":10,"data":[]}`)
	if paginationGuard(body) {
		t.Fatal("expected guard=false for terminal page")
	}
}

func TestPaginationGuard_HasNext(t *testing.T) {
	body := []byte(`{"status":"scraping","completed":3,"total":10,"next":"https://api.firecrawl.dev/x","data":[]}`)
	if paginationGuard(body) {
		t.Fatal("expected guard=false when next present")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL - `rewriteNext` / `paginationGuard` undefined.

- [ ] **Step 3: Write `rewrite.go`**

```go
package main

import (
	"encoding/json"
	"net/url"
	"strings"
)

// rewriteNext rewrites absolute upstream URLs in fields literally named "next".
// Strict scope: only "next" keys whose string value is an absolute URL on
// upstreamHost. Returns the (possibly modified) body and whether a change was
// made.
func rewriteNext(body []byte, proxyBase, upstreamHost string) ([]byte, bool) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		// not JSON - leave untouched
		return body, false
	}
	changed := rewriteNextInValue(root, proxyBase, upstreamHost)
	if !changed {
		return body, false
	}
	out, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return out, true
}

// rewriteNextInValue walks the decoded value looking only for object keys named
// "next". Returns true if any rewrite occurred.
func rewriteNextInValue(v any, proxyBase, upstreamHost string) bool {
	switch t := v.(type) {
	case map[string]any:
		changed := false
		for k, val := range t {
			if k == "next" {
				if s, ok := val.(string); ok {
					if nu, ok := rewriteOne(s, proxyBase, upstreamHost); ok {
						t[k] = nu
						changed = true
						continue
					}
				}
			}
			if rewriteNextInValue(val, proxyBase, upstreamHost) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, item := range t {
			if rewriteNextInValue(item, proxyBase, upstreamHost) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func rewriteOne(s, proxyBase, upstreamHost string) (string, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", false
	}
	if u.Host != upstreamHost {
		return "", false
	}
	base, err := url.Parse(proxyBase)
	if err != nil || base.Host == "" {
		return "", false
	}
	u.Scheme = base.Scheme
	u.Host = base.Host
	return u.String(), true
}

// paginationGuard reports whether a response indicates more data to fetch but
// has no "next" key. Terminal pages return false.
func paginationGuard(body []byte) bool {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return false
	}
	if _, hasNext := root["next"]; hasNext {
		return false
	}
	status, _ := root["status"].(string)
	switch status {
	case "completed", "failed", "cancelled":
		return false
	case "":
		// no status field at all - not a crawl-status payload
		return false
	}
	// non-terminal status and no next -> warn. Also cover completed<total
	// even if status is missing but counts are present.
	completed, hasC := jsonNumber(root["completed"])
	total, hasT := jsonNumber(root["total"])
	if hasC && hasT && completed < total {
		return true
	}
	// non-terminal status present, no next -> warn
	return true
}

func jsonNumber(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

// formatProxyBase ensures no trailing slash for URL composition.
func formatProxyBase(s string) string {
	return strings.TrimRight(s, "/")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS. If `fmt` unused-import error appears, remove the `var _ = fmt.Sprintf` line and the `fmt` import.

- [ ] **Step 5: Commit**

```bash
git add rewrite.go rewrite_test.go
git commit -m "feat: next-URL rewriter (strict next-field scope) + pagination guard"
```

---

## Task 5: Proxy handler - forward, buffer, rotate, retry

**Files:**
- Create: `proxy.go`
- Create: `proxy_test.go`

This is the core. It depends on `Config`, `KeyPool`, `shouldRotate`, `rewriteNext`, `paginationGuard`. The handler:

1. Reads the incoming request fully (body buffered for replay on retry).
2. Loops up to `MaxPasses * len(keys)` attempts:
   a. Pick current key. Build upstream request (drop `Authorization`/`Host`, set `Authorization: Bearer <key>`).
   b. Round-trip via the shared `*http.Client`.
   c. Read the response body fully, capped at `MaxBodyBytes` (0 = unbounded). If over cap, forward untouched (no rotate/rewrite) and return.
   d. `shouldRotate(status, body)`: if rotate -> record rejection, `Advance()`, continue loop. Else break.
3. After loop (non-rotating response or cap exhausted): run `rewriteNext` on the body if it's JSON; if `paginationGuard` fires, log a warning.
4. Write status + headers + (possibly rewritten) body to the caller. Fix `Content-Length`.

**Handler constructor:**

```go
type rotator struct {
	cfg     Config
	pool    *KeyPool
	client  *http.Client
	log     *logger
}

func newRotator(cfg Config, pool *KeyPool, client *http.Client, log *logger) *rotator
func (r *rotator) ServeHTTP(w http.ResponseWriter, req *http.Request)
```

- [ ] **Step 1: Write the failing tests**

`proxy_test.go` (uses `httptest.Server` as a fake Firecrawl):

```go
package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeBackend returns 402 for key "fc-bad" and 200 for "fc-good".
func newFakeBackend(t *testing.T, badKey, goodKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		switch auth {
		case "Bearer " + badKey:
			w.WriteHeader(402)
			_, _ = w.Write([]byte(`{"success":false,"error":"Insufficient credits"}`))
		case "Bearer " + goodKey:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"success":true,"data":[],"next":"https://api.firecrawl.dev/v2/x/next"}`))
		default:
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":"Unauthorized"}`))
		}
	}))
}

func newTestRotator(t *testing.T, upstream string, keys ...string) *rotator {
	t.Helper()
	cfg := Config{
		APIKeys:       keys,
		Upstream:      upstream,
		UpstreamHost:  "api.firecrawl.dev",
		MaxPasses:     2,
		MaxBodyBytes:  16 * 1024 * 1024,
		ProxyBaseURL:  "http://rotator.test",
	}
	pool := NewKeyPool(keys)
	client := &http.Client{}
	return newRotator(cfg, pool, client, newLogger("info"))
}

func TestRotator_RotatesOn402(t *testing.T) {
	fake := newFakeBackend(t, "fc-bad", "fc-good")
	defer fake.Close()

	// rewrite upstream host so the fake server's URL host matches
	cfg := Config{
		APIKeys:      []string{"fc-bad", "fc-good"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	// fake returns an absolute next URL using its own host; ensure it gets rewritten
	// to the proxy base. We assert the client sees a rewritten next.
	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{"query":"x"}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (should have rotated to good key)", rec.Code)
	}
	body, _ := io.ReadAll(rec.Body)
	if !bytes.Contains(body, []byte("http://rotator.test/v2/x/next")) {
		t.Fatalf("expected next URL rewritten to proxy base; got %s", body)
	}
	// cursor should now be on the good key (index 1)
	if i, _ := pool.Current(); i != 1 {
		t.Fatalf("cursor = %d, want 1 after rotating off bad key", i)
	}
}

func TestRotator_AllFailCap(t *testing.T) {
	// backend always 402 regardless of key
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
		_, _ = w.Write([]byte(`{"error":"Insufficient credits"}`))
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a", "fc-b"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 402 {
		t.Fatalf("status = %d, want 402 (last error passed through)", rec.Code)
	}
}

func TestRotator_5xxNoRotate(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(502)
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a", "fc-b"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != 502 {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	if i, _ := pool.Current(); i != 0 {
		t.Fatalf("cursor = %d, want 0 (5xx must not rotate)", i)
	}
}

func TestRotator_BodyCapPassthrough(t *testing.T) {
	big := bytes.Repeat([]byte("a"), 1000)
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402) // would normally rotate
		_, _ = w.Write(big)
	}))
	defer fake.Close()

	cfg := Config{
		APIKeys:      []string{"fc-a", "fc-b"},
		Upstream:     fake.URL,
		UpstreamHost: httptestURLHost(fake.URL),
		MaxPasses:    2,
		MaxBodyBytes: 10, // tiny cap: 1000-byte body exceeds it
		ProxyBaseURL: "http://rotator.test",
	}
	pool := NewKeyPool(cfg.APIKeys)
	r := newRotator(cfg, pool, &http.Client{}, newLogger("info"))

	req := httptest.NewRequest("POST", "/v2/search", bytes.NewReader([]byte(`{}`)))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	// over cap -> forwarded untouched, no rotation attempted
	if i, _ := pool.Current(); i != 0 {
		t.Fatalf("cursor = %d, want 0 (over-cap body must not rotate)", i)
	}
}

// httptestURLHost extracts host:port from a httptest.URL for upstreamHost matching.
func httptestURLHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Host
}
```

Note: add `"net/url"` to the test file imports.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL - `newRotator`, `newLogger` undefined.

- [ ] **Step 3: Write `server.go` (logger first - proxy depends on it)**

Create `server.go` with the logger (the `/healthz` and `/status` handlers come in Task 6, but the logger is needed now):

```go
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

type logger struct {
	level string
}

func newLogger(level string) *logger {
	return &logger{level: level}
}

func (l *logger) info(msg string, kv ...any)  { l.log("info", msg, kv...) }
func (l *logger) warn(msg string, kv ...any)  { l.log("warn", msg, kv...) }
func (l *logger) error(msg string, kv ...any) { l.log("error", msg, kv...) }

func (l *logger) debug(msg string, kv ...any) {
	if l.level != "debug" {
		return
	}
	l.log("debug", msg, kv...)
}

func (l *logger) log(level, msg string, kv ...any) {
	ts := time.Now().UTC().Format(time.RFC3339)
	line := fmt.Sprintf("[%s] %s %s", level, ts, msg)
	for i := 0; i+1 < len(kv); i += 2 {
		line += fmt.Sprintf(" %v=%v", kv[i], kv[1+i])
	}
	fmt.Fprintln(os.Stderr, line)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
```

- [ ] **Step 4: Write `proxy.go`**

```go
package main

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
)

type rotator struct {
	cfg    Config
	pool   *KeyPool
	client *http.Client
	log    *logger
}

func newRotator(cfg Config, pool *KeyPool, client *http.Client, log *logger) *rotator {
	return &rotator{cfg: cfg, pool: pool, client: client, log: log}
}

func (r *rotator) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	// Buffer the incoming body once so retries can replay it.
	inBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), 400)
		return
	}
	_ = req.Body.Close()

	maxAttempts := r.cfg.MaxPasses * len(r.pool.keys)
	var lastStatus int
	var lastHeader http.Header
	var lastBody []byte
	var overCap bool

	for attempt := 0; attempt < maxAttempts; attempt++ {
		idx, key := r.pool.Current()

		upReq, err := http.NewRequest(req.Method, r.cfg.Upstream+req.RequestURI, bytes.NewReader(inBody))
		if err != nil {
			http.Error(w, "build upstream request: "+err.Error(), 502)
			return
		}
		// Copy headers except hop-by-hop and the ones we own.
		copyHeaders(upReq.Header, req.Header)
		upReq.Header.Del("Authorization")
		upReq.Header.Del("Host")
		upReq.Header.Set("Authorization", "Bearer "+key)
		upReq.Host = r.cfg.UpstreamHost

		resp, err := r.client.Do(upReq)
		if err != nil {
			// network error: do not rotate, return 502
			r.log.warn("upstream request error", "key", idx, "err", err)
			http.Error(w, "upstream error: "+err.Error(), 502)
			return
		}

		// Read body with cap.
		body, capped := readCapped(resp.Body, r.cfg.MaxBodyBytes)
		_ = resp.Body.Close()
		lastStatus = resp.StatusCode
		lastHeader = resp.Header
		lastBody = body
		overCap = capped

		if capped {
			// over cap: forward untouched, no rotate/rewrite
			r.log.warn("response body over MAX_BODY_BYTES, forwarding untouched", "key", idx)
			break
		}

		rotate, reason := shouldRotate(resp.StatusCode, body)
		if !rotate {
			r.pool.RecordSuccess(idx)
			break
		}
		kind := rejectKind(resp.StatusCode)
		r.pool.RecordRejection(idx, kind)
		r.log.info("rotating key",
			"from", idx, "reason", reason, "masked", maskKey(key))
		r.pool.Advance()
		// loop continues with next key
	}

	if overCap {
		writeRawResponse(w, lastStatus, lastHeader, lastBody)
		return
	}

	// Rewrite next URLs + pagination guard on the final body.
	finalBody := lastBody
	if isJSON(lastHeader) {
		proxyBase := r.cfg.ProxyBaseURL
		if proxyBase == "" {
			proxyBase = "http://" + r.cfg.UpstreamHost // fallback; real base comes from Host header at runtime
		}
		proxyBase = formatProxyBase(proxyBase)
		if rb, changed := rewriteNext(lastBody, proxyBase, r.cfg.UpstreamHost); changed {
			finalBody = rb
		}
		if paginationGuard(lastBody) {
			r.log.warn("pagination response with more data but no 'next' key - rewrite skipped, pagination may bypass proxy")
		}
	}

	writeRawResponse(w, lastStatus, lastHeader, finalBody)
}

func rejectKind(status int) string {
	switch status {
	case 402:
		return "402"
	case 429:
		return "429"
	case 401, 403:
		return "auth"
	default:
		return "retry"
	}
}

func isJSON(h http.Header) bool {
	ct := strings.ToLower(h.Get("Content-Type"))
	return strings.Contains(ct, "json")
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func readCapped(r io.Reader, cap int64) ([]byte, bool) {
	if cap == 0 {
		b, _ := io.ReadAll(r)
		return b, false
	}
	// read up to cap+1 to detect overflow
	lr := io.LimitReader(r, cap+1)
	b, _ := io.ReadAll(lr)
	if int64(len(b)) > cap {
		return b, true
	}
	return b, false
}

func writeRawResponse(w http.ResponseWriter, status int, h http.Header, body []byte) {
	for k, vs := range h {
		if isHopByHop(k) {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func isHopByHop(k string) bool {
	switch k {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
		"Te", "Trailers", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS for all rotator tests. If `isJSON` has an operator-precedence bug (the `||` vs `&&`), fix it - the test for `Content-Type: application/json` must pass.

- [ ] **Step 6: Commit**

```bash
git add proxy.go proxy_test.go server.go
git commit -m "feat: proxy handler with rotation, retry, body cap, next-rewrite"
```

---

## Task 6: Server - /healthz and /status endpoints

**Files:**
- Modify: `server.go` (append handlers)
- Create: `server_test.go`

- [ ] **Step 1: Write the failing tests**

`server_test.go`:

```go
package main

import (
	"encoding/json"
	"net/http"
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
	pool.RecordRejection(0, "402")
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
	if snap.Keys[0].Stats.Pay402 != 1 {
		t.Fatalf("Pay402 = %d, want 1", snap.Keys[0].Stats.Pay402)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./...`
Expected: FAIL - `healthzHandler` / `statusHandler` undefined.

- [ ] **Step 3: Add handlers to `server.go`**

Append to `server.go`:

```go
func healthzHandler(pool *KeyPool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if pool == nil || len(pool.keys) == 0 {
			writeJSON(w, 503, map[string]any{"ok": false})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}
}

func statusHandler(pool *KeyPool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, 200, pool.Snapshot())
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add server.go server_test.go
git commit -m "feat: /healthz and /status endpoints"
```

---

## Task 7: Outbound proxy transport

**Files:**
- Create: `transport.go`
- Create: `transport_test.go`

Builds the `*http.Transport` used by the rotator's `http.Client`. Honors `UPSTREAM_PROXY` first, else `http.ProxyFromEnvironment` (system `HTTPS_PROXY`/`NO_PROXY`).

**API:**

```go
func buildTransport(cfg Config) (*http.Transport, error)
```

- [ ] **Step 1: Write the failing tests**

`transport_test.go`:

```go
package main

import (
	"net/http"
	"net/url"
	"testing"
)

func TestBuildTransport_UpstreamProxy(t *testing.T) {
	cfg := Config{UpstreamProxy: "http://proxy.corp:3128"}
	tr, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The Proxy func should route a request to the configured proxy.
	u, _ := url.Parse("https://api.firecrawl.dev/v2/search")
	proxyURL, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy() error: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "proxy.corp:3128" {
		t.Fatalf("proxyURL = %v, want proxy.corp:3128", proxyURL)
	}
}

func TestBuildTransport_NoProxyDirect(t *testing.T) {
	cfg := Config{UpstreamProxy: ""}
	tr, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, _ := url.Parse("https://api.firecrawl.dev/v2/search")
	// With no env set and no UPSTREAM_PROXY, ProxyFromEnvironment returns nil.
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "")
	proxyURL, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy() error: %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("proxyURL = %v, want nil (direct)", proxyURL)
	}
}

func TestBuildTransport_SystemEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy:8080")
	t.Setenv("NO_PROXY", "")
	cfg := Config{UpstreamProxy: ""}
	tr, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, _ := url.Parse("https://api.firecrawl.dev/v2/search")
	proxyURL, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy() error: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "env-proxy:8080" {
		t.Fatalf("proxyURL = %v, want env-proxy:8080", proxyURL)
	}
}
```

- [ ] **Step 2: Run tests to verify it fails**

Run: `go test ./...`
Expected: FAIL - `buildTransport` undefined.

- [ ] **Step 3: Write `transport.go`**

```go
package main

import (
	"net/http"
	"net/url"
)

func buildTransport(cfg Config) (*http.Transport, error) {
	tr := &http.Transport{
		ForceAttemptHTTP2: true,
	}

	if cfg.UpstreamProxy != "" {
		proxyURL, err := url.Parse(cfg.UpstreamProxy)
		if err != nil {
			return nil, err
		}
		tr.Proxy = http.ProxyURL(proxyURL)
	} else {
		// Honor system HTTPS_PROXY/HTTP_PROXY/NO_PROXY, curl-style.
		tr.Proxy = http.ProxyFromEnvironment
	}

	return tr, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add transport.go transport_test.go
git commit -m "feat: outbound proxy transport (UPSTREAM_PROXY or system env)"
```

---

## Task 8: main.go - wire everything and start the server

**Files:**
- Create: `main.go`
- Create: `main_test.go`

- [ ] **Step 1: Write the failing test (smoke)**

`main_test.go`:

```go
package main

import (
	"net"
	"net/http"
	"testing"
	"time"
)

func TestMain_SmokeHealthz(t *testing.T) {
	// Start the full server on a random port and hit /healthz.
	t.Setenv("FIRECRAWL_API_KEYS", "fc-smoke")
	srv, err := buildServer()
	if err != nil {
		t.Fatalf("buildServer: %v", err)
	}
	srv.Addr = "127.0.0.1:0"
	ln, err := net.Listen("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	base := "http://" + ln.Addr().String()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
}
```

(Removed the `PORT=0` indirection from the earlier sketch - using an ephemeral listener directly is simpler and avoids port-discovery issues.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./...`
Expected: FAIL - `buildServer` undefined.

- [ ] **Step 3: Write `main.go`**

```go
package main

import (
	"net/http"
	"os"
)

func buildServer() (*http.Server, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}

	pool := NewKeyPool(cfg.APIKeys)
	tr, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Transport: tr}
	log := newLogger(cfg.LogLevel)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(pool))
	mux.HandleFunc("/status", statusHandler(pool))
	// Everything else goes to the rotator.
	mux.Handle("/", newRotator(cfg, pool, client, log))

	log.info("firecrawl-rotator starting",
		"keys", len(cfg.APIKeys), "upstream", cfg.Upstream, "maxPasses", cfg.MaxPasses)

	return &http.Server{
		Addr:    cfg.Host + ":" + cfg.Port,
		Handler: mux,
	}, nil
}

func main() {
	srv, err := buildServer()
	if err != nil {
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := srv.ListenAndServe(); err != nil {
		os.Stderr.WriteString("server error: " + err.Error() + "\n")
		os.Exit(1)
	}
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./...`
Expected: PASS (smoke test hits `/healthz` and gets 200).

- [ ] **Step 5: Build the binary**

Run: `go build -o rotator .`
Expected: creates `./rotator` binary, no errors.

- [ ] **Step 6: Commit**

```bash
git add main.go main_test.go
git commit -m "feat: wire main server with routes and start"
```

---

## Task 9: Dockerfile (multi-stage, scratch)

**Files:**
- Create: `Dockerfile`

- [ ] **Step 1: Write the Dockerfile**

```dockerfile
# Build stage
FROM golang:1.22 AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
# Static build, stripped, for scratch
ENV CGO_ENABLED=0
RUN go build -ldflags="-s -w" -o /out/rotator .

# Final stage: scratch + CA certs + binary
FROM scratch
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=builder /out/rotator /rotator
EXPOSE 8788
ENTRYPOINT ["/rotator"]
```

- [ ] **Step 2: Verify it builds (if Docker is available)**

Run: `docker build -t firecrawl-rotator:dev .`
Expected: image builds. (If Docker is unavailable in the exec environment, skip and note it; the Dockerfile is validated by the compose smoke in Task 11 where possible.)

- [ ] **Step 3: Commit**

```bash
git add Dockerfile
git commit -m "feat: multi-stage Dockerfile, scratch final image"
```

---

## Task 10: docker-compose reference wiring

**Files:**
- Create: `docker-compose.yml`

- [ ] **Step 1: Write docker-compose.yml**

```yaml
services:
  firecrawl-rotator:
    build: .
    # or: image: ghcr.io/<you>/firecrawl-rotator:latest
    environment:
      FIRECRAWL_API_KEYS: "fc-key1,fc-key2,fc-key3"
      UPSTREAM: "https://api.firecrawl.dev"
      PORT: "8788"
      MAX_PASSES: "2"
      MAX_BODY_BYTES: "16777216"
      LOG_LEVEL: "info"
      # UPSTREAM_PROXY: "http://corp-proxy:3128"   # optional egress proxy
    ports:
      - "8788:8788"
    healthcheck:
      test: ["CMD", "/rotator", "-healthcheck"]  # see note; or use wget if added
      interval: 30s
      timeout: 3s
      retries: 3
    restart: unless-stopped

  firecrawl:
    image: ghcr.io/open-webui/mcpo:latest
    entrypoint: ["uvx", "mcpo"]
    command: ["--host", "0.0.0.0", "--port", "8000", "--config", "/config/config.json"]
    volumes:
      - ./config:/config:ro
    depends_on:
      firecrawl-rotator:
        condition: service_healthy
    environment:
      # mcpo's config.json sets FIRECRAWL_API_URL per MCP server; shown here
      # for clarity. The key is REMOVED - the rotator injects it.
      FIRECRAWL_API_URL: "http://firecrawl-rotator:8788"
    restart: unless-stopped
```

Note: the `healthcheck` `test` uses `/rotator -healthcheck`. Since the binary doesn't implement that flag, either (a) add a `-healthcheck` mode in `main.go` that GETs `http://localhost:PORT/healthz` and exits 0/1, or (b) drop the healthcheck `test` and rely on TCP liveness. **Choose (a): add the flag.**

- [ ] **Step 2: Add `-healthcheck` mode to main.go**

Replace the **entire** `main()` function in `main.go` with this version (keep `buildServer()` and the imports as-is, but **add `"flag"` to the import block**). The full `main.go` import block should read:

```go
import (
	"flag"
	"net/http"
	"os"
)
```

And the `main()` function becomes:

```go
func main() {
	healthcheck := flag.Bool("healthcheck", false, "GET /healthz and exit 0/1")
	flag.Parse()

	if *healthcheck {
		cfg, err := LoadConfig()
		if err != nil {
			os.Exit(1)
		}
		resp, err := http.Get("http://127.0.0.1:" + cfg.Port + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		_ = resp.Body.Close()
		os.Exit(0)
	}

	srv, err := buildServer()
	if err != nil {
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := srv.ListenAndServe(); err != nil {
		os.Stderr.WriteString("server error: " + err.Error() + "\n")
		os.Exit(1)
	}
}
```

(`buildServer()` stays exactly as written in Task 8.)

- [ ] **Step 3: Test the healthcheck flag locally**

Run: `FIRECRAWL_API_KEYS=fc-x PORT=8788 ./rotator &; sleep 1; ./rotator -healthcheck; echo exit=$?; kill %1`
Expected: `exit=0` (assuming the server is up). If the server isn't running, `exit=1`.

- [ ] **Step 4: Commit**

```bash
git add docker-compose.yml main.go
git commit -m "feat: docker-compose reference wiring + healthcheck flag"
```

---

## Task 11: README and final integration check

**Files:**
- Modify: `README.md` (replace stub)

- [ ] **Step 1: Write the full README**

```markdown
# firecrawl-rotator

A small reverse proxy that sits between **firecrawl-mcp** and the Firecrawl API
(`api.firecrawl.dev`). It holds a pool of Firecrawl API keys, injects one per
request, and **rotates to the next key on rejection** (credit exhaustion, rate
limit, bad key) - retrying transparently so the MCP client never sees a
key-level failure until the whole pool is exhausted.

It also rewrites Firecrawl's `next` pagination URLs so crawl pagination flows
back through the proxy and stays under rotation.

## Why

`firecrawl-mcp` forwards all upstream calls to `FIRECRAWL_API_URL`. Pointing
that at this proxy adds key rotation with **zero changes** to firecrawl-mcp -
run the stock `npx -y firecrawl-mcp`.

## Run

```bash
docker compose up -d
```

With `docker-compose.yml`:

```yaml
firecrawl-rotator:
  build: .
  environment:
    FIRECRAWL_API_KEYS: "fc-key1,fc-key2,fc-key3"
    UPSTREAM: "https://api.firecrawl.dev"
    PORT: "8788"
    MAX_PASSES: "2"

firecrawl:                     # your existing mcpo + firecrawl-mcp service
  environment:
    FIRECRAWL_API_URL: "http://firecrawl-rotator:8788"
    # FIRECRAWL_API_KEY removed - the rotator injects it
```

## Configuration (env vars)

| Var | Default | Purpose |
|-----|---------|---------|
| `FIRECRAWL_API_KEYS` | (required) | Comma-separated key pool. |
| `UPSTREAM` | `https://api.firecrawl.dev` | Upstream Firecrawl API base. |
| `UPSTREAM_PROXY` | (unset) | Explicit forward proxy for egress (`http`/`https`/`socks5`). Wins over system vars. |
| `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` | (unset) | System/curl-style proxy env, honored when `UPSTREAM_PROXY` is unset. |
| `PORT` | `8788` | Listen port. |
| `HOST` | `0.0.0.0` | Listen address. |
| `MAX_PASSES` | `2` | Full passes over the pool before giving up. |
| `MAX_BODY_BYTES` | `16777216` (16 MiB) | Cap on a buffered response body. Above it, forwarded untouched. `0` = no cap. |
| `PROXY_BASE_URL` | (from `Host` header) | Base used when rewriting `next` URLs. |
| `LOG_LEVEL` | `info` | `debug` adds per-request lines. |

## Endpoints

- `GET /healthz` -> `200 {"ok":true}` if pool non-empty, else `503`. Docker healthcheck target.
- `GET /status` -> pool size, current index, per-key stats (keys masked to last 4 chars).

## Rotation behavior

Rotates on HTTP **402** (credits), **429** (rate limit), **401/403** (bad key),
and on error bodies matching `insufficient credits`, `rate limit`, `exceeded`,
`payment required`, `unauthorized`, `forbidden` (catches `200 + success:false`).

- Tries each key up to `MAX_PASSES` times, then returns the last error verbatim.
- **5xx and network errors do NOT rotate** (not a key problem).
- The `next` field's absolute upstream URL is rewritten to the proxy so crawl
  pagination stays under rotation. Other occurrences of the host in response
  bodies are **never** rewritten (they may be real scraped content).

## Develop

```bash
go test ./...
go build -o rotator .
FIRECRAWL_API_KEYS=fc-x ./rotator
```

See `docs/superpowers/specs/2026-07-09-firecrawl-token-rotation-design.md` for
the full design.
```

- [ ] **Step 2: Run the full test suite one more time**

Run: `go test ./...`
Expected: all tests PASS.

- [ ] **Step 3: Run `go vet`**

Run: `go vet ./...`
Expected: no issues.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "docs: full README with config, endpoints, rotation behavior"
```

---

## Done criteria

- [ ] `go test ./...` passes with no failures.
- [ ] `go vet ./...` clean.
- [ ] `go build` produces a static binary.
- [ ] `docker build` produces a scratch-based image (if Docker available).
- [ ] `/healthz` returns 200 with a configured pool.
- [ ] `/status` returns masked stats.
- [ ] A 402 from the upstream triggers rotation to the next key and a 200 reaches the client (covered by `TestRotator_RotatesOn402`).
- [ ] All-keys-fail returns the last error verbatim (`TestRotator_AllFailCap`).
- [ ] 5xx does not rotate (`TestRotator_5xxNoRotate`).
- [ ] Over-cap body is forwarded untouched (`TestRotator_BodyCapPassthrough`).
- [ ] `next` URLs are rewritten; other host occurrences are not (`TestRewriteNext_*`).
- [ ] Outbound proxy honors `UPSTREAM_PROXY` and system env (`TestBuildTransport_*`).
