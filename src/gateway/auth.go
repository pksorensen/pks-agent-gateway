package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
)

// OIDCMiddleware verifies Bearer tokens against an OIDC issuer (e.g. Keycloak).
// When created via DevModeMiddleware it is a no-op that admits every request as
// an anonymous user.
type OIDCMiddleware struct {
	verifier *oidc.IDTokenVerifier // nil in dev mode
	devMode  bool
}

// NewOIDCMiddleware creates an OIDCMiddleware that verifies tokens issued by
// issuer. It performs an OIDC discovery request on initialisation.
func NewOIDCMiddleware(ctx context.Context, issuer string) (*OIDCMiddleware, error) {
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, err
	}
	verifier := provider.Verifier(&oidc.Config{SkipClientIDCheck: true})
	return &OIDCMiddleware{verifier: verifier}, nil
}

// DevModeMiddleware returns a middleware that admits all requests without
// verifying any token.
func DevModeMiddleware() *OIDCMiddleware {
	return &OIDCMiddleware{devMode: true}
}

// Require returns an http middleware that:
//  1. Extracts the Bearer token from Authorization header.
//  2. Verifies it (skipped in dev mode).
//  3. Checks that the user holds at least one of the supplied roles (if any are
//     specified).
//  4. Attaches UserClaims to the request context.
//
// Responds 401 for missing/invalid tokens, 403 for insufficient roles.
func (m *OIDCMiddleware) Require(roles ...string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var claims *UserClaims

			if m.devMode {
				// Dev mode: synthesise an anonymous admin so all routes work.
				claims = &UserClaims{
					Sub:   "dev",
					Email: "dev@localhost",
					Roles: []string{RoleGatewayAdmin, RoleProjectAdmin, RoleProjectUser},
				}
			} else {
				token, ok := bearerToken(r)
				if !ok {
					http.Error(w, "gateway: missing or malformed Authorization header", http.StatusUnauthorized)
					return
				}

				idToken, err := m.verifier.Verify(r.Context(), token)
				if err != nil {
					http.Error(w, "gateway: invalid token: "+err.Error(), http.StatusUnauthorized)
					return
				}

				claims, err = extractClaims(idToken)
				if err != nil {
					http.Error(w, "gateway: could not extract claims: "+err.Error(), http.StatusUnauthorized)
					return
				}
			}

			// Role check (only when caller specified required roles).
			if len(roles) > 0 {
				allowed := false
				for _, role := range roles {
					if claims.HasRole(role) {
						allowed = true
						break
					}
				}
				if !allowed {
					http.Error(w, "gateway: insufficient roles", http.StatusForbidden)
					return
				}
			}

			next.ServeHTTP(w, r.WithContext(withClaims(r.Context(), claims)))
		})
	}
}

// bearerToken extracts the raw JWT from an "Authorization: Bearer <token>"
// header.
func bearerToken(r *http.Request) (string, bool) {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return "", false
	}
	parts := strings.SplitN(auth, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return "", false
	}
	return strings.TrimSpace(parts[1]), true
}

// rawClaims is used to decode the JWT payload for role extraction.
type rawClaims struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`

	// Keycloak puts roles under realm_access.roles.
	RealmAccess struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`

	// Entra (and other providers) may put roles directly in the "roles" claim.
	Roles []string `json:"roles"`
}

func extractClaims(idToken *oidc.IDToken) (*UserClaims, error) {
	var rc rawClaims
	if err := idToken.Claims(&rc); err != nil {
		return nil, err
	}

	// Merge roles from both locations — Keycloak and Entra.
	seen := map[string]bool{}
	var roles []string
	for _, r := range rc.RealmAccess.Roles {
		if !seen[r] {
			seen[r] = true
			roles = append(roles, r)
		}
	}
	for _, r := range rc.Roles {
		if !seen[r] {
			seen[r] = true
			roles = append(roles, r)
		}
	}

	return &UserClaims{
		Sub:   rc.Sub,
		Email: rc.Email,
		Roles: roles,
	}, nil
}

// Ensure json import is used (it's used indirectly via oidc.IDToken.Claims).
var _ = json.Marshal
