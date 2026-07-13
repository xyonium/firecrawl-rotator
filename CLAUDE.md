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

1. **Buffer request body once** (`io.ReadAll`), then replay it across retry attempts — Firecrawl requests are not idempotent-safe to re-send, so the body must be re-readable.
2. **Retry loop** (`maxAttempts = MaxPasses * poolSize`): get current key via `pool.Current()` → build upstream request with `Authorization: Bearer <key>` → `client.Do`. On each attempt:
   - **Network error** → return 502 immediately, **do not rotate** (not a key problem).
   - **Response over `MAX_BODY_BYTES`** → forward untouched, break (no rotate, no rewrite).
   - **`shouldRotate` true** (status 402/429/401/403, or a `success:false` failure envelope whose error text matches the denylist in `rotate.go`) → record rejection, advance, retry next key. If `isCreditExhausted` (402 / credits-in-envelope), also **disable** the key until its per-key billing reset.
   - **Otherwise** → record success, break.
3. **After loop**: if all keys disabled, return `503`. If JSON response, rewrite `next` URLs (`rewrite.go`) to point at the proxy so pagination stays under rotation.

Key files and their roles:

| File | Responsibility |
|------|----------------|
| `main.go` | `buildServer` wires `Config` → `KeyPool` → `transport` → `http.Client` (30s timeout) → `rotator`. `mux` routes `/healthz`, `/status`, and `/` (catch-all to rotator). `--healthcheck` flag for Docker. Starts `resetLoop` goroutine that re-enables keys whose reset instant has passed. |
| `config.go` | `LoadConfig` parses all env vars (see README table) with validation. `UPSTREAM` and `UPSTREAM_PROXY` are parsed as URLs and scheme-checked. `fallbackReset` computes the `CREDIT_RESET_DAY`-based fallback reset instant. |
| `keys.go` | `KeyPool` - mutex-guarded slice of keys + per-key stats + per-key `disabled`/`disabledUntil`. `Current`/`Advance` skip disabled keys and return `(-1,"")` when all are disabled; they lock independently (intentional approximate round-robin under concurrency; see comment in `proxy.go`). `ReenableDue` clears disables past their reset time. `Snapshot` masks keys to last 4 chars for `/status`. |
| `proxy.go` | The rotator: retry loop, header copying (strips hop-by-hop + replaces `Authorization`/`Host`), body cap, disable-on-credit-exhaustion, rewrite+guard on final response, 503 when all disabled. Also `readCapped`, `writeRawResponse`, `isHopByHop`. |
| `rotate.go` | `shouldRotate(status, body)` - status-code set (402/429/401/403) **plus** denylist, but the denylist is checked **only against Firecrawl failure envelopes** (`success:false` or 4xx), never a `success:true` body. `isCreditExhausted` separates "disable the key" (402 / credits-in-envelope) from rotate-but-keep (429/401/403). Single source of truth for "is this a key-level rejection". |
| `rewrite.go` | `rewriteNext` walks decoded JSON and rewrites **only** keys literally named `"next"` whose value is an absolute URL on `upstreamHost`. `paginationGuard` warns when a non-terminal crawl status has no `next` (field may have been renamed). Other host occurrences in bodies are **never** rewritten (may be scraped content). |
| `transport.go` | `buildTransport` — `UPSTREAM_PROXY` wins; else `http.ProxyFromEnvironment` (curl-style `HTTPS_PROXY`/`HTTP_PROXY`/`NO_PROXY`, read at request time). `ForceAttemptHTTP2: true`. |
| `creditusage.go` | `fetchReset` queries a key's own `GET /v2/team/credit-usage` -> `billingPeriodEnd` (read-only, costs no credits) for that key's real reset instant. `disableUntilReset` ties it to the `CREDIT_RESET_DAY` fallback. Per-key reset matters: each key belongs to a separate account with its own billing anniversary. |
| `server.go` | `logger` (stderr, key=value, `LOG_LEVEL=debug` adds per-request lines), `healthzHandler` (503 when no usable key), `statusHandler`. |

### Non-obvious design decisions (respect these when editing)

- **A `success:true` response NEVER rotates.** The body denylist is checked only against Firecrawl failure envelopes (`success:false` or 4xx), never the whole body. Scraped content routinely contains words like "rate limit" / "payment required" / "credits" - scanning the whole body (the old behavior) misclassified good responses as rejections, causing duplicate upstream calls and credit burn. This was the original production bug.
- **Credit-exhausted keys are disabled, not retried.** A genuine 402 / `success:false`+credits envelope disables the key until its reset instant (queried per-key via `/v2/team/credit-usage`); 429/401/403 rotate but do NOT disable (transient/global). Disabling a key on a rate-limit or auth error would take a good key offline.
- **Rotation is approximate round-robin, not strict.** `Current()`/`Advance()` lock independently, so concurrent requests can read the same key and both advance. This is deliberate — a per-request lock would serialize all upstream calls. A good key is still found within `MaxPasses` full sweeps. Don't "fix" this by holding the lock across the upstream call.
- **5xx and network errors do NOT rotate.** Only key-level rejections rotate. A 500 is an upstream problem, not a key problem.
- **`next`-URL rewriting is intentionally narrow.** Only object keys named `"next"` with absolute URLs on the upstream host are rewritten. Never broaden this to rewrite the host string anywhere in the body — scraped page content can legitimately contain the upstream host.
- **`proxyBase` is derived from `req.Host`** when `PROXY_BASE_URL` is unset, so rewritten `next` URLs point back to whatever address the caller (firecrawl-mcp) used to reach the proxy.
- **Body cap (`MAX_BODY_BYTES`) is a hard boundary.** Above it, the response is forwarded untouched with no rotation or rewriting. `0` = no cap. This protects memory from huge crawl responses.
- **No external dependencies.** Everything is the Go stdlib. Keep it that way.
- **Package is `main`** — all files are in one package; tests (`*_test.go`) use `httptest` fake backends extensively. When testing the rotator, follow the `newFakeBackend` pattern in `proxy_test.go`.

Design rationale: `docs/superpowers/specs/2026-07-09-firecrawl-token-rotation-design.md`; implementation plan: `docs/superpowers/plans/2026-07-09-firecrawl-token-rotation-plan.md`.

## CodeGraph

This repo is indexed by CodeGraph (`.codegraph/` exists). Prefer `codegraph_explore` (MCP) or `codegraph explore "<symbol>"` (shell) over grep/Read when locating or understanding code.
