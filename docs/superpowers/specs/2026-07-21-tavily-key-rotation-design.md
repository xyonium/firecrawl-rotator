# Multi-Provider Key Rotation (Tavily support + rename to api-key-rotator) — Design

**Date:** 2026-07-21
**Status:** Draft (awaiting user review)
**Repo:** `firecrawl-rotator` → renamed **`api-key-rotator`**
**Prior spec:** `docs/superpowers/specs/2026-07-09-firecrawl-token-rotation-design.md`

## Problem

The rotator currently only serves Firecrawl. The user also runs OpenWebUI's
Tavily integration (`WEB_SEARCH_ENGINE=tavily`, `WEB_LOADER_ENGINE=tavily`)
with a single `TAVILY_API_KEY`, and wants the same key-rotation behavior when a
Tavily key hits its plan/pay-as-you-go limit or rate limit — **without
modifying OpenWebUI source in a maintained fork**.

## Established facts (from research, verified against docs + source)

- OpenWebUI hardcodes Tavily URLs (`https://api.tavily.com/search` in
  `backend/open_webui/retrieval/web/tavily.py`, `/extract` in
  `backend/open_webui/retrieval/loaders/tavily.py`). There is **no**
  `TAVILY_API_URL` / base-URL env var, unlike Firecrawl.
- Tavily auth is `Authorization: Bearer <key>` header only — same shape as
  Firecrawl, so the proxy can swap the key by rewriting one header.
- Tavily error codes: `401` invalid/missing key, `429` rate limit (with
  `retry-after`), `432` plan usage limit exceeded, `433` pay-as-you-go limit
  exceeded. Error bodies are `{"detail": {"error": "..."}}` (429: `{"error": ...}`).
- Tavily **has** a free read-only usage endpoint: `GET https://api.tavily.com/usage`
  (Bearer auth). Returns `{"key": {"usage": N, "limit": M, ...}, "account":
  {"plan_usage", "plan_limit", "paygo_usage", "paygo_limit", ...}}`. This makes
  full credit tracking (not just status-code-driven rotation) feasible.
- The user's OpenWebUI container already applies a `sed` patch at startup via
  compose `command:` (to firecrawl.py). So pointing Tavily at the rotator can
  be done the same way — no fork, no extra_hosts, no TLS certificates.

## Approach chosen

**Point OpenWebUI at the rotator by `sed`-patching the two hardcoded Tavily
URLs at container startup**, exactly like the existing firecrawl.py patch:

```yaml
command: >
  bash -c "
    sed -i \"s|https://api.tavily.com|http://api-key-rotator:8788/tavily|g\"
      /app/backend/open_webui/retrieval/web/tavily.py
      /app/backend/open_webui/retrieval/loaders/tavily.py
    && bash start.sh"
```

`TAVILY_API_KEY` in OpenWebUI is set to any placeholder; the rotator replaces
the `Authorization` header with a pooled key per request.

Rejected alternatives:

- **`extra_hosts` pointing `api.tavily.com` at the rotator** — requires the
  rotator to serve TLS with a cert for `api.tavily.com`, hence a private CA
  injected into OpenWebUI's trust store. Heavy and fragile. Rejected.
- **Separate `tavily-rotator` container/repo** — duplicates the entire pool /
  rotation / cooldown / status machinery. Rejected in favor of generalizing
  this repo.
- **Upstream PR to OpenWebUI for `TAVILY_API_URL`** — right long-term, but not
  usable now; the sed patch achieves the same redirect with zero upstream
  dependency. (Can be dropped if the PR ever lands.)

## Architecture

The binary generalizes from "Firecrawl proxy" to "multi-profile key-rotation
reverse proxy". One binary, one listen port, N **profiles** routed by URL path
prefix:

```
                    ┌─ (no prefix)  ──► firecrawl profile ──► api.firecrawl.dev
OpenWebUI ──► :8788 ┤                  (FIRECRAWL_API_KEYS, credit tracking,
                    │                   next-URL rewrite)
                    └─ /tavily/*    ──► tavily profile   ──► api.tavily.com
                                       (TAVILY_API_KEYS, credit tracking via
                                        GET /usage, prefix stripped)
```

A **profile** = { route prefix, upstream URL/host, KeyPool, refresh strategy,
rotate/disable status-code set, response rewriter }. The rotation loop,
backoff, cooldown, and key-selection logic are shared unchanged; only the
per-provider policy knobs differ.

### Routing

- Request path is matched against configured profile prefixes (longest match).
- Default (Firecrawl) profile has **no prefix** and is the fallback, preserving
  today's behavior for existing deployments bit-for-bit.
