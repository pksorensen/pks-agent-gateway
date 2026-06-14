// pks-agent-gateway — streaming reverse proxy to the Anthropic API with optional
// OIDC auth, project-scoped OTEL ingestion, and a simple REST management API.
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"time"
)

func main() {
	port := env("PORT", "8080")
	upstream := env("UPSTREAM", "https://api.anthropic.com")
	dataDir := env("USER_DATA_DIR", "./data")
	owner := env("GATEWAY_OWNER", "default")
	gatewayToken := os.Getenv("GATEWAY_TOKEN")
	oidcIssuer := os.Getenv("OIDC_ISSUER")

	store := NewStore(dataDir, owner)

	var auth *OIDCMiddleware
	if oidcIssuer != "" {
		var err error
		auth, err = NewOIDCMiddleware(context.Background(), oidcIssuer)
		if err != nil {
			log.Fatalf("OIDC init: %v", err)
		}
	} else {
		auth = DevModeMiddleware()
		log.Println("OIDC_ISSUER not set — auth disabled (dev mode)")
	}

	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintln(w, `{"status":"ok"}`)
	})

	// OTEL ingestion — no auth required.
	otel := newOtelHandler(store)
	mux.Handle("POST /otel/v1/logs", otel)
	mux.Handle("POST /otel/v1/traces", otel)
	mux.Handle("POST /otel/v1/metrics", otel)

	// Management API — auth required.
	api := newAPIHandler(store)
	mux.Handle("GET /api/projects", auth.Require()(api))
	mux.Handle("POST /api/projects", auth.Require(RoleGatewayAdmin)(api))
	mux.Handle("GET /api/projects/{id}", auth.Require()(api))
	mux.Handle("GET /api/projects/{id}/stats", auth.Require()(api))

	// Proxy catch-all — forwards everything else to the upstream Anthropic API.
	upstreamURL, err := url.Parse(upstream)
	if err != nil {
		log.Fatalf("invalid UPSTREAM %q: %v", upstream, err)
	}
	mux.Handle("/", newProxy(upstreamURL, gatewayToken))

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// Long timeouts: Claude streaming responses can run for minutes.
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      0, // unlimited — streaming
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("pks-agent-gateway listening on :%s → %s", port, upstream)
	log.Fatal(srv.ListenAndServe())
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
