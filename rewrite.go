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
// has no "next" key - a sign the pagination field may have been renamed.
// Returns true (warn) only for a non-terminal crawl status with no "next".
// Terminal pages (completed/failed/cancelled), pages with a "next", and
// non-crawl payloads (no "status" field) return false.
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
		return false // terminal - no more data expected
	case "":
		return false // not a crawl-status payload
	}
	// non-terminal status (e.g. "scraping") with no "next" -> warn
	return true
}

// formatProxyBase ensures no trailing slash for URL composition.
func formatProxyBase(s string) string {
	return strings.TrimRight(s, "/")
}
