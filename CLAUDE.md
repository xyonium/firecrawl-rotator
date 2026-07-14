# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`firecrawl-rotator` is a Go reverse proxy that sits between `firecrawl-mcp` and the Firecrawl API (`api.firecrawl.dev`). It holds a pool of Firecrawl API keys, injects one per request, and rotates to the next key when one is rejected (credit exhaustion / rate limit / bad key), retrying transparently. It also rewrites Firecrawl's `next` pagination URLs so crawl pagination flows back through the proxy and stays under rotation. The whole point: point firecrawl-mcp's `FIRECRAWL_API_URL` at this proxy and get key rotation with **zero changes** to firecrawl-mcp.

Stdlib-only (`go.mod` has no dependencies), single binary, no external state.

## Commands

```bash
go test ./...                      # run all tests
go test -run TestRotator_RotatesOn402 ./...   # single test
go test -run TestRotator ./...      # all tests matching a pattern
go vet ./...
go build -o rotator .               # build binary

FIRECRAWL_API_KEYS=fc-x ./rotator   # run locally
./rotator -healthcheck              # GET /healthz on 127.0.0.1:PORT, exit 0/1 (Docker healthcheck)

docker compose up -d --build        # run via compose (rotator + reference mcpo+firecrawl-mcp)
```

CI (`.github/workflows/build-docker.yml`) builds and pushes `ghcr.io/<repo>:latest` + `:sha` on push to main when `Dockerfile`, `*.go`, `go.mod`, or the workflow change.

## Architecture

Request flow lives in `proxy.go`'s `rotator.ServeHTTP`. Everything else exists to support it:

1. **Buffer request body once** (`io.ReadAll`), then replay it across retry attempts - Firecrawl requests are not idempotent-safe to re-send, so the body must be re-readable.
2. **Rotation loop** (`maxRotations = MaxPasses * poolSize`): pick the best key via `pool.Current()` (highest `remainingCredits` above the stop threshold, skipping cooled-down keys). Call `tryKey`:
   - `tryKey` sends the request and, on a **transient** error (`shouldRetry`: network error, 403, 408, 5xx), retries the **same key** with exponential backoff `500ms/1s/2s/4s/8s` before returning. It only signals "give up on this key" (`netErr=true`) after backoff is exhausted.
   - **Over `MAX_BODY_BYTES`** -> forward untouched, break (no rotate, no rewrite).
   - **`shouldRotate` true** (402/429/401, or a `success:false` envelope whose text matches the denylist) -> record rejection, disable if `isCreditExhausted`, `Advance` (cools the key down ~30s), retry next key.
   - **Otherwise** -> record success, `Decrement` predicted credits by `creditsUsed` (or 1), trigger `MaybeRefreshLow`, break.
3. **After loop**: if no usable key, return `503`; if JSON, rewrite `next` URLs (`rewrite.go`).

Key files and their roles:

| File | Responsibility |
|------|----------------|
| `main.go` | `buildServer` wires `Config` -> `KeyPool` (with `SetThresholds`) -> `transport` -> `http.Client` (30s) -> `Refresher` -> `rotator`. Routes `/healthz`, `/status`, `/`. `--healthcheck` flag. Starts goroutines: `RefreshAll` warm-up, `resetLoop` (re-enable past reset), `dailyRefreshLoop` (24h catch-all). |
| `config.go` | `LoadConfig` parses all env vars (see README) with validation, including `LOW_CREDIT_THRESHOLD`/`STOP_CREDIT_THRESHOLD`/`CREDIT_REFRESH_INTERVAL` (stop must be <= low). `fallbackReset` computes the `CREDIT_RESET_DAY` fallback reset instant. |
| `keys.go` | `KeyPool` - per-key `stats`, `disabled`/`disabledUntil`, `remainingCredits` (MaxInt64 = unmeasured), `cooldownUntil`. `Current`/`currentLocked` pick highest-credit usable key, skipping cooled-down keys (fallback to them if all cooled). `Advance` cools the current key ~30s so equal-credit keys actually rotate. `Decrement`/`SetCredits` adjust predicted/real balances. `AnyUsable` checks >= stop threshold. `Snapshot` masks keys + reports `remainingCredits` (-1 = unmeasured). |
| `proxy.go` | The rotator: rotation loop + `tryKey` (backoff retries on transient), header copying, body cap, disable-on-credit-exhaustion, credit decrement on success, rewrite+guard, 503 when no usable key. `backoffSchedule`, `extractCreditsUsed`, `readCapped`, `writeRawResponse`, `isHopByHop`. |
| `rotate.go` | `shouldRotate` (402/429/401 + denylist against failure envelopes only - NOT 403), `shouldRetry` (network/403/408/5xx = transient, backoff same key), `isCreditExhausted` (402 / credits-in-envelope -> disable). Single source of truth for reject-vs-retry-vs-disable. |
| `refresh.go` | `Refresher` applies the on-demand credit-refresh strategy: `OnSwitch` (refresh switched-off + switched-to keys), `MaybeRefreshLow` (refresh a key whose predicted balance < 100, throttled to `CREDIT_REFRESH_INTERVAL`), `DailyRefresh` (24h catch-all), `RefreshAll` (startup warm-up). All refreshes run in background goroutines so the request path never blocks. |
| `rewrite.go` | `rewriteNext` rewrites **only** `"next"` keys with absolute URLs on `upstreamHost`. `paginationGuard` warns on non-terminal crawl status with no `next`. Other host occurrences are never rewritten (may be scraped content). |
| `transport.go` | `buildTransport` - `UPSTREAM_PROXY` wins; else `http.ProxyFromEnvironment` (curl-style). `ForceAttemptHTTP2: true`. |
| `creditusage.go` | `fetchUsage` reads a key's `remainingCredits` + `billingPeriodEnd` from one `GET /v2/team/credit-usage` (read-only, no credit cost). `refreshKey` applies it to the pool. `disableUntilReset` ties reset to `CREDIT_RESET_DAY` fallback. |
| `server.go` | `logger` (stderr, key=value; `LOG_LEVEL=debug`), `healthzHandler` (503 when no usable key), `statusHandler`. |

