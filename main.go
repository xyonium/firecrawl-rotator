package main

import (
	"flag"
	"net/http"
	"os"
	"time"
)

func buildServer() (*http.Server, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return nil, err
	}

	pool := NewKeyPool(cfg.APIKeys)
	pool.SetThresholds(cfg.LowCreditThreshold, cfg.StopCreditThreshold)
	tr, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
	}
	log := newLogger(cfg.LogLevel)
	refresh := NewRefresher(pool, client, cfg, log)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(pool))
	mux.HandleFunc("/status", statusHandler(pool))
	// Everything else goes to the rotator.
	fcProfile := &Profile{
		Name:           "firecrawl",
		Upstream:       cfg.Upstream,
		UpstreamHost:   cfg.UpstreamHost,
		CreditResetDay: cfg.CreditResetDay,
		RewriteNext:    true,
		pool:           pool,
		refresh:        refresh,
	}
	mux.Handle("/", newRotator(cfg, []*Profile{fcProfile}, client, log))

	// Warm up: fetch each key's real remainingCredits so selection starts
	// accurate. Runs in the background so the server starts immediately; until
	// it completes, unmeasured keys are treated as "plenty" (math.MaxInt64).
	// If the warm-up leaves any key unmeasured (transient network blip at
	// startup), retry every minute until it succeeds - don't strand keys at
	// "unmeasured" waiting up to 24h for the daily refresh.
	go warmupLoop(refresh, log)

	// Background loop: re-enable keys whose per-key billing reset has passed.
	go resetLoop(pool, log)
	// Background loop: daily catch-all refresh of every key's credits.
	go dailyRefreshLoop(refresh, log)

	log.info("firecrawl-rotator starting",
		"keys", len(cfg.APIKeys), "upstream", cfg.Upstream, "maxPasses", cfg.MaxPasses,
		"creditResetDay", cfg.CreditResetDay,
		"lowCreditThreshold", cfg.LowCreditThreshold,
		"stopCreditThreshold", cfg.StopCreditThreshold,
		"creditRefreshSec", cfg.CreditRefreshSec)

	return &http.Server{
		Addr:    cfg.Host + ":" + cfg.Port,
		Handler: mux,
	}, nil
}

// resetLoop wakes every minute and re-enables any key whose disabledUntil has
// passed. Cheap and bounded; the heavy work (the /v2/team/credit-usage query)
// happens once, at disable time.
func resetLoop(pool *KeyPool, log *logger) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		if n := pool.ReenableDue(time.Now().UTC()); n > 0 {
			log.info("re-enabled credit-disabled keys", "count", n)
		}
	}
}

// dailyRefreshLoop wakes hourly and asks the refresher to refresh any key whose
// last daily refresh is older than 24h. (The refresher itself enforces the
// 24h-per-key cadence; this ticker just pokes it.)
func dailyRefreshLoop(refresh *Refresher, log *logger) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		refresh.DailyRefresh()
	}
}

// warmupLoop runs the initial warm-up. If any keys remain unmeasured after a
// pass (their credit-usage fetch failed), it retries every minute until all are
// measured - so a transient startup network failure self-heals quickly instead
// of leaving keys at "unmeasured" (-1) until the next daily refresh.
func warmupLoop(refresh *Refresher, log *logger) {
	if unmeasured := refresh.RefreshAll(); unmeasured > 0 {
		log.warn("warm-up left keys unmeasured, will retry", "unmeasured", unmeasured)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for range ticker.C {
			if unmeasured := refresh.RefreshAll(); unmeasured == 0 {
				log.info("warm-up complete, all keys measured")
				return
			}
		}
	}
}

func main() {
	healthcheck := flag.Bool("healthcheck", false, "GET /healthz and exit 0/1")
	flag.Parse()

	if *healthcheck {
		cfg, err := LoadConfig()
		if err != nil {
			os.Exit(1)
		}
		resp, err := http.Get("http://127.0.0.1:" + cfg.Port + "/healthz")
		if err != nil || resp.StatusCode != 200 {
			os.Exit(1)
		}
		_ = resp.Body.Close()
		os.Exit(0)
	}

	srv, err := buildServer()
	if err != nil {
		os.Stderr.WriteString("config error: " + err.Error() + "\n")
		os.Exit(1)
	}
	if err := srv.ListenAndServe(); err != nil {
		os.Stderr.WriteString("server error: " + err.Error() + "\n")
		os.Exit(1)
	}
}
