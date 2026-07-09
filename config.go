package main

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	APIKeys       []string
	Upstream      string
	UpstreamHost  string
	Port          string
	Host          string
	MaxPasses     int
	MaxBodyBytes  int64
	ProxyBaseURL  string
	UpstreamProxy string
	LogLevel      string
}

func envStr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %w", key, err)
	}
	return n, nil
}

func envInt64(key string, def int64) (int64, error) {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: not an integer: %w", key, err)
	}
	return n, nil
}

func parseKeys(raw string) []string {
	var out []string
	for _, k := range strings.Split(raw, ",") {
		k = strings.TrimSpace(k)
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}

func LoadConfig() (Config, error) {
	keys := parseKeys(os.Getenv("FIRECRAWL_API_KEYS"))
	if len(keys) == 0 {
		return Config{}, fmt.Errorf("FIRECRAWL_API_KEYS is required and must contain at least one non-empty key")
	}

	upstream := envStr("UPSTREAM", "https://api.firecrawl.dev")
	u, err := url.Parse(upstream)
	if err != nil {
		return Config{}, fmt.Errorf("UPSTREAM %q is not a valid http(s) URL: %w", upstream, err)
	}
	if u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return Config{}, fmt.Errorf("UPSTREAM %q is not a valid http(s) URL (scheme must be http/https, host required)", upstream)
	}

	maxPasses, err := envInt("MAX_PASSES", 2)
	if err != nil {
		return Config{}, err
	}
	if maxPasses < 1 {
		return Config{}, fmt.Errorf("MAX_PASSES must be >= 1, got %d", maxPasses)
	}

	maxBody, err := envInt64("MAX_BODY_BYTES", 16*1024*1024)
	if err != nil {
		return Config{}, err
	}
	if maxBody < 0 {
		return Config{}, fmt.Errorf("MAX_BODY_BYTES must be >= 0, got %d", maxBody)
	}

	proxyStr := strings.TrimSpace(os.Getenv("UPSTREAM_PROXY"))
	if proxyStr != "" {
		pu, err := url.Parse(proxyStr)
		if err != nil {
			return Config{}, fmt.Errorf("UPSTREAM_PROXY %q is not a valid proxy URL: %w", proxyStr, err)
		}
		if pu.Host == "" {
			return Config{}, fmt.Errorf("UPSTREAM_PROXY %q is not a valid proxy URL (host required)", proxyStr)
		}
		switch pu.Scheme {
		case "http", "https", "socks5":
		default:
			return Config{}, fmt.Errorf("UPSTREAM_PROXY scheme %q not supported (use http/https/socks5)", pu.Scheme)
		}
	}

	return Config{
		APIKeys:       keys,
		Upstream:      strings.TrimRight(upstream, "/"),
		UpstreamHost:  u.Host,
		Port:          envStr("PORT", "8788"),
		Host:          envStr("HOST", "0.0.0.0"),
		MaxPasses:     maxPasses,
		MaxBodyBytes:  maxBody,
		ProxyBaseURL:  strings.TrimSpace(os.Getenv("PROXY_BASE_URL")),
		UpstreamProxy: proxyStr,
		LogLevel:      envStr("LOG_LEVEL", "info"),
	}, nil
}
