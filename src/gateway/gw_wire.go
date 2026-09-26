package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Subscription-lane wiring (ADR 0001). Kept out of main() so tests mount the
// exact production routes.

type laneConfig struct {
	dataDir string
	sealer  *Sealer
	// MCP OAuth (FR-6a): the gateway is a protected resource only.
	oauthIssuer   string
	oauthAudience string
	publicBaseURL string
	verify        TokenVerifier // nil = derive from oauthIssuer via discovery
}

func laneConfigFromEnv(dataDir string) (laneConfig, error) {
	sealer, err := NewSealerFromEnv()
	if err != nil {
		return laneConfig{}, err
	}
	return laneConfig{
		dataDir:       dataDir,
		sealer:        sealer,
		oauthIssuer:   env("MCP_OAUTH_ISSUER", os.Getenv("OIDC_ISSUER")),
		oauthAudience: os.Getenv("MCP_OAUTH_AUDIENCE"),
		publicBaseURL: strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"),
	}, nil
}

// oidcTokenVerifier verifies access-token JWTs against issuer. The audience
// check is done by the lane (resource URL or MCP_OAUTH_AUDIENCE).
func oidcTokenVerifier(ctx context.Context, issuer string) (TokenVerifier, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("MCP OAuth discovery for %s: %w", issuer, err)
	}
	v := provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	return func(ctx context.Context, raw string) (string, []string, error) {
		tok, err := v.Verify(ctx, raw)
		if err != nil {
			return "", nil, err
		}
		return tok.Subject, tok.Audience, nil
	}, nil
}

// mountSubscriptionLane registers /apis/, /mcp/, the protected-resource
// metadata and the /api/v1/ management plane on mux.
func mountSubscriptionLane(mux *http.ServeMux, auth *OIDCMiddleware, cfg laneConfig) (*Lane, error) {
	root := NewGatewayRoot(cfg.dataDir)
	creds := NewCredentialResolver(cfg.sealer)
	lane := NewLane(root, creds)
	lane.oauthIssuer = cfg.oauthIssuer
	lane.oauthAudience = cfg.oauthAudience
	lane.publicBaseURL = cfg.publicBaseURL
	lane.verify = cfg.verify
	if lane.oauthIssuer != "" && lane.verify == nil {
		v, err := oidcTokenVerifier(context.Background(), lane.oauthIssuer)
		if err != nil {
			return nil, err
		}
		lane.verify = v
	}
	if cfg.sealer == nil {
		log.Println("GATEWAY_SEAL_KEY not set — sealed credentials are disabled")
	}

	mux.HandleFunc("GET /apis/{owner}/_catalog", lane.ServeCatalog)
	mux.HandleFunc("/apis/{owner}/{api}/{rest...}", lane.ServeAPI)
	// Anything else under /apis/ is local, never the passthrough catch-all.
	mux.HandleFunc("/apis/", func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, "no such api", http.StatusNotFound)
	})
	mux.HandleFunc("/mcp/{owner}/{server}", lane.ServeMCP)
	mux.HandleFunc("/mcp/", func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, "no such mcp server", http.StatusNotFound)
	})
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/mcp/{owner}/{server}", lane.ServeProtectedResourceMetadata)
	mux.HandleFunc("/.well-known/", func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, "not found", http.StatusNotFound)
	})
	// ADR 0004: connectors dial in here (agent-tunnel appends /v1/control
	// to --server …/connect). Authenticated by the register frame's token.
	mux.Handle("GET /connect/v1/control", &connectorControl{root: root, hub: lane.hub})
	mux.HandleFunc("/connect/", func(w http.ResponseWriter, r *http.Request) {
		jsonError(w, "not found", http.StatusNotFound)
	})
	api := newGatewayAPI(root, creds, cfg.sealer)
	api.hub = lane.hub
	mux.Handle("/api/v1/", auth.Require()(api))
	return lane, nil
}
