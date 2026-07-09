package main

import (
	"net/http"
	"net/url"
	"os"
	"strings"
)

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
		// Honor system HTTPS_PROXY/HTTP_PROXY/NO_PROXY, curl-style.
		// Uses a custom function (not http.ProxyFromEnvironment) so that
		// env vars are read at call time, avoiding the sync.Once caching
		// in the stdlib that makes tests brittle.
		tr.Proxy = proxyFromEnvironment
	}

	return tr, nil
}

// proxyFromEnvironment reads HTTPS_PROXY/HTTP_PROXY/NO_PROXY at call time,
// avoiding the sync.Once caching in http.ProxyFromEnvironment.
func proxyFromEnvironment(req *http.Request) (*url.URL, error) {
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		proxy = os.Getenv("https_proxy")
	}
	if proxy == "" {
		proxy = os.Getenv("HTTP_PROXY")
	}
	if proxy == "" {
		proxy = os.Getenv("http_proxy")
	}
	if proxy == "" {
		return nil, nil
	}

	proxyURL, err := url.Parse(proxy)
	if err != nil {
		return nil, err
	}

	noProxy := os.Getenv("NO_PROXY")
	if noProxy == "" {
		noProxy = os.Getenv("no_proxy")
	}
	if noProxy != "" && matchNoProxy(req.URL.Hostname(), noProxy) {
		return nil, nil
	}

	return proxyURL, nil
}

// matchNoProxy reports whether host should bypass the proxy
// according to the NO_PROXY pattern (comma-separated, leading dot = suffix match).
func matchNoProxy(host, noProxy string) bool {
	for _, pattern := range strings.Split(noProxy, ",") {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		if pattern == "*" {
			return true
		}
		if strings.HasPrefix(pattern, ".") {
			// Suffix match: .example.com matches any.example.com
			if strings.HasSuffix(host, pattern) {
				return true
			}
		} else if host == pattern {
			return true
		}
	}
	return false
}
