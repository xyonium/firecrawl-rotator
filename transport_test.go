package main

import (
	"net/http"
	"net/url"
	"testing"
)

func TestBuildTransport_UpstreamProxy(t *testing.T) {
	cfg := Config{UpstreamProxy: "http://proxy.corp:3128"}
	tr, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The Proxy func should route a request to the configured proxy.
	u, _ := url.Parse("https://api.firecrawl.dev/v2/search")
	proxyURL, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy() error: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "proxy.corp:3128" {
		t.Fatalf("proxyURL = %v, want proxy.corp:3128", proxyURL)
	}
}

func TestBuildTransport_Socks5hAccepted(t *testing.T) {
	// curl/SearXNG-style socks5h:// is normalized to socks5:// (the Go stdlib
	// resolves DNS via the SOCKS5 server for either form) and routed.
	cfg := Config{UpstreamProxy: "socks5h://192.168.254.253:1091"}
	tr, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, _ := url.Parse("https://api.firecrawl.dev/v2/search")
	proxyURL, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy() error: %v", err)
	}
	if proxyURL == nil || proxyURL.Scheme != "socks5" || proxyURL.Host != "192.168.254.253:1091" {
		t.Fatalf("proxyURL = %v, want socks5://192.168.254.253:1091", proxyURL)
	}
}

func TestBuildTransport_SystemEnvFallback(t *testing.T) {
	// When UPSTREAM_PROXY is empty, buildTransport wires the stdlib
	// http.ProxyFromEnvironment (curl-style HTTPS_PROXY/HTTP_PROXY/NO_PROXY).
	// We assert only that a Proxy func is set - we do NOT assert what it
	// resolves to, because ProxyFromEnvironment caches its config in a
	// process-wide sync.Once, making the resolved value depend on env state
	// at first call (not testable via t.Setenv). The stdlib's own tests
	// cover curl-exact NO_PROXY matching.
	cfg := Config{UpstreamProxy: ""}
	tr, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tr.Proxy == nil {
		t.Fatal("expected tr.Proxy to be set (ProxyFromEnvironment), got nil")
	}
}
