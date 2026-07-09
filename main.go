package main

import (
	"net/http"
	"os"
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
	client := &http.Client{Transport: tr}
	log := newLogger(cfg.LogLevel)

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", healthzHandler(pool))
	mux.HandleFunc("/status", statusHandler(pool))
	// Everything else goes to the rotator.
	mux.Handle("/", newRotator(cfg, pool, client, log))

	log.info("firecrawl-rotator starting",
		"keys", len(cfg.APIKeys), "upstream", cfg.Upstream, "maxPasses", cfg.MaxPasses)

	return &http.Server{
		Addr:    cfg.Host + ":" + cfg.Port,
		Handler: mux,
	}, nil
}

func main() {
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
