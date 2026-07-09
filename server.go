package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"time"
)

type logger struct {
	level string
}

func newLogger(level string) *logger {
	return &logger{level: level}
}

func (l *logger) info(msg string, kv ...any)  { l.log("info", msg, kv...) }
func (l *logger) warn(msg string, kv ...any)  { l.log("warn", msg, kv...) }
func (l *logger) error(msg string, kv ...any) { l.log("error", msg, kv...) }

func (l *logger) debug(msg string, kv ...any) {
	if l.level != "debug" {
		return
	}
	l.log("debug", msg, kv...)
}

func (l *logger) log(level, msg string, kv ...any) {
	ts := time.Now().UTC().Format(time.RFC3339)
	line := fmt.Sprintf("[%s] %s %s", level, ts, msg)
	for i := 0; i+1 < len(kv); i += 2 {
		line += fmt.Sprintf(" %v=%v", kv[i], kv[1+i])
	}
	fmt.Fprintln(os.Stderr, line)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func healthzHandler(pool *KeyPool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if pool == nil || len(pool.keys) == 0 {
			writeJSON(w, 503, map[string]any{"ok": false})
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
	}
}

func statusHandler(pool *KeyPool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		writeJSON(w, 200, pool.Snapshot())
	}
}
