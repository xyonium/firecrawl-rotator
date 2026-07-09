package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfig_defaults(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a, fc-b ,, fc-c")
	for _, k := range []string{"UPSTREAM", "PORT", "HOST", "MAX_PASSES", "MAX_BODY_BYTES", "PROXY_BASE_URL", "UPSTREAM_PROXY", "LOG_LEVEL"} {
		t.Setenv(k, "")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		APIKeys:      []string{"fc-a", "fc-b", "fc-c"}, // trimmed, empties dropped
		Upstream:     "https://api.firecrawl.dev",
		UpstreamHost: "api.firecrawl.dev",
		Port:         "8788",
		Host:         "0.0.0.0",
		MaxPasses:    2,
		MaxBodyBytes: 16 * 1024 * 1024,
		LogLevel:     "info",
	}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadConfig_emptyKeysErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", " , , ")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for empty key pool, got nil")
	}
}

func TestLoadConfig_badUpstreamErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM", "://no-scheme")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for unparseable upstream, got nil")
	}
}

func TestLoadConfig_badUpstreamProxyErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM_PROXY", "://bad")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for unparseable UPSTREAM_PROXY, got nil")
	}
}

func TestLoadConfig_maxBodyBytesZero(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("MAX_BODY_BYTES", "0")
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MaxBodyBytes != 0 {
		t.Fatalf("MaxBodyBytes = %d, want 0", cfg.MaxBodyBytes)
	}
}

func TestLoadConfig_negativeMaxPassesErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("MAX_PASSES", "0")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for MAX_PASSES=0, got nil")
	}
}

func TestLoadConfig_nonIntegerMaxPassesErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("MAX_PASSES", "abc")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for MAX_PASSES=abc, got nil")
	}
}

func TestLoadConfig_upstreamEmptyHostErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM", "http://")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for UPSTREAM=http://, got nil")
	}
	if !strings.Contains(err.Error(), "not a valid http(s) URL") {
		t.Fatalf("error %q does not contain %q", err.Error(), "not a valid http(s) URL")
	}
}

func TestLoadConfig_upstreamProxyBadSchemeErrors(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
	t.Setenv("UPSTREAM_PROXY", "ftp://proxy:21")
	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected error for UPSTREAM_PROXY=ftp://proxy:21, got nil")
	}
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("error %q does not contain %q", err.Error(), "not supported")
	}
}
