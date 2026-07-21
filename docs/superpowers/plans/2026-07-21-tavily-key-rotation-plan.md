# Multi-Provider Key Rotation (Tavily + rename to api-key-rotator) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Generalize the Firecrawl-only rotator into a multi-profile key-rotation reverse proxy, add a Tavily profile (route prefix `/tavily`, usage tracking via `GET /usage`, disable on 432/433), and rename the project to `api-key-rotator`.

**Architecture:** One binary, one port, N profiles routed by URL path prefix. The unprefixed default profile keeps today's Firecrawl behavior byte-identical. Each profile owns its KeyPool, Refresher, upstream, and rotation policy (which statuses rotate vs. disable). Shared code (rotation loop, backoff, cooldown, transport) is profile-agnostic.

**Tech Stack:** Go 1.26, stdlib only (no new dependencies), `httptest` for tests, Docker for running tests (host has no Go toolchain).

**Spec:** `docs/superpowers/specs/2026-07-21-tavily-key-rotation-design.md`

## Global Constraints

- **Stdlib only.** `go.mod` must keep zero dependencies.
- **Package stays `main`.**
- **Backward compatibility:** with `TAVILY_API_KEYS` unset, behavior must be byte-identical to today. All existing tests must pass with at most mechanical updates (constructor signatures), never semantic changes.
- **Run tests via Docker** (no host Go): `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "go test ./... && go vet ./... && go build -o /tmp/rotator ."`
- **Design decisions that must survive the refactor** (from CLAUDE.md, non-negotiable):
  - Selection is credit-based with a ~30s cooldown on rotated-off keys.
  - 403 is transient (retry same key with backoff), never rotate/disable.
  - A `success:true` Firecrawl response NEVER rotates; denylist applies only to failure envelopes.
  - Credit-exhausted keys are disabled, not retried; 429/401 rotate-but-keep.
  - `next`-URL rewriting stays narrowly scoped to `"next"` keys with absolute URLs on the upstream host.
- **Commit after every task.**

## File Structure

| File | Change | Responsibility after refactor |
|---|---|---|
| `config.go` | Modify | Parse env into `Config` (global knobs) + `[]ProfileConfig` (per-provider). Validation incl. prefix rules. |
| `profile.go` | **Create** | `ProfileConfig`, `Profile` (runtime: pool/refresher/policy), `buildProfiles`, `matchProfile`, per-profile `shouldRotate`/`isCreditExhausted`. |
| `proxy.go` | Modify | `rotator` routes by path prefix to a `Profile`, strips prefix, runs the (unchanged-shape) rotation loop against `p.pool`. |
| `rotate.go` | Modify | Keep Firecrawl-specific helpers (`firecrawlFailure`, `rejectDenylist`, `shouldRetry`); profile-dispatching wrappers move to `profile.go`. |
| `creditusage.go` | Modify | `fetchUsage`/`refreshKey`/`disableUntilReset` become profile-aware; add `fetchTavilyUsage` + `tavilyRemaining`. |
| `refresh.go` | Modify | `Refresher` takes `*Profile` instead of `Config`. |
| `keys.go` | Modify | `keyStat.Pay402` → `Exhausted` (json `"exhausted"`); `RecordRejection` kinds `"402"`→`"exhausted"`, `"429"`→`"rate"`. |
| `server.go` | Modify | `healthzHandler([]*Profile)` (200 if ANY usable), `statusHandler([]*Profile)` (per-profile snapshots). |
| `main.go` | Modify | `buildServer` builds profiles, wires per-profile goroutines, updates startup log line. |
| `keys_test.go` | Modify | Mechanical: rejection-kind renames. |
| `profile_test.go` | **Create** | Unit tests for matchProfile, per-profile shouldRotate/isCreditExhausted, fetchTavilyUsage/tavilyRemaining. |
| `proxy_tavily_test.go` | **Create** | End-to-end Tavily profile tests through `rotator.ServeHTTP`. |
| `main_test.go` | Modify | Multi-profile /status + /healthz tests. |
| `config_test.go` | Modify | New env var parsing/validation tests. |
| `server_test.go` | Modify | healthz signature updates. |
| `proxy_test.go` | Modify | `cfgFor`/`testRotator` helper updates only. |
| `go.mod`, `Dockerfile`, `docker-compose.yml`, `.github/workflows/build-docker.yml`, `README.md`, `CLAUDE.md` | Modify | Rename + Tavily docs. |

---

### Task 1: Config — parse Tavily profile env vars

**Files:**
- Modify: `config.go`
- Test: `config_test.go`

**Interfaces:**
- Produces (used by Tasks 2, 4, 8):
  ```go
  type ProfileConfig struct {
      Name         string // "firecrawl" | "tavily"
      RoutePrefix  string // "" = default fallback (firecrawl); "/tavily" for tavily
      Upstream     string
      UpstreamHost string
      APIKeys      []string
      LowCredit    int64
      StopCredit   int64
  }

  // Config gains one field:
  Tavily TavilyConfig // zero value (APIKeys nil) = tavily profile disabled

  type TavilyConfig struct {
      APIKeys     []string
      Upstream    string // default https://api.tavily.com
      RoutePrefix string // default /tavily
      LowCredit   int64
      StopCredit  int64
  }
  ```

- [ ] **Step 1: Write the failing tests**

Append to `config_test.go` (note: the existing env-clearing helper at the top lists vars to unset — add `"TAVILY_API_KEYS", "TAVILY_UPSTREAM", "TAVILY_ROUTE_PREFIX", "TAVILY_LOW_CREDIT_THRESHOLD", "TAVILY_STOP_CREDIT_THRESHOLD"` to that list):

```go
func TestLoadConfig_tavilyDefaultsWhenUnset(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tavily.APIKeys) != 0 {
		t.Fatalf("Tavily.APIKeys = %v, want empty (profile disabled)", cfg.Tavily.APIKeys)
	}
	// Defaults are still populated so the profile is ready if keys appear.
	if cfg.Tavily.Upstream != "https://api.tavily.com" {
		t.Fatalf("Tavily.Upstream = %q", cfg.Tavily.Upstream)
	}
	if cfg.Tavily.RoutePrefix != "/tavily" {
		t.Fatalf("Tavily.RoutePrefix = %q", cfg.Tavily.RoutePrefix)
	}
	if cfg.Tavily.LowCredit != cfg.LowCreditThreshold || cfg.Tavily.StopCredit != cfg.StopCreditThreshold {
		t.Fatalf("tavily thresholds = %d/%d, want shared %d/%d",
			cfg.Tavily.LowCredit, cfg.Tavily.StopCredit, cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	}
}

func TestLoadConfig_tavilyKeysParsed(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("TAVILY_API_KEYS", "tvly-a, tvly-b")
	t.Setenv("TAVILY_ROUTE_PREFIX", "/tv")
	t.Setenv("TAVILY_LOW_CREDIT_THRESHOLD", "5")
	t.Setenv("TAVILY_STOP_CREDIT_THRESHOLD", "1")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Tavily.APIKeys) != 2 || cfg.Tavily.APIKeys[0] != "tvly-a" || cfg.Tavily.APIKeys[1] != "tvly-b" {
		t.Fatalf("Tavily.APIKeys = %v", cfg.Tavily.APIKeys)
	}
	if cfg.Tavily.RoutePrefix != "/tv" {
		t.Fatalf("Tavily.RoutePrefix = %q", cfg.Tavily.RoutePrefix)
	}
	if cfg.Tavily.LowCredit != 5 || cfg.Tavily.StopCredit != 1 {
		t.Fatalf("tavily thresholds = %d/%d", cfg.Tavily.LowCredit, cfg.Tavily.StopCredit)
	}
}

func TestLoadConfig_tavilyBadPrefixErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("TAVILY_API_KEYS", "tvly-a")
	for _, bad := range []string{"tavily", "/", "/tavily/", "/healthz", "/status"} {
		t.Setenv("TAVILY_ROUTE_PREFIX", bad)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("expected error for TAVILY_ROUTE_PREFIX=%q, got nil", bad)
		}
	}
}

func TestLoadConfig_tavilyBadUpstreamErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("TAVILY_API_KEYS", "tvly-a")
	t.Setenv("TAVILY_UPSTREAM", "ftp://example.com")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error for TAVILY_UPSTREAM=ftp://example.com, got nil")
	}
}

func TestLoadConfig_tavilyStopAboveLowErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("TAVILY_API_KEYS", "tvly-a")
	t.Setenv("TAVILY_LOW_CREDIT_THRESHOLD", "2")
	t.Setenv("TAVILY_STOP_CREDIT_THRESHOLD", "5")
	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected error when TAVILY_STOP > TAVILY_LOW, got nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test -run TestLoadConfig_tavily ./...`
