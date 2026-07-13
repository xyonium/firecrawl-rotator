package main

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLoadConfig_defaults(t *testing.T) {
	t.Setenv("FIRECRAWL_API_KEYS", "fc-a, fc-b ,, fc-c")
	for _, k := range []string{"UPSTREAM", "PORT", "HOST", "MAX_PASSES", "MAX_BODY_BYTES", "PROXY_BASE_URL", "UPSTREAM_PROXY", "LOG_LEVEL", "CREDIT_RESET_DAY"} {
		t.Setenv(k, "")
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := Config{
		APIKeys:       []string{"fc-a", "fc-b", "fc-c"}, // trimmed, empties dropped
		Upstream:      "https://api.firecrawl.dev",
		UpstreamHost:  "api.firecrawl.dev",
		Port:          "8788",
		Host:          "0.0.0.0",
		MaxPasses:     2,
		MaxBodyBytes:  16 * 1024 * 1024,
		LogLevel:      "info",
		CreditResetDay: 1,
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

func TestLoadConfig_creditResetDayValidation(t *testing.T) {
	for _, bad := range []string{"0", "32", "-1"} {
		t.Setenv("FIRECRAWL_API_KEYS", "fc-a")
		t.Setenv("CREDIT_RESET_DAY", bad)
		if _, err := LoadConfig(); err == nil {
			t.Fatalf("CREDIT_RESET_DAY=%s: expected error, got nil", bad)
		}
	}
}

func TestFallbackReset(t *testing.T) {
	// resetDay 15, on July 13 2026 -> next July 15 2026 00:00 UTC
	now := time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC)
	got := fallbackReset(now, 15)
	want := time.Date(2026, 7, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("fallbackReset = %v, want %v", got, want)
	}
	// on July 15 after midnight -> rolls to next month (Aug 15)
	now = time.Date(2026, 7, 15, 1, 0, 0, 0, time.UTC)
	got = fallbackReset(now, 15)
	want = time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("fallbackReset roll = %v, want %v", got, want)
	}
}
