// pks-agent-gateway — a transparent streaming reverse proxy to the Anthropic API.
//
// Purpose (v1): give Claude Code a base URL that is reachable from networks
// where api.anthropic.com is blocked. Point ANTHROPIC_BASE_URL at this gateway
// and every request (including streaming SSE) is forwarded upstream unchanged,
// carrying the caller's own x-api-key / authorization headers.
//
// It is deliberately dumb: no auth plane of its own, no request rewriting beyond
// the Host header. The only optional gate is a shared GATEWAY_TOKEN to stop the
// public endpoint from being used as an open proxy.
package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"time"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	port := env("PORT", "8080")
	upstreamRaw := env("UPSTREAM", "https://api.anthropic.com")
	gatewayToken := os.Getenv("GATEWAY_TOKEN") // optional shared secret

	upstream, err := url.Parse(upstreamRaw)
	if err != nil {
		log.Fatalf("invalid UPSTREAM %q: %v", upstreamRaw, err)
	}

	proxy := httputil.NewSingleHostReverseProxy(upstream)

	// Stream every byte the moment it arrives — critical for SSE token streaming.
	proxy.FlushInterval = -1

	// Rewrite the request so the upstream sees its own host (TLS SNI + Host
	// header), but otherwise leave the path, query, and headers untouched so
	// the caller's x-api-key / anthropic-version / authorization pass straight
	// through.
	defaultDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		defaultDirector(r)
		r.Host = upstream.Host
		r.Header.Set("X-Forwarded-Host", r.Header.Get("Host"))
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream error: %s %s -> %v", r.Method, r.URL.Path, err)
		http.Error(w, "gateway: upstream request failed: "+err.Error(), http.StatusBadGateway)
	}

	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if gatewayToken != "" && r.Header.Get("X-Gateway-Token") != gatewayToken {
			http.Error(w, "gateway: forbidden", http.StatusForbidden)
			return
		}
		// Log enough to see traffic, never the credentials.
		log.Printf("%s %s", r.Method, r.URL.Path)
		proxy.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// Long timeouts: Claude streaming responses can run for minutes.
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      0, // unlimited — streaming
		IdleTimeout:       120 * time.Second,
	}

	gate := "off"
	if gatewayToken != "" {
		gate = "on (X-Gateway-Token required)"
	}
	log.Printf("pks-agent-gateway listening on :%s -> %s  (token gate: %s)", port, strings.TrimRight(upstreamRaw, "/"), gate)
	log.Fatal(srv.ListenAndServe())
}