Expected: FAIL — `cfg.Tavily` undefined (compile error).

- [ ] **Step 3: Implement**

In `config.go`, add after the `Config` struct:

```go
// TavilyConfig holds the Tavily profile's settings. The profile is disabled
// when APIKeys is empty.
type TavilyConfig struct {
	APIKeys     []string
	Upstream    string
	RoutePrefix string
	LowCredit   int64
	StopCredit  int64
}
```

Add `Tavily TavilyConfig` to the `Config` struct.

In `LoadConfig`, after the existing `UPSTREAM_PROXY` block and before the `return Config{...}`:

```go
	tavily, err := loadTavilyConfig(lowCredit, stopCredit)
	if err != nil {
		return Config{}, err
	}
```

Add `Tavily: tavily,` to the returned struct literal. Then add:

```go
// loadTavilyConfig parses the TAVILY_* env vars. Tavily thresholds default to
// the shared LOW/STOP values. The route prefix must start with '/', must not
// end with '/', and must not shadow the reserved /healthz or /status paths.
func loadTavilyConfig(sharedLow, sharedStop int64) (TavilyConfig, error) {
	t := TavilyConfig{
		APIKeys:     parseKeys(os.Getenv("TAVILY_API_KEYS")),
		RoutePrefix: envStr("TAVILY_ROUTE_PREFIX", "/tavily"),
		LowCredit:   sharedLow,
		StopCredit:  sharedStop,
	}

	upstream := envStr("TAVILY_UPSTREAM", "https://api.tavily.com")
	tu, err := url.Parse(upstream)
	if err != nil || tu.Host == "" || (tu.Scheme != "http" && tu.Scheme != "https") {
		return TavilyConfig{}, fmt.Errorf("TAVILY_UPSTREAM %q is not a valid http(s) URL", upstream)
	}
	t.Upstream = strings.TrimRight(upstream, "/")

	if !strings.HasPrefix(t.RoutePrefix, "/") || len(t.RoutePrefix) < 2 || strings.HasSuffix(t.RoutePrefix, "/") {
		return TavilyConfig{}, fmt.Errorf("TAVILY_ROUTE_PREFIX %q must start with '/' and be a non-root path without trailing slash", t.RoutePrefix)
	}
	if t.RoutePrefix == "/healthz" || t.RoutePrefix == "/status" {
		return TavilyConfig{}, fmt.Errorf("TAVILY_ROUTE_PREFIX %q shadows a reserved path", t.RoutePrefix)
	}

	if v := strings.TrimSpace(os.Getenv("TAVILY_LOW_CREDIT_THRESHOLD")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return TavilyConfig{}, fmt.Errorf("TAVILY_LOW_CREDIT_THRESHOLD must be a non-negative integer, got %q", v)
		}
		t.LowCredit = n
	}
	if v := strings.TrimSpace(os.Getenv("TAVILY_STOP_CREDIT_THRESHOLD")); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return TavilyConfig{}, fmt.Errorf("TAVILY_STOP_CREDIT_THRESHOLD must be a non-negative integer, got %q", v)
		}
		t.StopCredit = n
	}
	if t.StopCredit > t.LowCredit {
		return TavilyConfig{}, fmt.Errorf("TAVILY_STOP_CREDIT_THRESHOLD (%d) must be <= TAVILY_LOW_CREDIT_THRESHOLD (%d)", t.StopCredit, t.LowCredit)
	}
	return t, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "go test ./... && go vet ./..."`
Expected: PASS (all existing tests unaffected).

- [ ] **Step 5: Commit**

```bash
git add config.go config_test.go
git commit -m "feat(config): parse TAVILY_* env vars for the tavily profile

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 2: Profile type, routing, and per-profile rotation policy

**Files:**
- Create: `profile.go`
- Modify: `rotate.go`
- Test: `profile_test.go` (create)

**Interfaces:**
- Consumes: `ProfileConfig`-shaped data (constructed inline here; Task 1's `Config.Tavily` is wired in Task 4), `firecrawlFailure`/`rejectDenylist`/`shouldRetry` from `rotate.go`.
- Produces (used by Tasks 3, 4, 6, 7):
  ```go
  type Profile struct {
      Name         string
      RoutePrefix  string
      Upstream     string
      UpstreamHost string
      CreditResetDay int
      RewriteNext  bool // firecrawl only
      pool         *KeyPool
      refresh      *Refresher // set by main.go, nil in most tests
  }

  func (p *Profile) shouldRotate(status int, body []byte) (bool, string)
  func (p *Profile) isCreditExhausted(status int, body []byte) bool

  // matchProfile returns the profile whose RoutePrefix is a prefix of path
  // (boundary-matched), plus the path with the prefix stripped. The
  // no-prefix profile is the fallback. Second return is false if a
  // prefixed path matched no configured profile.
  func matchProfile(profiles []*Profile, path string) (*Profile, string, bool)
  ```

- [ ] **Step 1: Write the failing tests**

Create `profile_test.go`:

```go
package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func profilesForTest() []*Profile {
	fc := &Profile{Name: "firecrawl", RoutePrefix: "", pool: NewKeyPool([]string{"fc-a"})}
	tv := &Profile{Name: "tavily", RoutePrefix: "/tavily", pool: NewKeyPool([]string{"tvly-a"})}
	return []*Profile{fc, tv}
}

func TestMatchProfile(t *testing.T) {
	profiles := profilesForTest()
	cases := []struct {
		path       string
		wantName   string
		wantStripped string
		wantOK     bool
	}{
		{"/v2/scrape", "firecrawl", "/v2/scrape", true},
		{"/tavily/search", "tavily", "/search", true},
		{"/tavily/extract", "tavily", "/extract", true},
		{"/tavily", "tavily", "/", true},
		{"/tavilyfoo/search", "firecrawl", "/tavilyfoo/search", true}, // no segment boundary -> default
		{"/", "firecrawl", "/", true},
	}
	for _, c := range cases {
		p, stripped, ok := matchProfile(profiles, c.path)
		if !ok || p.Name != c.wantName || stripped != c.wantStripped {
			t.Errorf("matchProfile(%q) = (%v, %q, %v), want (%s, %q, true)",
				c.path, p, stripped, ok, c.wantName, c.wantStripped)
		}
	}
}

func TestMatchProfile_unconfiguredPrefixFallsToDefault(t *testing.T) {
	// Only the firecrawl profile configured: /tavily/... falls through to the
	// default profile (byte-compat: single-profile deployments change nothing).
	profiles := []*Profile{{Name: "firecrawl", RoutePrefix: "", pool: NewKeyPool([]string{"fc-a"})}}
	p, stripped, ok := matchProfile(profiles, "/tavily/search")
	if !ok || p.Name != "firecrawl" || stripped != "/tavily/search" {
		t.Fatalf("matchProfile = (%v, %q, %v), want firecrawl default fallthrough", p, stripped, ok)
	}
}

func TestProfile_shouldRotate_firecrawl(t *testing.T) {
	p := &Profile{Name: "firecrawl"}
	for _, st := range []int{401, 402, 429} {
		if rotate, _ := p.shouldRotate(st, nil); !rotate {
			t.Errorf("firecrawl shouldRotate(%d) = false, want true", st)
		}
	}
	if rotate, _ := p.shouldRotate(403, nil); rotate {
		t.Error("firecrawl shouldRotate(403) = true, want false (transient)")
	}
	// Failure envelope with denylist phrase still rotates on 200.
	body := []byte(`{"success":false,"error":"rate limit exceeded"}`)
	if rotate, _ := p.shouldRotate(200, body); !rotate {
		t.Error("firecrawl shouldRotate(200, failure envelope) = false, want true")
	}
	// success:true NEVER rotates, even with denylist words in content.
	ok := []byte(`{"success":true,"data":[{"markdown":"payment required here"}]}`)
	if rotate, _ := p.shouldRotate(200, ok); rotate {
		t.Error("firecrawl shouldRotate(200, success:true) = true, want false")
	}
}

