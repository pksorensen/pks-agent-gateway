package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// Subscription-lane wiring (ADR 0001). Kept out of main() so tests mount the
// exact production routes.

type laneConfig struct {
	dataDir string
	sealer  *Sealer
	// identityDir holds the gateway's own key/cert for entra-cert
	// credentials (ADR 0006). Empty = {dataDir}/identity.
	identityDir string
	// MCP OAuth (FR-6a): the gateway is a protected resource only.
	oauthIssuer   string
	oauthAudience string
	publicBaseURL string
	verify        TokenVerifier // nil = derive from oauthIssuer via discovery
	// entraJWT enables entra-jwt caller auth on /apis/ (ADR 0006); nil = off.
	entraJWT *entraJWTConfig
}

func laneConfigFromEnv(dataDir string) (laneConfig, error) {
	sealer, err := NewSealerFromEnv()
	if err != nil {
		return laneConfig{}, err
	}
	var ej *entraJWTConfig
	if t := os.Getenv("ENTRA_JWT_TENANT"); t != "" {
		ej = &entraJWTConfig{
			Tenant:        t,
			Audiences:     splitList(os.Getenv("ENTRA_JWT_AUDIENCES")),
			AuthorityHost: env("ENTRA_AUTHORITY_HOST", "https://login.microsoftonline.com"),
		}
		if err := ej.validate(); err != nil {
			return laneConfig{}, err
		}
	}
	return laneConfig{
		entraJWT:      ej,
		dataDir:       dataDir,
		sealer:        sealer,
		identityDir:   os.Getenv("GATEWAY_IDENTITY_DIR"),
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
	creds.identityDir = cfg.identityDir
	if creds.identityDir == "" {
		creds.identityDir = filepath.Join(root.dataDir, "identity")
	}
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
	if cfg.entraJWT != nil {
		v, err := newEntraJWTVerifier(*cfg.entraJWT)
		if err != nil {
			return nil, err
		}
		lane.entra = v
		log.Printf("entra-jwt caller auth on /apis/: tenant %s, v2 tokens, audiences %v", cfg.entraJWT.Tenant, cfg.entraJWT.Audiences)
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

// splitList parses a comma/space separated env list.
func splitList(v string) []string {
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' })
}