- The Tavily profile's prefix (`/tavily`, configurable) is stripped before
  forwarding: `/tavily/search` → `https://api.tavily.com/search`.
- A request path that matches no profile prefix goes to the default profile.
- The `/usage` self-endpoint: since Tavily's usage path lives under the same
  upstream, the refresher calls it server-side with the pool key; client
  requests to `/tavily/usage` are proxied like any other path (harmless).

### Per-profile policy

| Concern | Firecrawl (default, unchanged) | Tavily |
|---|---|---|
| Route prefix | (none) | `/tavily` (env-configurable) |
| Rotate (keep key) | 429, 401 | 429, 401 |
| Disable key | 402 / credits-exhausted envelope | 432, 433 |
| Transient retry (same key, backoff) | network, 403, 408, 5xx | same |
| Credit usage endpoint | `GET /v2/team/credit-usage` | `GET /usage` |
| Credit field | `remainingCredits` + `billingPeriodEnd` | `min` of non-positive-skipped layers: `key.limit-key.usage`, `account.plan_limit-plan_usage`, `account.paygo_limit-paygo_usage`; a layer with `limit <= 0` or absent is skipped (unlimited/unmeasured) |
| Reset instant | `billingPeriodEnd`, else `CREDIT_RESET_DAY` fallback | `CREDIT_RESET_DAY` fallback only (Tavily returns no period end) |
| Success-envelope denylist scan | yes (failure envelopes only) | no (status codes are sufficient and unambiguous) |
| `next`-URL rewrite | yes | no (Tavily search/extract return no `next` pagination URLs) |
| Predicted decrement | `creditsUsed` or 1 | 1 per successful request (Tavily has no `creditsUsed` field; search=1 credit, extract counts per URL — decrement 1 per request is a lower bound, refresher corrects drift) |

### Credit tracking for Tavily

The `Refresher` machinery (`OnSwitch`, `MaybeRefreshLow`, `DailyRefresh`,
`RefreshAll`) is reused with a per-profile `usageFetcher` function:

- Firecrawl: existing `fetchUsage` against `/v2/team/credit-usage`.
- Tavily: new `fetchTavilyUsage` against `/usage`, mapping to
  `remainingCredits = min(layers)` as above. Thresholds
  (`LOW_CREDIT_THRESHOLD` / `STOP_CREDIT_THRESHOLD`) apply per-profile; Tavily
  credits are roughly 1 credit = 1 search, so the same defaults (10/2) are
  sensible; separate `TAVILY_LOW_CREDIT_THRESHOLD` / `TAVILY_STOP_CREDIT_THRESHOLD`
  env vars are added for independent tuning, defaulting to the shared values.

- `TAVILY_ROUTE_PREFIX` must not shadow the reserved paths `/healthz` or
  `/status` (prefix `/` or `/s` etc. would); config validation rejects this at
  startup. The default `/tavily` is safe.

If `/usage` fails for a key, that key stays unmeasured (MaxInt64) and the
existing self-healing warm-up retry logic applies unchanged.

### Key disable semantics on 432/433

- `432` (plan limit) / `433` (paygo limit) disable the key until the fallback
  reset instant (`CREDIT_RESET_DAY`), same as Firecrawl's 402 path. Because
  432/433 are often **account-level**, other keys of the same account will also
  fail and disable themselves on their own 432/433 — the pool converges to 503
  only when every key is genuinely exhausted, which is the correct behavior.
- `401` / `429` rotate-but-keep (key may recover), unchanged semantics.

## Config (env vars)

New:

| Var | Default | Purpose |
|---|---|---|
| `TAVILY_API_KEYS` | (unset) | Comma-separated Tavily key pool. Unset = Tavily profile disabled entirely. |
| `TAVILY_UPSTREAM` | `https://api.tavily.com` | Tavily upstream base. |
| `TAVILY_ROUTE_PREFIX` | `/tavily` | Path prefix routed to the Tavily profile. Must start with `/`. |
| `TAVILY_LOW_CREDIT_THRESHOLD` | `LOW_CREDIT_THRESHOLD` | Per-profile low-credit switch threshold. |
| `TAVILY_STOP_CREDIT_THRESHOLD` | `STOP_CREDIT_THRESHOLD` | Per-profile stop threshold (must be <= its low threshold). |

Unchanged: everything else (`FIRECRAWL_API_KEYS` stays the default-profile key
source; `UPSTREAM`, `PORT`, `CREDIT_RESET_DAY`, refresh interval, etc.).

Backward compatibility: with `TAVILY_API_KEYS` unset, behavior is byte-identical
to today (single Firecrawl profile, no prefix routing).

## Components / code changes

