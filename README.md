# firecrawl-rotator

A small reverse proxy that sits between **firecrawl-mcp** and the Firecrawl API
(`api.firecrawl.dev`). It holds a pool of Firecrawl API keys, injects one per
request, and **picks the key with the most remaining credits**, rotating to the
next-richest when one drops to a low threshold. Transient errors (403, 5xx,
network) are retried with exponential backoff on the same key; genuine key
rejections (402/429/401) rotate. When every key is below the stop threshold the
proxy refuses requests (503) so the MCP client sees a clean failure instead of
burning dead keys.

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
| `CREDIT_RESET_DAY` | `1` | Fallback day-of-month (1-31, UTC) when a key's billing reset can't be auto-detected. See "Credit disabling" below. |
| `LOW_CREDIT_THRESHOLD` | `10` | Switch off a key (rotate to the next) when its predicted `remainingCredits` drops to/below this. |
| `STOP_CREDIT_THRESHOLD` | `2` | Stop accepting requests when every key is below this. Must be <= `LOW_CREDIT_THRESHOLD`. |
| `CREDIT_REFRESH_INTERVAL` | `300` | Seconds; minimum interval between credit refreshes of a low-balance key. See "Credit-aware selection". |
| `LOG_LEVEL` | `info` | `debug` adds per-request lines. |

## Endpoints

- `GET /healthz` -> `200 {"ok":true}` if at least one key is usable, else `503`. Docker healthcheck target.
- `GET /status` -> pool size, current index, per-key stats, disabled state, and `remainingCredits` (keys masked to last 4 chars; `-1` = unmeasured).

## Credit-aware key selection

The rotator picks the key with the **highest remaining credits** that is above
the stop threshold, so traffic concentrates on the healthiest account. It tracks
each key's `remainingCredits` from `GET /v2/team/credit-usage` (read-only, costs
no credits):

- **Startup**: fetches every key's real balance in the background (the server
  starts immediately; until this completes, unmeasured keys are assumed plentiful).
- **On success**: decrements the used key's predicted balance by the response's
  `creditsUsed` (or by 1 if that field is absent).
- **On rotation**: refreshes the keys we switched off and onto.
- **Low balance**: when a key's *predicted* balance drops below 100, it is
  refreshed at most once per `CREDIT_REFRESH_INTERVAL` (default 5 min) to correct
  estimation drift.
- **Daily**: every key is refreshed once per 24h as a catch-all.

When the current key's predicted balance hits `LOW_CREDIT_THRESHOLD` (default
10), it is rotated off (cooled down ~30s) and the next-richest key takes over.
When **every** key is below `STOP_CREDIT_THRESHOLD` (default 2), `/healthz`
returns `503` and new requests return
`503 {"success":false,"error":"all keys credit-exhausted until billing reset"}`.

## Rotation & retry behavior

Two kinds of failure are handled differently:

**Key-level rejection -> rotate to another key:** HTTP **402** (credits),
**429** (rate limit), **401** (bad key), and failure envelopes
(`{"success":false,...}`) whose text matches `insufficient credits`, `rate
limit`, `exceeded`, `payment required`, `unauthorized`, `forbidden`. The key is
cooled down ~30s (or disabled if credit-exhausted - see below) and the next key
is tried, up to `MAX_PASSES` full sweeps.

**Transient error -> backoff on the SAME key:** HTTP **403** (edge/WAF), **408**,
**5xx**, and network errors are retried on the same key with exponential backoff
`500ms -> 1s -> 2s -> 4s -> 8s` (5 attempts, ~15s total) before rotating. A 403
is usually a network/edge-layer issue, not a per-key problem, so it does NOT
disable or rotate immediately.

A **successful** response (`status < 400` with `success:true`, or no `success`
field) **never** rotates - even if the scraped *content* mentions "rate limit" or
"payment required". The denylist is checked against the Firecrawl failure
envelope only, not the response body as a whole.

- The `next` field's absolute upstream URL is rewritten to the proxy so crawl
  pagination stays under rotation. Other occurrences of the host in response
  bodies are **never** rewritten (they may be real scraped content).

## Credit disabling

A key that returns a **genuine credit-exhaustion** signal (HTTP 402, or a
`success:false` envelope mentioning `insufficient credits` / `payment required`
/ `exceeded`) is **disabled** and skipped on all subsequent requests until its
credits reset - it is not retried every pass (which would waste upstream calls
and risk account flags).

- The reset instant is read **per key** from that key's own
  `GET /v2/team/credit-usage` -> `billingPeriodEnd` (a read-only endpoint that
  costs no credits). This matters because each key belongs to a separate
  account and resets on **that account's billing anniversary**, which is often
  a different day per key - not a universal date.
- If the credit-usage call fails, the key is disabled until the next
  occurrence of `CREDIT_RESET_DAY` (UTC) as a fallback.
- **429 (rate limit)** and **401 (auth)** rotate but do NOT disable -
  they are transient or account-global, and disabling on them would take a
  good key offline. **403** is retried with backoff, never disabled.
- A background loop re-enables each key at its own reset instant; restarting
  the container also clears all disables.

## Develop

```bash
go test ./...
go build -o rotator .
FIRECRAWL_API_KEYS=fc-x ./rotator
```

See `docs/superpowers/specs/2026-07-09-firecrawl-token-rotation-design.md` for
the full design.