### Non-obvious design decisions (respect these when editing)

- **Selection is credit-based, not round-robin.** `Current()` picks the highest-`remainingCredits` usable key. To make rotation actually rotate when credits are equal (e.g. all unmeasured at startup, or all 1000), a rotated-off key gets a ~30s **cooldown** (`Advance` sets it; `RecordSuccess` clears it). Don't "simplify" this away or equal-credit keys will thrash on the same index.
- **403 is transient, not a key rejection.** A 403 is usually an edge/WAF/network-layer issue, so `tryKey` retries it with backoff on the **same key** (`shouldRetry`), and only rotates after backoff is exhausted. It is NOT in `shouldRotate` and never disables. Moving 403 back to rotate/disable would reintroduce the production storm where every key 403s and all get disabled.
- **A `success:true` response NEVER rotates.** The denylist is checked only against failure envelopes (`success:false` or 4xx), never the whole body. Scraped content routinely contains "rate limit"/"payment required"/"credits"; scanning the whole body (the original bug) misclassified good responses and burned credits.
- **Credit-exhausted keys are disabled, not retried.** A genuine 402 / `success:false`+credits envelope disables the key until its reset instant (queried per-key via `/v2/team/credit-usage`); 429/401 rotate-but-keep. Disabling on rate-limit/auth would take a good key offline.
- **Predicted credits decrement between refreshes.** `Decrement` subtracts `creditsUsed` (or 1) on success so selection stays roughly correct without a refresh per request. Unmeasured keys (MaxInt64) are never decremented. `Refresher` corrects drift: on switch, when predicted < 100 (throttled), and daily.
- **`Current()`/`Advance()` lock independently.** Concurrent requests can pick the same key; a per-request lock would serialize all upstream calls. This is deliberate. A good key is found within `MaxPasses` sweeps.
- **`next`-URL rewriting is intentionally narrow.** Only `"next"` keys with absolute URLs on the upstream host are rewritten. Never broaden this - scraped content can legitimately contain the upstream host.
- **`proxyBase` is derived from `req.Host`** when `PROXY_BASE_URL` is unset.
- **Body cap (`MAX_BODY_BYTES`) is a hard boundary.** Above it, forwarded untouched, no rotate/rewrite. `0` = no cap.
- **No external dependencies.** Stdlib only. Keep it that way.
- **Package is `main`**; tests use `httptest` fakes. When testing the rotator, override `backoffSchedule` to short durations (see `testRotator` + `cfgFor` helpers in `proxy_test.go`) so backoff tests don't sleep ~15s.
Design rationale: `docs/superpowers/specs/2026-07-09-firecrawl-token-rotation-design.md`; implementation plan: `docs/superpowers/plans/2026-07-09-firecrawl-token-rotation-plan.md`.

## CodeGraph

This repo is indexed by CodeGraph (`.codegraph/` exists). Prefer `codegraph_explore` (MCP) or `codegraph explore "<symbol>"` (shell) over grep/Read when locating or understanding code.
