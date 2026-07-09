# Firecrawl Token Rotation Proxy — Design

**Date:** 2026-07-09
**Status:** Draft (awaiting user review)
**Repo:** `firecrawl-rotator` (separate from `firecrawl-mcp`)

## Problem

The user runs the published `firecrawl-mcp` package under `mcpo` (Open WebUI's
`mcpo:latest` image) over stdio, with a single `FIRECRAWL_API_KEY`. When that
key's credits are exhausted, or it is rate-limited / rejected, every Firecrawl
tool call fails until a human swaps the key. The user wants automatic rotation
across a pool of keys on rejection, attached "in front of" the MCP entry point
without modifying firecrawl-mcp.

## Architecture

A new standalone reverse proxy, `firecrawl-rotator`, sits between firecrawl-mcp
and `api.firecrawl.dev`:

```
mcpo  ──stdio──▶  firecrawl-mcp  ──HTTP──▶  firecrawl-rotator  ──HTTPS──▶  api.firecrawl.dev
                  (FIRECRAWL_API_URL              (holds key pool,
                   = http://firecrawl-rotator:    rotates on 402/429/401/403,
                   PORT)                           rewrites `next` URLs)
```

`firecrawl-mcp` stays as the stock `npx -y firecrawl-mcp`. The only config
change: `FIRECRAWL_API_KEY` is removed and replaced with
`FIRECRAWL_API_URL=http://firecrawl-rotator:PORT`. firecrawl-mcp treats a set
`FIRECRAWL_API_URL` as self-hosted mode and does not require a key; the proxy
injects one per request.

The rotator imports no firecrawl-mcp code. It is a pure HTTP reverse proxy that
only ever sees traffic to `api.firecrawl.dev`. Hence a separate repo.

### Why a proxy, not a fork or stdio dispatcher

- **Fork of firecrawl-mcp:** rotation happens inside a tool call, when the
  upstream HTTP response comes back. Embedding it in firecrawl-mcp means
  maintaining a fork and rebuilding on every upstream update. Rejected.
- **stdio dispatcher (N keyed children):** the stdio/JSON-RPC layer cannot
  reliably distinguish a credit-exhaustion error from a real content error, so
  rejection-based rotation is fragile at that seam. Rejected.
- **External proxy:** firecrawl-mcp already forwards all upstream calls to
  `FIRECRAWL_API_URL`, so a proxy there gives rotation across every tool with
  zero changes to firecrawl-mcp. Chosen.

### Crawl `next`-URL problem (the load-bearing detail)

firecrawl-mcp paginates crawl results by following a `next` field in the
response (`src/index.ts:1487`, `client.http.get(current, ...)`). If Firecrawl
returns that `next` value as an **absolute** URL to `api.firecrawl.dev`, the
follow-up page fetch goes straight to Firecrawl and **bypasses the proxy**,
losing rotation. The same can happen for any absolute Firecrawl URL returned in
any response body.

**Solution (Approach A):** the proxy rewrites `next` (and only `next`) fields
in JSON response bodies whose value is an absolute URL pointing at the upstream
host, replacing the host with the proxy's own address. firecrawl-mcp then
follows the rewritten URL back through the proxy, which strips its prefix,
forwards to Firecrawl with a rotated key, and rewrites again. Rotation is
uniform across all tools, including crawl pagination.

## Language and image

Go. The stdlib's `httputil.ReverseProxy` does forwarding/streaming/header
handling for free; its `ModifyResponse` hook is the natural home for both the
rotation decision and the `next`-URL rewriting. `go test` is built in.

Multi-stage build. Final image is `scratch` + a static binary + CA roots copied
from the builder. Expected final image ~8–12 MB, vs ~85 MB for the smallest
Node image — Go is ~6–8× smaller because there is no runtime.

## Components

### 1. Key pool

- Loaded once at startup from `FIRECRAWL_API_KEYS` (comma-separated, whitespace
  trimmed, empties dropped).
- If the pool is empty after parsing, log and `exit(1)` (fail fast, same
  pattern firecrawl-mcp uses for missing creds).