func TestProfile_shouldRotate_tavily(t *testing.T) {
	p := &Profile{Name: "tavily"}
	for _, st := range []int{401, 429, 432, 433} {
		if rotate, _ := p.shouldRotate(st, nil); !rotate {
			t.Errorf("tavily shouldRotate(%d) = false, want true", st)
		}
	}
	for _, st := range []int{200, 400, 403, 402} {
		if rotate, _ := p.shouldRotate(st, nil); rotate {
			t.Errorf("tavily shouldRotate(%d) = true, want false", st)
		}
	}
	// Body text never triggers rotation for tavily (status codes suffice).
	body := []byte(`{"detail":{"error":"payment required"}}`)
	if rotate, _ := p.shouldRotate(200, body); rotate {
		t.Error("tavily shouldRotate(200, denylist-ish body) = true, want false")
	}
}

func TestProfile_isCreditExhausted(t *testing.T) {
	fc := &Profile{Name: "firecrawl"}
	if !fc.isCreditExhausted(402, nil) {
		t.Error("firecrawl isCreditExhausted(402) = false, want true")
	}
	if !fc.isCreditExhausted(200, []byte(`{"success":false,"error":"insufficient credits"}`)) {
		t.Error("firecrawl isCreditExhausted(credits envelope) = false, want true")
	}
	if fc.isCreditExhausted(429, nil) {
		t.Error("firecrawl isCreditExhausted(429) = true, want false")
	}

	tv := &Profile{Name: "tavily"}
	if !tv.isCreditExhausted(432, nil) || !tv.isCreditExhausted(433, nil) {
		t.Error("tavily isCreditExhausted(432/433) = false, want true")
	}
	for _, st := range []int{401, 402, 429} {
		if tv.isCreditExhausted(st, nil) {
			t.Errorf("tavily isCreditExhausted(%d) = true, want false", st)
		}
	}
	// Tavily never disables on body text.
	if tv.isCreditExhausted(200, []byte(`{"detail":{"error":"insufficient credits"}}`)) {
		t.Error("tavily isCreditExhausted(200 body) = true, want false")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test -run "TestMatchProfile|TestProfile_" ./...`
Expected: FAIL — `Profile`, `matchProfile` undefined (compile error).

- [ ] **Step 3: Implement**

Create `profile.go`:

```go
package main

import (
	"strconv"
	"strings"
)

// Profile is one upstream provider's runtime bundle: key pool, refresher, and
// the per-provider rotation policy. The default (firecrawl) profile has an
// empty RoutePrefix and catches every path no other profile claims.
type Profile struct {
	Name           string
	RoutePrefix    string
	Upstream       string
	UpstreamHost   string
	CreditResetDay int
	RewriteNext    bool // rewrite "next" pagination URLs (firecrawl only)

	pool    *KeyPool
	refresh *Refresher
}

// shouldRotate is the per-profile reject decision.
//
// firecrawl: 402/429/401 always rotate; otherwise a failure envelope whose
// error text matches the denylist rotates. A success:true response NEVER
// rotates (scraped content legitimately contains denylist words).
//
// tavily: 401/429/432/433 rotate purely on status; the body is never
// consulted (Tavily error codes are unambiguous).
//
// 403 is never here for any profile - it is transient (edge/WAF) and retried
// with backoff on the SAME key via shouldRetry.
func (p *Profile) shouldRotate(status int, body []byte) (bool, string) {
	if p.Name == "tavily" {
		switch status {
		case 401, 429, 432, 433:
			return true, "status " + strconv.Itoa(status)
		}
		return false, ""
	}
	// firecrawl (default)
	switch status {
	case 402, 429, 401:
		return true, "status " + strconv.Itoa(status)
	}
	if !firecrawlFailure(status, body) {
		return false, ""
	}
	if m := rejectDenylist.Find(body); m != nil {
		return true, "body:" + string(m)
	}
	return false, ""
}

// isCreditExhausted reports whether a rejection means the key's credits are
// genuinely gone until reset (disables the key). Rate-limit/auth never
// disable.
//
// firecrawl: 402, or a failure envelope mentioning credits/payment.
// tavily: 432 (plan limit) / 433 (pay-as-you-go limit), status only.
func (p *Profile) isCreditExhausted(status int, body []byte) bool {
	if p.Name == "tavily" {
		return status == 432 || status == 433
	}
	if status == 402 {
		return true
	}
	if firecrawlFailure(status, body) {
		return creditExhaustedPattern.Find(body) != nil
	}
	return false
}

// matchProfile resolves a request path to a profile. A prefixed profile
// matches when path == prefix or path starts with prefix+"/" (segment
// boundary, so "/tavilyfoo" does not match "/tavily"). The matched prefix is
// stripped. The no-prefix profile is the fallback for everything else,
// including prefixes that were never configured.
func matchProfile(profiles []*Profile, path string) (*Profile, string, bool) {
	var def *Profile
	for _, p := range profiles {
		if p.RoutePrefix == "" {
			def = p
			continue
		}
		if path == p.RoutePrefix {
			return p, "/", true
		}
		if strings.HasPrefix(path, p.RoutePrefix+"/") {
			return p, path[len(p.RoutePrefix):], true
		}
	}
	if def != nil {
		return def, path, true
	}
	return nil, path, false
}
```

In `rotate.go`, delete the old `shouldRotate` and `isCreditExhausted` functions (their logic moved into `profile.go`). Add the extracted pattern at the top (next to `rejectDenylist`):

```go
// creditExhaustedPattern matches failure-envelope text that means the key's
// credits are genuinely gone (firecrawl profile only).
var creditExhaustedPattern = regexp.MustCompile(`(?i)(insufficient credits?|payment required|exceeded)`)
```

Keep `firecrawlFailure`, `rejectDenylist`, and `shouldRetry` exactly as they are.

- [ ] **Step 4: Run tests to verify they pass**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "go test -run 'TestMatchProfile|TestProfile_' ./... && go vet ./..."`
Expected: PASS. (`proxy.go` still references the deleted package-level `shouldRotate`/`isCreditExhausted` — that breaks compile until Task 3. Run only the new tests via `go test -run` after confirming the whole build in Task 3; if compile fails on proxy.go, proceed to Task 3 immediately and run both tasks' tests together there.)

- [ ] **Step 5: Commit** (together with Task 3 if compile coupling requires)

```bash
git add profile.go profile_test.go rotate.go
git commit -m "feat(profile): Profile type, path-prefix routing, per-profile rotate/disable policy

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 3: Rotator routes requests to profiles

**Files:**
- Modify: `proxy.go`
- Modify: `proxy_test.go` (helpers only)
- Test: `proxy_tavily_test.go` (create)

**Interfaces:**
- Consumes: `Profile`, `matchProfile` (Task 2).
- Produces:
  ```go
  type rotator struct {
      cfg      Config
      profiles []*Profile
      client   *http.Client
      log      *logger
  }
  func newRotator(cfg Config, profiles []*Profile, client *http.Client, log *logger) *rotator
  ```
  The 503-exhausted body becomes profile-aware: `{"success":false,"error":"all keys credit-exhausted until billing reset","profile":"<name>"}`.

- [ ] **Step 1: Update test helpers + write the failing Tavily e2e tests**

In `proxy_test.go`, replace `testRotator`:

```go
// testRotator builds a rotator with a single firecrawl profile from cfg, with
// credit thresholds set and a nil refresher.
func testRotator(cfg Config, pool *KeyPool, client *http.Client) *rotator {
	pool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	p := &Profile{
		Name:           "firecrawl",
		Upstream:       cfg.Upstream,
		UpstreamHost:   cfg.UpstreamHost,
		CreditResetDay: cfg.CreditResetDay,
		RewriteNext:    true,
		pool:           pool,
	}
	return newRotator(cfg, []*Profile{p}, client, newLogger("info"))
}
```

Create `proxy_tavily_test.go`:

```go
package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// tavilyTestRotator builds a rotator with BOTH profiles: firecrawl (no
// prefix, upstream fcFake) and tavily (/tavily, upstream tvFake).
func tavilyTestRotator(cfg Config, fcPool, tvPool *KeyPool, client *http.Client, fcUpstream, tvUpstream string) *rotator {
	fcPool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	tvPool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	host := func(u string) string {
		if i := strings.Index(u, "://"); i >= 0 {
			return u[i+3:]
		}
		return u
	}
	fc := &Profile{Name: "firecrawl", Upstream: fcUpstream, UpstreamHost: host(fcUpstream), RewriteNext: true, pool: fcPool}
	tv := &Profile{Name: "tavily", RoutePrefix: "/tavily", Upstream: tvUpstream, UpstreamHost: host(tvUpstream), pool: tvPool}
	return newRotator(cfg, []*Profile{fc, tv}, client, newLogger("info"))
}

func post(t *testing.T, r *rotator, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Host = "rotator.test"
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func TestRotator_TavilyPrefixStripped(t *testing.T) {
	var gotPath, gotAuth string
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if gotPath != "/search" {
		t.Fatalf("upstream path = %q, want /search (prefix stripped)", gotPath)
	}
	if gotAuth != "Bearer tvly-a" {
		t.Fatalf("upstream auth = %q, want pooled key", gotAuth)
	}
}

func TestRotator_TavilyRotatesOn432AndDisables(t *testing.T) {
	var callsA, callsB atomic.Int32
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer tvly-a":
			callsA.Add(1)
			w.WriteHeader(432)
			_, _ = w.Write([]byte(`{"detail":{"error":"plan usage limit exceeded"}}`))
		default:
			callsB.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[{"title":"t"}]}`))
		}
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 after rotation (body %q)", rec.Code, rec.Body.String())
	}
	if callsA.Load() != 1 || callsB.Load() != 1 {
		t.Fatalf("calls a/b = %d/%d, want 1/1", callsA.Load(), callsB.Load())
	}
	snap := tvPool.Snapshot()
	if !snap.Keys[0].Disabled {
		t.Fatal("key 0 (432) should be disabled")
	}
	if snap.Keys[1].Disabled {
		t.Fatal("key 1 (success) should not be disabled")
	}
}

func TestRotator_TavilyRotatesOn433(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tvly-a" {
			w.WriteHeader(433)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/extract", `{"urls":["https://x"]}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if !tvPool.Snapshot().Keys[0].Disabled {
		t.Fatal("key 0 (433) should be disabled")
	}
}

func TestRotator_Tavily429RotatesButKeepsKey(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer tvly-a" {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}
	if tvPool.Snapshot().Keys[0].Disabled {
		t.Fatal("429 must NOT disable the key")
	}
}

func TestRotator_TavilySuccessDoesNotRotateAndDecrementsOne(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Even denylist-flavored content in a 200 must not rotate.
		_, _ = w.Write([]byte(`{"results":[{"title":"rate limit exceeded news"}]}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	tvPool.SetCredits(0, 50)
	tvPool.SetCredits(1, 50)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d (body %q)", rec.Code, rec.Body.String())
	}
	if got := tvPool.Snapshot().Keys[0].RemainingCredits; got != 49 {
		t.Fatalf("remaining = %d, want 49 (decrement by 1)", got)
	}
}

func TestRotator_TavilyNoDenylistRotationOn200FailureShapedBody(t *testing.T) {
	// Tavily errors are always non-2xx; a 200 with an error-looking body is
	// content, not a rejection. Must NOT rotate.
	var calls atomic.Int32
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"detail":{"error":"odd but 200"}}`))
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a", "tvly-b"})
	withCredits(tvPool)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("upstream calls = %d, want 1 (no rotation on 200)", calls.Load())
	}
}

func TestRotator_UnprefixedStillFirecrawl(t *testing.T) {
	fcFake := newFakeBackend(t, "fc-a", "fc-b")
	defer fcFake.Close()
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer tvFake.Close()

	cfg := cfgFor(fcFake)
	fcPool := NewKeyPool(cfg.APIKeys)
	withCredits(fcPool)
	r := tavilyTestRotator(cfg, fcPool, NewKeyPool([]string{"tvly-a"}), http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/v2/scrape", `{"url":"https://x"}`)
	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200 from firecrawl profile", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"success":true`) {
		t.Fatalf("body = %q, want firecrawl success envelope", rec.Body.String())
	}
}

func TestRotator_TavilyAllExhausted503(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(432)
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	tvPool := NewKeyPool([]string{"tvly-a"})
	tvPool.SetCredits(0, 5)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	rec := post(t, r, "/tavily/search", `{"query":"x"}`)
	if rec.Code != 503 {
		t.Fatalf("status = %d, want 503 (body %q)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "credit-exhausted") {
		t.Fatalf("body = %q, want credit-exhausted message", rec.Body.String())
	}
}

func TestRotator_TavilyDisableUsesFallbackReset(t *testing.T) {
	tvFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(432)
	}))
	defer tvFake.Close()
	fcFake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer fcFake.Close()

	cfg := cfgFor(fcFake)
	cfg.CreditResetDay = 15
	tvPool := NewKeyPool([]string{"tvly-a"})
	tvPool.SetCredits(0, 5)
	r := tavilyTestRotator(cfg, NewKeyPool(cfg.APIKeys), tvPool, http.DefaultClient, fcFake.URL, tvFake.URL)

	post(t, r, "/tavily/search", `{"query":"x"}`)
	k := tvPool.Snapshot().Keys[0]
	if !k.Disabled {
		t.Fatal("key should be disabled after 432")
	}
	// Tavily has no billingPeriodEnd: reset is the CREDIT_RESET_DAY fallback
	// (day 15 of this or next month, 00:00 UTC), so DisabledUntil's day is 15.
	if k.DisabledUntil.Day() != 15 {
		t.Fatalf("DisabledUntil = %v, want day 15 (CREDIT_RESET_DAY fallback)", k.DisabledUntil)
	}
	if k.DisabledUntil.Before(time.Now()) {
		t.Fatalf("DisabledUntil = %v is in the past", k.DisabledUntil)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test -run Tavily ./...`
Expected: FAIL — `newRotator` signature mismatch / compile error.

- [ ] **Step 3: Implement**

In `proxy.go`, replace the `rotator` struct, constructor, and `ServeHTTP` with profile-routed versions. `tryKey` and everything below it stay, with small signature changes:

```go
type rotator struct {
	cfg      Config
	profiles []*Profile
	client   *http.Client
	log      *logger
}

func newRotator(cfg Config, profiles []*Profile, client *http.Client, log *logger) *rotator {
	return &rotator{cfg: cfg, profiles: profiles, client: client, log: log}
}
```

New `ServeHTTP` (prefix routing happens first, then the per-profile loop — the loop body is today's, with `r.pool` → `p.pool`, `r.refresh` → `p.refresh`, `shouldRotate(...)` → `p.shouldRotate(...)`, `isCreditExhausted(...)` → `p.isCreditExhausted(...)`, and the disable call updated):

```go
func (r *rotator) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	p, strippedPath, ok := matchProfile(r.profiles, req.URL.Path)
	if !ok {
		http.Error(w, `{"success":false,"error":"no profile configured for this path"}`, http.StatusNotFound)
		return
	}

	// Buffer the incoming body once so retries can replay it.
	inBody, err := io.ReadAll(req.Body)
	if err != nil {
		http.Error(w, "read request body: "+err.Error(), 400)
		return
	}
	_ = req.Body.Close()

	maxRotations := r.cfg.MaxPasses * len(p.pool.keys)
	var lastStatus int
	var lastHeader http.Header
	var lastBody []byte
	var overCap bool
	var cleanBreak bool

	for rotation := 0; rotation < maxRotations; rotation++ {
		idx, key := p.pool.Current()
		if idx < 0 {
			r.log.warn("no usable keys (all below stop credit threshold)", "profile", p.Name)
			break
		}

		status, header, body, capped, netErr := r.tryKey(req, p, idx, key, inBody, strippedPath)

		if capped {
			lastStatus, lastHeader, lastBody, overCap = status, header, body, true
			r.log.warn("response body over MAX_BODY_BYTES, forwarding untouched", "profile", p.Name, "key", idx)
			cleanBreak = true
			break
		}

		if netErr {
			lastStatus, lastHeader, lastBody = status, header, body
			r.log.warn("transient errors exhausted on key, rotating", "profile", p.Name, "key", idx)
			prev := idx
			p.pool.Advance()
			if next, _ := p.pool.Current(); next >= 0 && p.refresh != nil {
				p.refresh.OnSwitch(prev, next)
			}
			continue
		}

		lastStatus, lastHeader, lastBody = status, header, body

		rotate, reason := p.shouldRotate(status, body)
		if !rotate {
			p.pool.RecordSuccess(idx)
			p.pool.Decrement(idx, extractCreditsUsed(body))
			if p.refresh != nil {
				p.refresh.MaybeRefreshLow(idx)
			}
			cleanBreak = true
			break
		}

		kind := rejectKind(status)
		p.pool.RecordRejection(idx, kind)
		r.log.info("rotating key", "profile", p.Name, "from", idx, "reason", reason, "masked", maskKey(key))
		if p.isCreditExhausted(status, body) {
			disableUntilReset(p, r.client, r.cfg, idx, key, time.Now().UTC(), r.log)
			r.log.warn("key credit-disabled until reset", "profile", p.Name, "key", idx, "masked", maskKey(key))
		}
		prev := idx
		p.pool.Advance()
		if next, _ := p.pool.Current(); next >= 0 && next != prev && p.refresh != nil {
			p.refresh.OnSwitch(prev, next)
		}
	}

	if !cleanBreak {
		r.log.warn("all keys exhausted", "profile", p.Name, "lastStatus", lastStatus, "keys", len(p.pool.keys), "maxPasses", r.cfg.MaxPasses)
	}

	if overCap {
		writeRawResponse(w, lastStatus, lastHeader, lastBody)
		return
	}

	if idx, _ := p.pool.Current(); idx < 0 {
		http.Error(w, `{"success":false,"error":"all keys credit-exhausted until billing reset","profile":"`+p.Name+`"}`, http.StatusServiceUnavailable)
		return
	}

	if !cleanBreak && lastStatus == 0 {
		http.Error(w, `{"success":false,"error":"upstream unavailable"}`, http.StatusBadGateway)
		return
	}

	// Rewrite next URLs + pagination guard on the final body (firecrawl only).
	finalBody := lastBody
	if p.RewriteNext && isJSON(lastHeader) {
		proxyBase := r.cfg.ProxyBaseURL
		if proxyBase == "" {
			proxyBase = "http://" + req.Host
		}
		proxyBase = formatProxyBase(proxyBase)
		if rb, changed := rewriteNext(lastBody, proxyBase, p.UpstreamHost); changed {
			finalBody = rb
		}
		if paginationGuard(lastBody) {
			r.log.warn("pagination response with more data but no 'next' key - rewrite skipped, pagination may bypass proxy")
		}
	}

	writeRawResponse(w, lastStatus, lastHeader, finalBody)
}
```

Update `tryKey`'s signature and the two upstream-URL lines:

```go
func (r *rotator) tryKey(req *http.Request, p *Profile, idx int, key string, inBody []byte, strippedPath string) (status int, header http.Header, body []byte, capped bool, netErr bool) {
	for attempt := 0; attempt < len(backoffSchedule)+1; attempt++ {
		upstreamURL := p.Upstream + strippedPath
		if req.URL.RawQuery != "" {
			upstreamURL += "?" + req.URL.RawQuery
		}
		upReq, err := http.NewRequestWithContext(req.Context(), req.Method, upstreamURL, bytes.NewReader(inBody))
		...
		upReq.Host = p.UpstreamHost
```

(Everything else in `tryKey` unchanged; log lines may add `"profile", p.Name`.)

Also update `rejectKind` for the renamed stats kinds (Task 5 renames the counters; do it now to keep one compile unit):

```go
func rejectKind(status int) string {
	switch status {
	case 402, 432, 433:
		return "exhausted"
	case 429:
		return "rate"
	case 401:
		return "auth"
	default:
		return "retry"
	}
}
```

- [ ] **Step 4: Run all tests**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "go test ./... && go vet ./..."`
Expected: existing `proxy_test.go` tests pass via updated helpers; new Tavily tests pass; Task 2's profile tests pass. Tests referencing `rejectKind`/stats kinds or `disableUntilReset`'s old signature may fail until Tasks 5/6 — if so, implement Tasks 5 and 6 before this run and verify everything together (these three are one compile unit).

- [ ] **Step 5: Commit**

```bash
git add proxy.go proxy_test.go proxy_tavily_test.go profile.go profile_test.go rotate.go keys.go keys_test.go creditusage.go
git commit -m "feat(proxy): route by path prefix to per-profile pools; add tavily profile policy

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 4: buildProfiles — wire Config to Profiles

**Files:**
- Modify: `profile.go`
- Test: `profile_test.go`

**Interfaces:**
- Consumes: `Config` + `TavilyConfig` (Task 1), `Profile` (Task 2).
- Produces (used by `main.go` in Task 7):
  ```go
  func buildProfiles(cfg Config) []*Profile
  ```
  Always returns the firecrawl profile first; appends tavily only when `cfg.Tavily.APIKeys` is non-empty. Sets pool thresholds per profile.

- [ ] **Step 1: Write the failing tests**

Append to `profile_test.go`:

```go
func TestBuildProfiles_firecrawlOnly(t *testing.T) {
	cfg := Config{
		APIKeys:            []string{"fc-a", "fc-b"},
		Upstream:           "https://api.firecrawl.dev",
		UpstreamHost:       "api.firecrawl.dev",
		CreditResetDay:     3,
		LowCreditThreshold: 10,
		StopCreditThreshold: 2,
	}
	profiles := buildProfiles(cfg)
	if len(profiles) != 1 {
		t.Fatalf("len = %d, want 1 (tavily disabled)", len(profiles))
	}
	p := profiles[0]
	if p.Name != "firecrawl" || p.RoutePrefix != "" || !p.RewriteNext {
		t.Fatalf("firecrawl profile = %+v", p)
	}
	if p.CreditResetDay != 3 || p.UpstreamHost != "api.firecrawl.dev" {
		t.Fatalf("firecrawl profile fields wrong: %+v", p)
	}
	snap := p.pool.Snapshot()
	if snap.PoolSize != 2 {
		t.Fatalf("pool size = %d, want 2", snap.PoolSize)
	}
}

func TestBuildProfiles_withTavily(t *testing.T) {
	cfg := Config{
		APIKeys:            []string{"fc-a"},
		Upstream:           "https://api.firecrawl.dev",
		UpstreamHost:       "api.firecrawl.dev",
		LowCreditThreshold: 10,
		StopCreditThreshold: 2,
		Tavily: TavilyConfig{
			APIKeys:     []string{"tvly-a", "tvly-b"},
			Upstream:    "https://api.tavily.com",
			RoutePrefix: "/tavily",
			LowCredit:   5,
			StopCredit:  1,
		},
	}
	profiles := buildProfiles(cfg)
	if len(profiles) != 2 {
		t.Fatalf("len = %d, want 2", len(profiles))
	}
	tv := profiles[1]
	if tv.Name != "tavily" || tv.RoutePrefix != "/tavily" || tv.RewriteNext {
		t.Fatalf("tavily profile = %+v", tv)
	}
	if tv.UpstreamHost != "api.tavily.com" {
		t.Fatalf("tavily UpstreamHost = %q", tv.UpstreamHost)
	}
	// Thresholds are per-profile: tavily pool uses its own 5/1.
	tv.pool.mu.Lock()
	low, stop := tv.pool.lowThreshold, tv.pool.stopThreshold
	tv.pool.mu.Unlock()
	if low != 5 || stop != 1 {
		t.Fatalf("tavily pool thresholds = %d/%d, want 5/1", low, stop)
	}
	if tv.pool.Snapshot().PoolSize != 2 {
		t.Fatalf("tavily pool size = %d, want 2", tv.pool.Snapshot().PoolSize)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test -run TestBuildProfiles ./...`
Expected: FAIL — `buildProfiles` undefined.

- [ ] **Step 3: Implement**

Append to `profile.go`:

```go
// buildProfiles constructs the runtime profiles from config. The firecrawl
// (default, unprefixed) profile always comes first. Tavily is appended only
// when TAVILY_API_KEYS is set. Each profile gets its own KeyPool with its own
// thresholds.
func buildProfiles(cfg Config) []*Profile {
	fcPool := NewKeyPool(cfg.APIKeys)
	fcPool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	profiles := []*Profile{{
		Name:           "firecrawl",
		Upstream:       cfg.Upstream,
		UpstreamHost:   cfg.UpstreamHost,
		CreditResetDay: cfg.CreditResetDay,
		RewriteNext:    true,
		pool:           fcPool,
	}}

	if len(cfg.Tavily.APIKeys) > 0 {
		tvPool := NewKeyPool(cfg.Tavily.APIKeys)
		tvPool.SetThresholds(cfg.Tavily.LowCredit, cfg.Tavily.StopCredit)
		host := cfg.Tavily.Upstream
		if i := strings.Index(host, "://"); i >= 0 {
			host = host[i+3:]
		}
		profiles = append(profiles, &Profile{
			Name:           "tavily",
			RoutePrefix:    cfg.Tavily.RoutePrefix,
			Upstream:       cfg.Tavily.Upstream,
			UpstreamHost:   host,
			CreditResetDay: cfg.CreditResetDay,
			RewriteNext:    false,
			pool:           tvPool,
		})
	}
	return profiles
}
```

- [ ] **Step 4: Run tests**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "go test -run TestBuildProfiles ./... && go vet ./..."`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add profile.go profile_test.go
git commit -m "feat(profile): buildProfiles wires Config to runtime profiles

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 5: KeyPool rejection kinds are provider-neutral

**Files:**
- Modify: `keys.go`
- Modify: `keys_test.go`

**Interfaces:**
- Produces: `RecordRejection` kinds are now `"exhausted"` (was `"402"`), `"rate"` (was `"429"`), `"auth"`, `"retry"`. `keyStat.Pay402 int \`json:"402"\`` becomes `Exhausted int \`json:"exhausted"\``; `Rate429 int \`json:"429"\`` becomes `RateLimited int \`json:"rateLimited"\``.
- Consumed by: `rejectKind` in `proxy.go` (already updated in Task 3), `/status` JSON consumers.

- [ ] **Step 1: Update the tests**

In `keys_test.go`, find every `RecordRejection(i, "402")` → `RecordRejection(i, "exhausted")`, `RecordRejection(i, "429")` → `RecordRejection(i, "rate")`, and assertions on `stats.Pay402` → `stats.Exhausted`, `stats.Rate429` → `stats.RateLimited`. Also update the unknown-kind panic test to use a bogus kind (unchanged) and any test asserting all four kinds.

- [ ] **Step 2: Run tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test -run TestKeyPool ./...`
Expected: FAIL — fields renamed / panic on old kinds.

- [ ] **Step 3: Implement**

In `keys.go`:

```go
type keyStat struct {
	Success     int `json:"success"`
	Exhausted   int `json:"exhausted"`
	RateLimited int `json:"rateLimited"`
	Auth        int `json:"auth"`
	Retries     int `json:"retries"`
}
```

and in `RecordRejection`:

```go
	switch kind {
	case "exhausted":
		p.stats[index].Exhausted++
	case "rate":
		p.stats[index].RateLimited++
	case "auth":
		p.stats[index].Auth++
	case "retry":
		p.stats[index].Retries++
	default:
		panic("unknown rejection kind: " + kind)
	}
```

- [ ] **Step 4: Run tests**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test -run TestKeyPool ./...`
Expected: PASS.

- [ ] **Step 5: Commit** (may be folded into Task 3's commit — see Task 3 Step 4 note)

```bash
git add keys.go keys_test.go
git commit -m "refactor(keys): provider-neutral rejection kinds (exhausted/rate/auth/retry)

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 6: Profile-aware credit usage + Tavily `/usage` fetcher

**Files:**
- Modify: `creditusage.go`
- Test: `creditusage_test.go` (modify), `profile_test.go` (tavily fetcher tests)

**Interfaces:**
- Consumes: `Profile` (Task 2).
- Produces:
  ```go
  // Profile-aware entry points (used by proxy.go and refresh.go):
  func refreshKey(p *Profile, client *http.Client, cfg Config, index int, key string, log *logger) int64
  func disableUntilReset(p *Profile, client *http.Client, cfg Config, index int, key string, now time.Time, log *logger)

  // Provider dispatch:
  func fetchUsageFor(p *Profile, client *http.Client, key string, log *logger) usage

  // Tavily:
  func fetchTavilyUsage(client *http.Client, upstream, key string, log *logger) usage
  func tavilyRemaining(keyUsage, keyLimit, planUsage, planLimit, paygoUsage, paygoLimit int64) (int64, bool)
  ```

- [ ] **Step 1: Write the failing tests**

Append to `profile_test.go`:

```go
func TestTavilyRemaining(t *testing.T) {
	cases := []struct {
		name                              string
		keyU, keyL, planU, planL, payU, payL int64
		want                              int64
		wantOK                            bool
	}{
		{"all layers", 150, 1000, 500, 15000, 25, 100, 75, true},       // min(850, 14500, 75)
		{"key layer smallest", 990, 1000, 0, 15000, 0, 100, 10, true},  // min(10, 15000, 100)
		{"plan layer smallest", 0, 1000, 14990, 15000, 0, 100, 10, true},
		{"no key limit (0 = unlimited)", 100, 0, 500, 15000, 25, 100, 75, true},
		{"no paygo limit", 100, 1000, 500, 15000, 0, 0, 900, true},
		{"all unlimited", 100, 0, 500, 0, 25, 0, 0, false},
		{"exhausted key", 1000, 1000, 500, 15000, 25, 100, 0, true},
	}
	for _, c := range cases {
		got, ok := tavilyRemaining(c.keyU, c.keyL, c.planU, c.planL, c.payU, c.payL)
		if got != c.want || ok != c.wantOK {
			t.Errorf("%s: tavilyRemaining = (%d, %v), want (%d, %v)", c.name, got, ok, c.want, c.wantOK)
		}
	}
}

func TestFetchTavilyUsage(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/usage" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tvly-a" {
			w.WriteHeader(401)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"key": {"usage": 150, "limit": 1000},
			"account": {"plan_usage": 500, "plan_limit": 15000, "paygo_usage": 25, "paygo_limit": 100}
		}`))
	}))
	defer fake.Close()

	u := fetchTavilyUsage(fake.Client(), fake.URL, "tvly-a", nil)
	if !u.ok {
		t.Fatal("fetchTavilyUsage failed")
	}
	if u.remaining != 75 {
		t.Fatalf("remaining = %d, want 75 (min of 850/14500/75)", u.remaining)
	}
	if !u.periodEnd.IsZero() {
		t.Fatalf("periodEnd = %v, want zero (tavily returns no period end)", u.periodEnd)
	}
}

func TestFetchTavilyUsage_unauthorized(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer fake.Close()
	if u := fetchTavilyUsage(fake.Client(), fake.URL, "bad", nil); u.ok {
		t.Fatal("expected failure for 401")
	}
}
```

In `creditusage_test.go`, update existing call sites: `refreshKey(pool, client, cfg, ...)` → build a firecrawl `&Profile{Name:"firecrawl", Upstream: cfg.Upstream, UpstreamHost: cfg.UpstreamHost, CreditResetDay: cfg.CreditResetDay, pool: pool}` and call `refreshKey(p, client, cfg, ...)`; same for `disableUntilReset`. (If the existing tests construct via a helper, update the helper.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test -run "TavilyRemaining|FetchTavilyUsage" ./...`
Expected: FAIL — undefined functions.

- [ ] **Step 3: Implement**

In `creditusage.go`:

```go
// fetchUsageFor dispatches to the profile's provider usage endpoint.
func fetchUsageFor(p *Profile, client *http.Client, key string, log *logger) usage {
	if p.Name == "tavily" {
		return fetchTavilyUsage(client, p.Upstream, key, log)
	}
	return fetchUsage(client, p.Upstream, key, log)
}

// fetchTavilyUsage reads a key's usage from GET {upstream}/usage (read-only,
// no credit cost). remaining is the min across the key/plan/paygo limit
// layers; periodEnd is always zero (Tavily exposes no billing period end).
func fetchTavilyUsage(client *http.Client, upstream, key string, log *logger) usage {
	const timeout = 5 * time.Second
	c := client
	if c == nil {
		c = &http.Client{Timeout: timeout}
	} else {
		c = &http.Client{Transport: c.Transport, Timeout: timeout}
	}

	var lastReason string
	for attempt := 0; attempt <= len(usageBackoff); attempt++ {
		u, reason := fetchTavilyUsageOnce(c, upstream, key)
		if u.ok {
			return u
		}
		lastReason = reason
		if !shouldRetryUsage(reason) || attempt >= len(usageBackoff) {
			break
		}
		time.Sleep(usageBackoff[attempt])
	}
	if log != nil {
		log.warn("tavily usage fetch failed", "reason", lastReason, "masked", maskKey(key))
	}
	return usage{}
}

func fetchTavilyUsageOnce(c *http.Client, upstream, key string) (usage, string) {
	req, err := http.NewRequest(http.MethodGet, upstream+"/usage", nil)
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
		Key struct {
			Usage int64 `json:"usage"`
			Limit int64 `json:"limit"`
		} `json:"key"`
		Account struct {
			PlanUsage  int64 `json:"plan_usage"`
			PlanLimit  int64 `json:"plan_limit"`
			PaygoUsage int64 `json:"paygo_usage"`
			PaygoLimit int64 `json:"paygo_limit"`
		} `json:"account"`
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return usage{}, "parse:" + err.Error()
	}
	rem, ok := tavilyRemaining(env.Key.Usage, env.Key.Limit,
		env.Account.PlanUsage, env.Account.PlanLimit,
		env.Account.PaygoUsage, env.Account.PaygoLimit)
	if !ok {
		return usage{}, "parse:no limit layers"
	}
	return usage{remaining: rem, ok: true}, ""
}

// tavilyRemaining computes the effective remaining credits as the minimum over
// the limit layers present. A layer with limit <= 0 is unlimited/unmeasured
// and skipped. ok is false when every layer is unlimited (caller treats the
// key as unmeasured).
func tavilyRemaining(keyUsage, keyLimit, planUsage, planLimit, paygoUsage, paygoLimit int64) (int64, bool) {
	layers := [][2]int64{
		{keyUsage, keyLimit},
		{planUsage, planLimit},
		{paygoUsage, paygoLimit},
	}
	best := int64(-1)
	for _, l := range layers {
		used, limit := l[0], l[1]
		if limit <= 0 {
			continue // unlimited / unmeasured layer
		}
		rem := limit - used
		if rem < 0 {
			rem = 0
		}
		if best < 0 || rem < best {
			best = rem
		}
	}
	if best < 0 {
		return 0, false
	}
	return best, true
}
```

Update `refreshKey` and `disableUntilReset` to take `*Profile` and dispatch:

```go
func refreshKey(p *Profile, client *http.Client, cfg Config, index int, key string, log *logger) int64 {
	u := fetchUsageFor(p, client, key, log)
	if !u.ok {
		return -1
	}
	p.pool.SetCredits(index, u.remaining)
	return u.remaining
}

// disableUntilReset disables key index in the profile's pool. Firecrawl keys
// reset at their real billing-period end when available; Tavily exposes no
// period end, so it always uses the CREDIT_RESET_DAY fallback.
func disableUntilReset(p *Profile, client *http.Client, cfg Config, index int, key string, now time.Time, log *logger) {
	fallback := fallbackReset(now, p.CreditResetDay)
	reset := fallback
	if p.Name != "tavily" {
		u := fetchUsageFor(p, client, key, log)
		if u.ok && !u.periodEnd.IsZero() && !u.periodEnd.Before(now) && !u.periodEnd.After(now.AddDate(1, 0, 0)) {
			reset = u.periodEnd
		}
	}
	p.pool.Disable(index, reset)
}
```

(`cfg` is now unused in these two — remove the parameter and update callers: `refreshKey(p, client, index, key, log)` / `disableUntilReset(p, client, index, key, now, log)`. Update the proxy.go call from Task 3 accordingly.)

- [ ] **Step 4: Run tests**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "go test ./... && go vet ./..."`
Expected: PASS (whole suite, including updated creditusage tests).

- [ ] **Step 5: Commit** (if not folded into Task 3)

```bash
git add creditusage.go creditusage_test.go profile.go profile_test.go proxy.go
git commit -m "feat(credits): profile-aware usage fetch; tavily /usage with min-layer remaining

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 7: Refresher per profile + main.go wiring + server handlers

**Files:**
- Modify: `refresh.go`, `main.go`, `server.go`
- Modify: `refresh_test.go`, `server_test.go`, `main_test.go`

**Interfaces:**
- Consumes: `buildProfiles` (Task 4), `refreshKey(p *Profile, ...)` (Task 6).
- Produces:
  ```go
  func NewRefresher(p *Profile, client *http.Client, cfg Config, log *logger) *Refresher
  func healthzHandler(profiles []*Profile) http.HandlerFunc // 200 if ANY profile usable
  func statusHandler(profiles []*Profile) http.HandlerFunc  // {"profiles": {name: snapshot}}
  ```

- [ ] **Step 1: Update existing tests + add multi-profile tests**

In `refresh_test.go`: each `NewRefresher(pool, client, cfg, log)` call becomes `NewRefresher(p, client, cfg, log)` where `p` is a firecrawl `&Profile{Name: "firecrawl", Upstream: cfg.Upstream, UpstreamHost: cfg.UpstreamHost, pool: pool}`.

In `server_test.go`: update `healthzHandler(pool)` call sites to `healthzHandler([]*Profile{{Name: "firecrawl", pool: pool}})`.

Append to `main_test.go` (or a new block in `server_test.go`):

```go
func TestHealthz_anyProfileUsable(t *testing.T) {
	exhausted := NewKeyPool([]string{"fc-a"})
	exhausted.SetThresholds(10, 2)
	exhausted.SetCredits(0, 0) // below stop threshold
	healthy := NewKeyPool([]string{"tvly-a"})
	healthy.SetThresholds(10, 2)
	healthy.SetCredits(0, 50)

	profiles := []*Profile{
		{Name: "firecrawl", pool: exhausted},
		{Name: "tavily", pool: healthy},
	}
	rec := httptest.NewRecorder()
	healthzHandler(profiles)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("healthz = %d, want 200 (tavily still usable)", rec.Code)
	}

	// Both exhausted -> 503.
	healthy.SetCredits(0, 0)
	rec = httptest.NewRecorder()
	healthzHandler(profiles)(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != 503 {
		t.Fatalf("healthz = %d, want 503 (all profiles exhausted)", rec.Code)
	}
}

func TestStatus_multiProfile(t *testing.T) {
	fc := NewKeyPool([]string{"fc-a"})
	fc.SetThresholds(10, 2)
	tv := NewKeyPool([]string{"tvly-a", "tvly-b"})
	tv.SetThresholds(10, 2)
	profiles := []*Profile{
		{Name: "firecrawl", pool: fc},
		{Name: "tavily", pool: tv},
	}
	rec := httptest.NewRecorder()
	statusHandler(profiles)(rec, httptest.NewRequest(http.MethodGet, "/status", nil))
	if rec.Code != 200 {
		t.Fatalf("status = %d", rec.Code)
	}
	var body struct {
		Profiles map[string]PoolSnapshot `json:"profiles"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Profiles["firecrawl"].PoolSize != 1 {
		t.Fatalf("firecrawl poolSize = %d, want 1", body.Profiles["firecrawl"].PoolSize)
	}
	if body.Profiles["tavily"].PoolSize != 2 {
		t.Fatalf("tavily poolSize = %d, want 2", body.Profiles["tavily"].PoolSize)
	}
}
```

(`server_test.go`/`main_test.go` need `encoding/json`, `net/http`, `net/http/httptest` imports as needed.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine go test -run "TestHealthz|TestStatus" ./...`
Expected: FAIL — signature mismatches.

- [ ] **Step 3: Implement**

In `refresh.go`, change the struct field and constructor:

```go
type Refresher struct {
	profile    *Profile
	client     *http.Client
	cfg        Config
	keys       []string
	log        *logger
	lowInterval time.Duration

	mu        sync.Mutex
	lastLow   []time.Time
	lastDaily []time.Time
}

func NewRefresher(p *Profile, client *http.Client, cfg Config, log *logger) *Refresher {
	n := len(p.pool.Snapshot().Keys)
	return &Refresher{
		profile:     p,
		client:      client,
		cfg:         cfg,
		keys:        p.pool.keys,
		log:         log,
		lowInterval: time.Duration(cfg.CreditRefreshSec) * time.Second,
		lastLow:     make([]time.Time, n),
		lastDaily:   make([]time.Time, n),
	}
}
```

And `refreshOne` calls the profile-aware fetcher:

```go
	got := refreshKey(r.profile, r.client, idx, r.keys[idx], r.log)
```

(Everything else in `refresh.go` unchanged.)

In `server.go`:

```go
func healthzHandler(profiles []*Profile) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		usable := false
		for _, p := range profiles {
			if p.pool != nil && len(p.pool.keys) > 0 && p.pool.AnyUsable() {
				usable = true
				break
			}
		}
		if !usable {
			writeJSON(w, 503, map[string]any{"ok": false})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}
}

func statusHandler(profiles []*Profile) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		snaps := make(map[string]PoolSnapshot, len(profiles))
		for _, p := range profiles {
			snaps[p.Name] = p.pool.Snapshot()
		}
		writeJSON(w, 200, map[string]any{"profiles": snaps})
	}
}
```

In `main.go`, rewrite `buildServer`:

```go
func buildServer() (*http.Server, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}

	profiles := buildProfiles(cfg)
	tr, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
	}
	log := newLogger(cfg.LogLevel)

	for _, p := range profiles {
		p.refresh = NewRefresher(p, client, cfg, log)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(profiles))
	mux.HandleFunc("/status", statusHandler(profiles))
	// Everything else goes to the rotator.
	mux.Handle("/", newRotator(cfg, profiles, client, log))

	// Per-profile background loops: warm-up (self-healing), reset re-enable,
	// daily catch-all refresh. See the original single-profile comments.
	for _, p := range profiles {
		go warmupLoop(p.refresh, log)
		go resetLoop(p.pool, log)
		go dailyRefreshLoop(p.refresh, log)
	}

	log.info("api-key-rotator starting",
		"profiles", len(profiles),
		"keys", len(cfg.APIKeys), "upstream", cfg.Upstream, "maxPasses", cfg.MaxPasses,
		"tavilyKeys", len(cfg.Tavily.APIKeys), "tavilyUpstream", cfg.Tavily.Upstream,
		"tavilyPrefix", cfg.Tavily.RoutePrefix,
		"creditResetDay", cfg.CreditResetDay,
		"lowCreditThreshold", cfg.LowCreditThreshold,
		"stopCreditThreshold", cfg.StopCreditThreshold,
		"creditRefreshSec", cfg.CreditRefreshSec)

	return &http.Server{
		Addr:    cfg.Host + ":" + cfg.Port,
		Handler: mux,
	}, nil
}
```

Update `main_test.go`'s smoke test if it asserts on the `/status` JSON shape (now `{"profiles": {...}}`).

- [ ] **Step 4: Run the full suite**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "go test ./... && go vet ./... && go build -o /tmp/rotator ."`
Expected: PASS, vet clean, build OK.

- [ ] **Step 5: Commit**

```bash
git add refresh.go main.go server.go refresh_test.go server_test.go main_test.go
git commit -m "feat(server): per-profile refresher, multi-profile /status and /healthz

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 8: Rename to api-key-rotator + docs

**Files:**
- Modify: `go.mod`, `Dockerfile`, `docker-compose.yml`, `.github/workflows/build-docker.yml`, `README.md`, `CLAUDE.md`

- [ ] **Step 1: go.mod + Dockerfile**

`go.mod`:
```
module api-key-rotator

go 1.26.4
```

`Dockerfile` — change binary name and entrypoint:

```dockerfile
RUN go build -ldflags="-s -w" -o /out/api-key-rotator .
...
COPY --from=builder /out/api-key-rotator /api-key-rotator
EXPOSE 8788
ENTRYPOINT ["/api-key-rotator"]
```

- [ ] **Step 2: CI workflow**

In `.github/workflows/build-docker.yml`, replace every `firecrawl-rotator` with `api-key-rotator` (image name `ghcr.io/<owner>/api-key-rotator`). Read the file and update image tags/env accordingly.

- [ ] **Step 3: docker-compose.yml**

Rename the service `firecrawl-rotator` → `api-key-rotator`, update the healthcheck test to `["CMD", "/api-key-rotator", "-healthcheck"]`, the commented image reference, and the `firecrawl` service's `FIRECRAWL_API_URL: "http://api-key-rotator:8788"` + `depends_on`. Add the tavily env vars as commented examples:

```yaml
      # TAVILY_API_KEYS: "tvly-key1,tvly-key2"     # optional tavily profile
      # TAVILY_UPSTREAM: "https://api.tavily.com"
      # TAVILY_ROUTE_PREFIX: "/tavily"
```

- [ ] **Step 4: README**

Update `README.md`:
- Title/description: multi-provider key-rotation reverse proxy (Firecrawl + Tavily).
- Config table: add `TAVILY_API_KEYS`, `TAVILY_UPSTREAM`, `TAVILY_ROUTE_PREFIX`, `TAVILY_LOW_CREDIT_THRESHOLD`, `TAVILY_STOP_CREDIT_THRESHOLD` rows.
- New "Tavily profile" section: routing (`/tavily` prefix stripped), rotation policy (401/429 rotate; 432/433 disable until `CREDIT_RESET_DAY`), usage tracking via `GET /usage` with min-layer remaining.
- New "OpenWebUI + Tavily" section with the sed-patched compose `command:` from the spec:
  ```yaml
  command: >
    bash -c "
      sed -i \"s|https://api.tavily.com|http://api-key-rotator:8788/tavily|g\"
        /app/backend/open_webui/retrieval/web/tavily.py
        /app/backend/open_webui/retrieval/loaders/tavily.py
      && bash start.sh"
  ```
- Migration note: repo/image renamed `firecrawl-rotator` → `api-key-rotator`; update compose service name and `FIRECRAWL_API_URL` host.

- [ ] **Step 5: CLAUDE.md**

Update `CLAUDE.md`: project name, the architecture description (profiles + routing), the key-files table (`profile.go`, new `proxy.go` routing), the non-obvious decisions (add: "Tavily rotates purely on status codes, never body text; firecrawl denylist stays firecrawl-only"), commands (Docker test command note).

- [ ] **Step 6: Full verification**

Run: `docker run --rm -v "$PWD":/src -w /src golang:1.26-alpine sh -c "go test ./... && go vet ./... && go build -o /tmp/api-key-rotator ."` and `docker build -t api-key-rotator:test .`
Expected: tests PASS, Docker image builds.

- [ ] **Step 7: Commit**

```bash
git add -A
git commit -m "refactor: rename to api-key-rotator; docs for tavily profile

Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>"
```

---

### Task 9: End-to-end verification against the real compose stack

**Files:**
- Modify: `docker-compose.yml` (if a local verification service is added — optional)

- [ ] **Step 1: Boot the stack**

```bash
docker compose up -d --build
docker compose ps
```
Expected: `api-key-rotator` healthy.

- [ ] **Step 2: Firecrawl path still works**

```bash
curl -s -X POST http://localhost:8788/v2/scrape -H 'Content-Type: application/json' -d '{"url":"https://example.com"}' | head -c 300
```
Expected: a firecrawl response (success or an upstream error — NOT a rotator routing error).

- [ ] **Step 3: Tavily path works**

```bash
curl -s -X POST http://localhost:8788/tavily/search -H 'Content-Type: application/json' -d '{"query":"test","max_results":1}' | head -c 300
curl -s http://localhost:8788/status | python3 -m json.tool
```
Expected: tavily search results; `/status` shows both `firecrawl` and `tavily` profiles with measured `remainingCredits`.

- [ ] **Step 4: Final commit (if anything changed)**

```bash
git add -A && git commit -m "chore: e2e verification fixes" || true
```

---

## Self-Review Notes (already applied)

- Tasks 2/3/5/6 form one compile unit (deleting package-level `shouldRotate` breaks `proxy.go` until Task 3; `rejectKind` kinds must match Task 5). Their steps say to implement together and run the full suite at Task 3 Step 4 / Task 6 Step 4 before committing. Each task's tests are still written first, per task.
- `RequestURI` was replaced by explicit path+query composition in `tryKey` (prefix stripping makes raw `RequestURI` unusable).
- `/tavily/usage` client requests proxy through harmlessly; the refresher's server-side `GET /usage` is what feeds credit tracking.
- Unconfigured-prefix requests fall through to the default profile (byte-compat), per spec's backward-compatibility requirement.
