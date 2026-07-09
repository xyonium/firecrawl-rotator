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

	// The pool cursor is shared across concurrent requests: Current() and
	// Advance() lock independently, so under load two requests may read the
	// same key and both advance. This is intended — rotation is approximate
	// round-robin, and a per-request lock would serialize all upstream calls.
	// A good key is still found within MaxPasses full sweeps of the pool.
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
		// Strip hop-by-hop headers from the forwarded request (Connection,
		// Transfer-Encoding, etc.) the same way we do for responses.
		for k := range upReq.Header {
			if isHopByHop(k) {
				upReq.Header.Del(k)
			}
		}

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
