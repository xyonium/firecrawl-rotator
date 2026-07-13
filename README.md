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
| `CREDIT_RESET_DAY` | `1` | Fallback day-of-month (1-31, UTC) when a key's billing reset can't be auto-detected. See "Credit disabling" below. |
| `LOG_LEVEL` | `info` | `debug` adds per-request lines. |

## Endpoints

- `GET /healthz` -> `200 {"ok":true}` if at least one key is usable, else `503`. Docker healthcheck target.
- `GET /status` -> pool size, current index, per-key stats + disabled state (keys masked to last 4 chars).

## Rotation behavior

Rotates on HTTP **402** (credits), **429** (rate limit), **401/403** (bad key),
and on **failure envelopes** (`{"success":false,...}`) whose `error`/`message`
text matches `insufficient credits`, `rate limit`, `exceeded`,
`payment required`, `unauthorized`, `forbidden`.

A **successful** response (`status < 400` with `success:true`, or no `success`
field) **never** rotates - even if the scraped *content* happens to contain
words like "rate limit" or "payment required". The denylist is checked against
the Firecrawl failure envelope only, not the response body as a whole.

- Tries each key up to `MAX_PASSES` times, then returns the last error verbatim.
- **5xx and network errors do NOT rotate** (not a key problem).
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
- **429 (rate limit)** and **401/403 (auth)** rotate but do NOT disable -
  they are transient or account-global, and disabling on them would take a
  good key offline.
- A background loop re-enables each key at its own reset instant; restarting
  the container also clears all disables.
- When every key is disabled, `/healthz` returns `503` and proxied requests
  return `503 {"success":false,"error":"all keys credit-exhausted..."}`.

## Develop

```bash
go test ./...
go build -o rotator .
FIRECRAWL_API_KEYS=fc-x ./rotator
```

See `docs/superpowers/specs/2026-07-09-firecrawl-token-rotation-design.md` for
the full design.
