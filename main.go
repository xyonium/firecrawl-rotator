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
	tr, err := buildTransport(cfg)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Transport: tr,
		Timeout:   30 * time.Second,
	}
	log := newLogger(cfg.LogLevel)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(pool))
	mux.HandleFunc("/status", statusHandler(pool))
	// Everything else goes to the rotator.
	mux.Handle("/", newRotator(cfg, pool, client, log))

	// Background loop: re-enable keys whose per-key billing reset has passed.
	// Each key stores its own reset instant (queried from its account's
	// billing period), so accounts on different anniversaries come back online
	// independently. Restarting the container also clears all disables.
	go resetLoop(pool, log)

	log.info("firecrawl-rotator starting",
		"keys", len(cfg.APIKeys), "upstream", cfg.Upstream, "maxPasses", cfg.MaxPasses,
		"creditResetDay", cfg.CreditResetDay)

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
