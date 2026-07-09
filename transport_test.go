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
	u, _ := url.Parse("https://api.firecrawl.dev/v2/search")
	proxyURL, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy() error: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "proxy.corp:3128" {
		t.Fatalf("proxyURL = %v, want proxy.corp:3128", proxyURL)
	}
}

func TestBuildTransport_NoProxyDirect(t *testing.T) {
	cfg := Config{UpstreamProxy: ""}
	tr, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, _ := url.Parse("https://api.firecrawl.dev/v2/search")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("NO_PROXY", "")
	proxyURL, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy() error: %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("proxyURL = %v, want nil (direct)", proxyURL)
	}
}

func TestBuildTransport_SystemEnv(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://env-proxy:8080")
	t.Setenv("NO_PROXY", "")
	cfg := Config{UpstreamProxy: ""}
	tr, err := buildTransport(cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, _ := url.Parse("https://api.firecrawl.dev/v2/search")
	proxyURL, err := tr.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy() error: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "env-proxy:8080" {
		t.Fatalf("proxyURL = %v, want env-proxy:8080", proxyURL)
	}
}