- Held in memory as a slice. A shared mutex guards a `currentIndex int`.
- No persistence, no hot-reload (env-var source per the user's choice).

### 2. Forwarder (request side)

For each incoming request:

- Read method, path, query string, body, headers.
- Drop incoming `Authorization` and `Host` headers (the proxy owns both).
- Pick `keys[currentIndex]`; set `Authorization: Bearer <key>`.
- Preserve `X-Origin`, `Content-Type`, and other pass-through headers.
- Forward to `UPSTREAM<path>?<query>` (default `https://api.firecrawl.dev`).
- Buffer the upstream response body so the rotation decider and `next`-rewriter
  can inspect/modify it before it is returned (and before a retry decision).
  Buffering is **required** by rotation: a retry can only be chosen after seeing
  the body, and a stream cannot be inspected-and-then-replayed.

Buffering is the default because rotation needs it. To bound memory, the proxy
enforces a `MAX_BODY_BYTES` cap (default `16 MiB`); a response body larger than
that is forwarded to the caller **without** inspection/rewriting (rotation
skipped for that one response, `next` not rewritten), and a warning is logged.
Firecrawl JSON responses are normally far under this; the cap exists only to
prevent a pathological payload from holding memory. `MAX_BODY_BYTES=0` disables
the cap (buffer unconditionally) if you prefer the original behavior.

Forwarding is done with a direct `http.Client` round-trip per attempt (a single
`httputil.ReverseProxy` cannot loop on its `ModifyResponse`, so the retry loop
owns the round-trips). `httputil.ReverseProxy`'s header-copying logic is reused
as a helper for dropping/injecting headers, but the proxy does not use
`ReverseProxy.ServeHTTP` as its transport.

### 2.1 Outbound proxy (egress to Firecrawl)

The rotator's own egress to `UPSTREAM` can optionally go through a forward
proxy (corporate egress, a caching proxy, a SOCKS/HTTP tunnel). Two sources,
in priority order:

1. **Explicit setting** `UPSTREAM_PROXY` (e.g. `http://proxy.corp:3128`,
   `https://...`, `socks5://host:1080`). If set, it wins.
2. **System/curl-style env vars** - `HTTPS_PROXY`/`https_proxy` (for the
   `https://api.firecrawl.dev` upstream), `HTTP_PROXY`/`http_proxy`, and
   `NO_PROXY`/`no_proxy`. Honored via Go's stdlib
   `httpproxy.FromEnvironment().ProxyFunc()` so the semantics match `curl`/
   `wget` exactly (including `NO_PROXY` wildcards). This is what "use system
   curl proxy setting" means in practice.

Implementation: build a single `http.Transport` at startup. If an outbound
proxy is resolved, set `Transport.Proxy` to a function returning it; otherwise
leave `Transport.Proxy` nil (direct). The `http.Client` used by the forwarder
is backed by this transport. `NO_PROXY` matching is delegated to the stdlib
proxy func so a host listed there bypasses the proxy even when
`UPSTREAM_PROXY`/`HTTPS_PROXY` is set.

- An invalid `UPSTREAM_PROXY` URL (unparseable, or an unsupported scheme) is a
  startup error: log and `exit(1)`.
- If the upstream itself is reached *via* the outbound proxy, TLS still
  terminates at `api.firecrawl.dev` (the rotator does CONNECT for HTTPS
  proxies); the injected `Authorization` header is end-to-end and not exposed
  to the outbound proxy hop.
- This is **egress only.** It does not change how firecrawl-mcp reaches the
  rotator (that stays direct on the docker network). It only changes how the
  rotator reaches Firecrawl.

### 3. Rotation decider

A response triggers rotation when **either**:

- HTTP status is `402`, `429`, `401`, or `403`; OR
- status is anything but the decoded body matches a denylist regex (default,
  case-insensitive): `insufficient credits?`, `rate limit`, `exceeded`,
  `payment required`, `unauthorized`, `forbidden`. This catches Firecrawl's
  `200 + success:false` and non-standard-status cases.

On trigger:

- Increment `currentIndex` (mod pool size).
- Bump a per-key stat counter for that error class (`402` / `429` / `auth` /
  `retry`).
- Log the rotation: timestamp, `from key<idx>` -> `to key<idx>`, reason
  (status + matched text, keys masked to last 4 chars).
- Retry the **original** request with the new key.

Non-rotating responses (success, 4xx other than the four, 5xx, network errors)
are returned to firecrawl-mcp unchanged. In particular, **5xx and connection
errors do NOT rotate** — they are not key problems, and rotating would burn
keys pointlessly.

### 4. Retry mechanism (the 2-pass cap)

- Maintain a per-request attempt counter.
- On each rotation trigger, advance the cursor and retry, until either a
  non-rotating response is obtained or the request has touched every key
  `MAX_PASSES` times. `MAX_PASSES` defaults to `2` and is overridable via the
  env var of the same name (set `1` for "each key once, then fail", higher for
  more resilience to transient 429s at the cost of latency).
- If `MAX_PASSES` is exhausted, return the **last upstream response verbatim**
  (status + body) to firecrawl-mcp and log `all keys exhausted` with the last
  error. The agent sees the real Firecrawl error and can surface it.
- Worst case latency is bounded: `MAX_PASSES * poolSize` upstream calls.

- Each attempt is a fresh round-trip with the current key (see Forwarder). The
  original request body is fully buffered at ingress so it can be replayed on
  each retry.

### 5. `next`-URL rewriter (Approach A)

- Applied to every JSON response body before returning it (success or final
  error).
- Walk the decoded JSON. For any field literally named `next` whose value is a
  string that parses as an absolute URL whose host equals the upstream host,
  rewrite it to `<proxy base>/<path>?<query>` — same path and query, host
  swapped to the proxy's own address.
- Relative `next` URLs, foreign hosts, non-URL strings, and missing/odd values
  are left untouched. A `next` that fails to parse is passed through unchanged
  with a warning log.
- **Scope is strictly the `next` field.** The upstream host string appearing
  *anywhere else* in the body (inside scraped page content, in a result `url`
  field, in an error message, in metadata) is **never** rewritten - those are
  real data, and rewriting them would corrupt user content. Only a field named
  `next` is a pagination cursor firecrawl-mcp will follow, so only `next` is
  safe to touch.
- **Pagination-signal guard:** fire a warning **only when a response indicates
  there is more data to fetch but carries no `next` key** - i.e. it looks
  paginated (has a crawl `status` that is *not* terminal - not
  `completed`/`failed`/`cancelled` - or `completed < total`) yet has no field
  named `next` at all. The warning text: `pagination response with more data but no 'next' key - rewrite skipped, pagination may bypass proxy`. A
  *terminal* page (e.g. `status:"completed"`, `completed == total`, no `next`)
  is the normal end of a crawl and stays silent - it legitimately has no
  `next`. This detects a future API rename of the pagination field without
  false-positiving on every finished crawl. Rotation is unaffected; only the
  warning fires.
- Re-serialize and fix up `Content-Length` on the rewritten body.

The proxy base is derived from the incoming `Host` header (or
`PROXY_BASE_URL` env if set, for cases where the in-container address differs
from what callers use).

### 6. Observability endpoints

- `GET /healthz` -> `200 {"ok":true}` iff pool non-empty; `503` otherwise.
  Suitable as a docker `HEALTHCHECK`.
- `GET /status` -> `{"poolSize":N,"currentIndex":I,"keys":[{"index":0,"last4":"abcd","stats":{"success":..,"402":..,"429":..,"auth":..,"retries":..}}]}`.
  Keys masked to last 4 chars. No secrets.
- Per-rotation structured logs to stdout (one line each): timestamp, from/to
  key index, reason, masked keys.

`/healthz` and `/status` are matched on the inbound path **before** forwarding;
they never reach the upstream.

## Data flow (single search request)

1. firecrawl-mcp `POST /v2/search` -> rotator.
2. Rotator picks `keys[0]`, forwards to Firecrawl.
3. Firecrawl returns `402` (key 0 credits gone).
4. Rotator: cursor -> `keys[1]`, logs `rotate key0->key1 reason=402`, retries
   `POST /v2/search` with key 1.
5. Firecrawl returns `200` with `next: "https://api.firecrawl.dev/v2/search/.../next"`.
6. Rotator rewrites `next` to `http://firecrawl-rotator:8788/v2/search/.../next`,
   returns `200` to firecrawl-mcp.
7. firecrawl-mcp follows `next` -> back to rotator -> repeats from step 2 with
   the current key.

## Error handling

- **All keys fail within `MAX_PASSES`:** return last upstream response
  verbatim. Log `all keys exhausted`.
- **Upstream 5xx / network error:** no rotation; return after one attempt.
  (Optional: one same-key retry on connection reset. Defaults to off.)
- **Empty key pool at startup:** log + `exit(1)`.
- **Invalid `UPSTREAM_PROXY` URL:** unparseable or unsupported scheme -> log + `exit(1)` at startup.
- **Malformed `next` URL:** pass through unchanged, log warning.
- **Non-JSON response body:** forward untouched; `next` rewriting skipped.
- **Response body exceeds `MAX_BODY_BYTES`:** forward to caller without
  inspection/rewriting (rotation and `next`-rewrite skipped for that response
  only), log warning. Bounds memory.

## Configuration (env vars)

| Var | Default | Purpose |
|-----|---------|---------|
| `FIRECRAWL_API_KEYS` | (required) | Comma-separated key pool. |
| `UPSTREAM` | `https://api.firecrawl.dev` | Upstream Firecrawl API base. |
| `UPSTREAM_PROXY` | (unset) | Explicit forward proxy for egress to `UPSTREAM` (`http://`, `https://`, `socks5://`). Wins over system vars if set. |
| `HTTPS_PROXY` / `HTTP_PROXY` / `NO_PROXY` | (unset) | System/curl-style proxy env, honored via Go stdlib when `UPSTREAM_PROXY` is unset. Lets you "use system curl proxy setting". |
| `PORT` | `8788` | Listen port. |
| `HOST` | `0.0.0.0` | Listen address. |
| `MAX_PASSES` | `2` | Full passes over the pool before giving up. Override per deployment. |
| `MAX_BODY_BYTES` | `16777216` (16 MiB) | Cap on a buffered response body. Above it, the response is forwarded without inspection/rewriting and a warning is logged. `0` = no cap (buffer unconditionally). |
| `PROXY_BASE_URL` | (from `Host` header) | Base used when rewriting `next` URLs. |
| `LOG_LEVEL` | `info` | `debug` adds per-request lines. |

## docker-compose wiring (sketch)

```yaml
firecrawl-rotator:
  build: ./firecrawl-rotator          # or image: ghcr.io/<you>/firecrawl-rotator:latest
  environment:
    FIRECRAWL_API_KEYS: "fc-key1,fc-key2,fc-key3"
    UPSTREAM: "https://api.firecrawl.dev"
    PORT: "8788"
    MAX_PASSES: "2"
  healthcheck:
    test: ["CMD", "wget", "-qO-", "http://localhost:8788/healthz"]
    interval: 30s

firecrawl:                           # existing mcpo service
  image: ghcr.io/open-webui/mcpo:latest
  entrypoint: ["uvx", "mcpo"]
  command: ["--host", "0.0.0.0", "--port", "8000", "--config", "/config/config.json"]
  # config.json's firecrawl env becomes:
  #   "FIRECRAWL_API_URL": "http://firecrawl-rotator:8788"
  #   (FIRECRAWL_API_KEY removed)
```

## Testing

- **Unit — decider:** triggers on each of 402/429/401/403; triggers on body
  regex match for each default pattern; does NOT trigger on 200, 404, 500, or
  unrelated body text.
- **Unit — cursor:** advances mod N; wraps at pool boundary.
- **Unit — outbound proxy:** `UPSTREAM_PROXY` set -> transport routes through it; unset with `HTTPS_PROXY` set -> stdlib honors it; `NO_PROXY` containing the upstream host -> bypasses even when a proxy is configured; unparseable `UPSTREAM_PROXY` -> startup error.
- **Unit — 2-pass cap:** with a pool of 3 and a backend that always 402s,
  exactly `MAX_PASSES * 3` attempts are made and the last 402 is returned.
- **Unit — rewriter:** rewrites an absolute upstream `next`; leaves relative,
  foreign-host, and non-URL values untouched; passes through unparseable
  values with a warning.
- **Unit — rewrite scope:** a scraped-page body containing the literal
  string `api.firecrawl.dev` in its content/`url`/metadata is returned
  **unchanged**; only a field named `next` holding an absolute upstream URL is
  rewritten.
- **Unit — pagination guard:** a non-terminal crawl page
  (`status:"scraping"`, `completed < total`, no `next`) logs the warning; a
  terminal page (`status:"completed"`, `completed == total`, no `next`) logs
  nothing.
- **Unit — MAX_BODY_BYTES:** a response just under the cap is inspected/rewritten
  normally; one over the cap is forwarded untouched with a warning and rotation
  is not attempted on it.
- **Unit — 5xx no-rotate:** a 502 upstream is returned after a single attempt,
  cursor unchanged.
- **Integration:** a fake Firecrawl backend (`httptest.Server`) that 402s for
  key 0 and 200s for key 1; assert the client gets 200 and `currentIndex`
  advanced. A second test where all keys 402 asserts the cap and verbatim
  passthrough of the last error.
- **docker-compose smoke:** mcpo + firecrawl-mcp + rotator + fake backend; a
  `firecrawl_search` round-trips successfully through rotation.

## Out of scope (YAGNI)

- No key generation or OAuth — keys supplied via env.
- No per-key cooldown/quota cache for rate-limited keys (2-pass handles the
  common case; add later if needed).
- No Prometheus/metrics export — `/status` JSON + logs suffice.
- No auth between mcpo and the rotator (same docker network; add a shared
  secret later if the port is exposed).
- No hot-reload of keys (env-var source chosen; re-compose to change).
- No persistence of stats across restarts.
