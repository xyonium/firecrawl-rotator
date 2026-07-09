package main

import (
	"net/http"
	"net/url"
)

// buildTransport constructs the *http.Transport used for upstream Firecrawl
// calls. If UPSTREAM_PROXY is set, all egress routes through it. Otherwise the
// system proxy env vars (HTTPS_PROXY/HTTP_PROXY/NO_PROXY) are honored
// curl-style via the stdlib - read at request time, not cached at startup.
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
		// Honor system HTTPS_PROXY/HTTP_PROXY/NO_PROXY, curl-style. The
		// stdlib reads these at request time and matches NO_PROXY with the
		// same rules as curl/wget.
		tr.Proxy = http.ProxyFromEnvironment
	}

	return tr, nil
}
