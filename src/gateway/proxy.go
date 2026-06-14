package main

import (
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
)

// newProxy returns an http.Handler that reverse-proxies all requests to upstream.
// If gatewayToken is non-empty it injects X-Gateway-Token on outbound requests.
// FlushInterval = -1 is required for SSE streaming — do not remove.
func newProxy(upstream *url.URL, gatewayToken string) http.Handler {
	proxy := httputil.NewSingleHostReverseProxy(upstream)

	// Stream every byte the moment it arrives — critical for SSE token streaming.
	proxy.FlushInterval = -1

	// Rewrite Host so upstream sees its own hostname for TLS SNI + routing.
	// The caller's x-api-key / anthropic-version / authorization pass through
	// unchanged.
	defaultDirector := proxy.Director
	proxy.Director = func(r *http.Request) {
		defaultDirector(r)
		r.Host = upstream.Host
		r.Header.Set("X-Forwarded-Host", r.Header.Get("Host"))
		if gatewayToken != "" {
			r.Header.Set("X-Gateway-Token", gatewayToken)
		}
	}

	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("upstream error: %s %s -> %v", r.Method, r.URL.Path, err)
		http.Error(w, "gateway: upstream request failed: "+err.Error(), http.StatusBadGateway)
	}

	return proxy
}