- **`config.go`** — parse Tavily env vars into a `[]ProfileConfig`; existing
  fields become the default profile. Validate prefix (leading `/`, no trailing
  `/`, no overlap with reserved paths `/healthz`, `/status`).
- **new `profile.go`** — `Profile` struct: name, routePrefix, upstream,
  upstreamHost, pool, refresher, fetchUsage func, shouldRotate/isCreditExhausted
  status sets, rewriteEnabled flag.
- **`proxy.go`** — `rotator` holds `[]*Profile` + default; `ServeHTTP` resolves
  profile by longest-prefix match, strips prefix, then runs the existing loop
  against that profile. The loop's hardcoded status-code checks are replaced by
  the profile's policy sets.
- **`rotate.go`** — `shouldRotate` / `isCreditExhausted` take the profile's
  status sets instead of package-level constants. Firecrawl denylist logic
  becomes Firecrawl-profile-specific.
- **`creditusage.go`** — extract provider-neutral `refreshKey`; Firecrawl's
  `fetchUsage` stays; add `fetchTavilyUsage` (+ limit-layer min logic + tests).
- **`refresh.go`** — `Refresher` is constructed per profile with that profile's
  fetcher; no behavioral change otherwise.
- **`rewrite.go`** — invoked only when `profile.rewriteEnabled`.
- **`main.go`** — build profiles from config, wire pools/refresher per profile,
  same goroutines per profile (warm-up, resetLoop, dailyRefreshLoop).
- **`server.go`** — `/status` reports all profiles keyed by name.
  `/healthz` returns 200 if **any** enabled profile has a usable key, 503 only
  when every enabled profile is exhausted (one provider's exhaustion must not
  kill the container's health while the other provider still serves traffic).
- **Rename** — module path, binary name (`api-key-rotator`), Dockerfile,
  compose service/image name, README, CLAUDE.md. GitHub repo renamed via
  settings (old URL 301-redirects). CI workflow image name updated.

## Error handling

- All keys of the matched profile exhausted → `503` (unchanged semantics).
- Profile disabled (`TAVILY_API_KEYS` unset) but request arrives with its
  prefix → `404` with a clear JSON error (`{"error":"tavily profile not configured"}`),
  not a silent fallthrough to Firecrawl.
- Prefix collision with reserved paths → config validation error at startup.
- Network/5xx/403/408 → existing per-key backoff (`tryKey`), unchanged.

## Testing

- Existing Firecrawl tests must stay green untouched (byte-compat guarantee).
- New `httptest` suites for the Tavily profile:
  - 432 → disable + rotate; 433 → disable + rotate
  - 429/401 → rotate, key kept
  - success → no rotate, decrement 1
  - prefix stripping (`/tavily/search` → upstream `/search`)
  - unprefixed request still hits Firecrawl profile
  - `fetchTavilyUsage` min-layer computation (incl. `limit: 0` / missing layer
    skipped; all layers unlimited → unmeasured)
  - profile-not-configured → 404
  - `/status` shows both profiles
- Rename: build produces `api-key-rotator` binary; Dockerfile unchanged
  functionally.

## Deployment (compose sketch)

```yaml
services:
  api-key-rotator:
    image: ghcr.io/<owner>/api-key-rotator:latest
    environment:
      FIRECRAWL_API_KEYS: fc-a,fc-b
      TAVILY_API_KEYS: tvly-a,tvly-b
  openwebui:
    environment:
      WEB_SEARCH_ENGINE: tavily
      TAVILY_API_KEY: placeholder   # rotator replaces it
      WEB_LOADER_ENGINE: tavily
    command: >
      bash -c "
        sed -i \"s|https://api.tavily.com|http://api-key-rotator:8788/tavily|g\"
          /app/backend/open_webui/retrieval/web/tavily.py
          /app/backend/open_webui/retrieval/loaders/tavily.py
        && bash start.sh"
```

## Rename plan

1. Code/docs/Dockerfile/CI updated to `api-key-rotator` in the implementation PR.
2. GitHub repo renamed after merge (redirects keep old links/CI image refs
   alive briefly; README badge/image refs updated in the PR).
3. Compose service renamed `firecrawl-rotator` → `api-key-rotator`; users'
   sed targets updated accordingly (documented in README migration note).

## Non-goals (YAGNI)

- No third provider. The profile abstraction is kept minimal (a slice, not a
  plugin system) — adding Brave/SerpAPI later is a small diff, but we don't
  build config-file-driven dynamic providers now.
- No mitmproxy / TLS / extra_hosts support.
- No per-URL Tavily credit accounting (extract with 5 URLs costs 5 credits;
  we decrement 1 and let the refresher correct — documented, acceptable drift).
- No use of Tavily's keyless mode or enterprise key-management endpoints.
