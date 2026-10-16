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
	if len(os.Args) > 1 && os.Args[1] == "identity" {
		os.Exit(runIdentityCmd(os.Args[2:], os.Stdout, os.Stderr))
	}
	port := env("PORT", "8080")
	upstream := env("UPSTREAM", "https://api.anthropic.com")
	dataDir := env("USER_DATA_DIR", "./data")
	owner := env("GATEWAY_OWNER", "default")
	gatewayToken := os.Getenv("GATEWAY_TOKEN")
	oidcIssuer := os.Getenv("OIDC_ISSUER")
	flags, err := runtimeFlagsFromEnv(os.Getenv)
	if err != nil {
		log.Fatalf("gateway config: %v", err)
	}
	simEnabled := flags.sim
	speech := speechConfigFromEnv()
	speech.Enabled = flags.realtimeSpeech
	log.Println(flags.summary())

	store := NewStore(dataDir, owner)
	sim := newSimulator(store, simEnabled)
	if simEnabled {
		log.Println("SIMULATOR ENABLED — sim-* API keys are served locally, never proxied upstream")
	}

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
	mux.HandleFunc("GET /v1/capabilities", newSpeechCapabilitiesHandler(speech))
	mux.HandleFunc("/v1/speech/realtime", newSpeechRealtimeHandler(speech))

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

	// Test-bench management API — registered unconditionally (it answers 403
	// when the sim is disabled) so a typo'd testbench path can never fall
	// through to the proxy catch-all carrying a Bearer token.
	mux.Handle("/api/testbench/", auth.Require(RoleGatewayAdmin)(newTestbenchHandler(store, sim)))

	// Subscription lane (ADR 0001): /apis/, /mcp/, /api/v1/. Registered as
	// subtrees so nothing under them can reach the passthrough catch-all.
	lcfg, err := laneConfigFromEnv(dataDir)
	if err != nil {
		log.Fatalf("gateway config: %v", err)
	}
	if _, err := mountSubscriptionLane(mux, auth, lcfg); err != nil {
		log.Fatalf("subscription lane: %v", err)
	}

	// Proxy catch-all — forwards everything else to the upstream Anthropic
	// API. The sim gate intercepts sim-keyed requests (always — even when
	// disabled they get a local 403, never the proxy) and handles cassette
	// recording for passthrough traffic.
	upstreamURL, err := url.Parse(upstream)
	if err != nil {
		log.Fatalf("invalid UPSTREAM %q: %v", upstream, err)
	}
	// gatewayKeyGuard: a gwk_ key never rides the passthrough lane. With
	// GATEWAY_PASSTHROUGH=off the catch-all answers locally; the sim gate
	// stays in front so sim keys keep their local answer either way.
	var catchAll http.Handler = newProxy(upstreamURL, gatewayToken)
	if !flags.passthrough {
		catchAll = passthroughDisabled()
	}
	mux.Handle("/", gatewayKeyGuard(sim.Gate(catchAll)))

	srv := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
		// Long timeouts: Claude streaming responses can run for minutes.
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      0, // unlimited — streaming
		IdleTimeout:       120 * time.Second,
	}

	if flags.passthrough {
		log.Printf("pks-agent-gateway listening on :%s → %s", port, upstream)
	} else {
		log.Printf("pks-agent-gateway listening on :%s (passthrough off)", port)
	}
	log.Fatal(srv.ListenAndServe())
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// runtimeFlags are the deployment kill switches (ADR 0006). Everything that
// can reach a third party or open a socket is either explicit or off.
type runtimeFlags struct {
	prod           bool // GATEWAY_MODE=prod
	passthrough    bool // GATEWAY_PASSTHROUGH (default on; "off" disables)
	sim            bool // GATEWAY_SIM_ENABLED=1
	realtimeSpeech bool // GATEWAY_REALTIME_SPEECH_ENABLED=1
	oidc           bool
}

func runtimeFlagsFromEnv(getenv func(string) string) (runtimeFlags, error) {
	f := runtimeFlags{
		sim:            getenv("GATEWAY_SIM_ENABLED") == "1",
		realtimeSpeech: getenv("GATEWAY_REALTIME_SPEECH_ENABLED") == "1",
		oidc:           getenv("OIDC_ISSUER") != "",
	}
	switch getenv("GATEWAY_MODE") {
	case "", "dev":
	case "prod":
		f.prod = true
	default:
		return f, fmt.Errorf("GATEWAY_MODE must be dev or prod")
	}
	switch getenv("GATEWAY_PASSTHROUGH") {
	case "", "on":
		f.passthrough = true
	case "off":
	default:
		return f, fmt.Errorf("GATEWAY_PASSTHROUGH must be on or off")
	}
	// Without OIDC the management plane admits everyone as an admin.
	if f.prod && !f.oidc {
		return f, fmt.Errorf("GATEWAY_MODE=prod requires OIDC_ISSUER (the management API would be open)")
	}
	return f, nil
}

// summary is one startup line saying which lanes are live — the cheapest
// check that a deployment exposes only what it should.
func (f runtimeFlags) summary() string {
	onoff := func(b bool) string {
		if b {
			return "on"
		}
		return "off"
	}
	mode := "dev"
	if f.prod {
		mode = "prod"
	}
	auth := "OIDC"
	if !f.oidc {
		auth = "OPEN (dev)"
	}
	return fmt.Sprintf("gateway mode=%s management-auth=%s passthrough=%s sim=%s realtime-speech=%s",
		mode, auth, onoff(f.passthrough), onoff(f.sim), onoff(f.realtimeSpeech))
}

// passthroughDisabled replaces the Anthropic passthrough when it is off.
func passthroughDisabled() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, "not found", http.StatusNotFound)
	})
}
