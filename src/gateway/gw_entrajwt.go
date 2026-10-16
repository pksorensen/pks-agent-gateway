package main

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"
)

// entra-jwt caller auth (ADR 0006): a workload presents an Entra **v2**
// app-only access token instead of a gwk_ key. One issuer tenant, one token
// version — v1 tokens (iss https://sts.windows.net/…) are refused, not
// tolerated. The token's azp (the caller's app id) maps to a Principal with a
// matching appId; that principal's subscriptions decide what it may call.

type entraJWTConfig struct {
	Tenant        string   // issuer tenant id (GUID)
	Audiences     []string // accepted aud values, e.g. the gateway app's client id
	AuthorityHost string   // https://login.microsoftonline.com; tests point it at httptest
	Skew          time.Duration
}

var guidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (c entraJWTConfig) validate() error {
	if !guidPattern.MatchString(c.Tenant) {
		return fmt.Errorf("ENTRA_JWT_TENANT must be a tenant id (GUID)")
	}
	if len(c.Audiences) == 0 {
		return fmt.Errorf("ENTRA_JWT_AUDIENCES is required when ENTRA_JWT_TENANT is set")
	}
	return nil
}

func (c entraJWTConfig) authority() string {
	return strings.TrimRight(c.AuthorityHost, "/") + "/" + c.Tenant
}

// issuer is the exact v2 iss for the tenant.
func (c entraJWTConfig) issuer() string { return c.authority() + "/v2.0" }

const (
	// jwksMinRefresh rate-limits refetches triggered by an unknown kid, so
	// garbage tokens can't turn the gateway into a JWKS amplifier.
	jwksMinRefresh = 30 * time.Second
	// jwksMaxAge forces a periodic refresh; Entra rolls signing keys.
	jwksMaxAge = 24 * time.Hour
)

type entraJWTVerifier struct {
	cfg    entraJWTConfig
	client *http.Client
	now    func() time.Time

	mu        sync.Mutex // guards keys, fetchedAt, triedAt
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
	triedAt   time.Time
	fetchMu   sync.Mutex // one JWKS fetch at a time
}

func newEntraJWTVerifier(cfg entraJWTConfig) (*entraJWTVerifier, error) {
	if cfg.AuthorityHost == "" {
		cfg.AuthorityHost = "https://login.microsoftonline.com"
	}
	if cfg.Skew == 0 {
		cfg.Skew = 2 * time.Minute
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &entraJWTVerifier{cfg: cfg, client: &http.Client{Timeout: 10 * time.Second}, now: time.Now}, nil
}

var errEntraToken = errors.New("invalid access token")

// entraClaims are the v2 access-token claims the gateway checks.
type entraClaims struct {
	Iss   string          `json:"iss"`
	Aud   json.RawMessage `json:"aud"`
	Tid   string          `json:"tid"`
	Azp   string          `json:"azp"`
	Ver   string          `json:"ver"`
	Idtyp string          `json:"idtyp"`
	Scp   json.RawMessage `json:"scp"`
	Exp   *float64        `json:"exp"`
	Nbf   *float64        `json:"nbf"`
}

func (c *entraClaims) audiences() []string {
	var one string
	if json.Unmarshal(c.Aud, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(c.Aud, &many)
	return many
}

// Verify checks raw and returns the caller's app id (azp). Every failure is
// errEntraToken (wrapped with the reason for the log, never the token).
func (v *entraJWTVerifier) Verify(ctx context.Context, raw string) (string, error) {
	fail := func(why string) (string, error) { return "", fmt.Errorf("%w: %s", errEntraToken, why) }
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return fail("not a JWS")
	}
	var hdr struct {
		Alg string `json:"alg"`
		Kid string `json:"kid"`
	}
	if b, err := base64.RawURLEncoding.DecodeString(parts[0]); err != nil || json.Unmarshal(b, &hdr) != nil {
		return fail("bad header")
	}
	// RS256 only: no "none", no HS256 with the public key as secret.
	if hdr.Alg != "RS256" || hdr.Kid == "" {
		return fail("alg " + hdr.Alg)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fail("bad signature encoding")
	}
	key, err := v.key(ctx, hdr.Kid)
	if err != nil {
		return fail(err.Error())
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, digest[:], sig) != nil {
		return fail("signature")
	}

	var c entraClaims
	if b, err := base64.RawURLEncoding.DecodeString(parts[1]); err != nil || json.Unmarshal(b, &c) != nil {
		return fail("bad claims")
	}
	now := v.now()
	switch {
	case c.Ver != "2.0":
		return fail("token version " + c.Ver)
	case c.Iss != v.cfg.issuer():
		return fail("issuer")
	case c.Tid != v.cfg.Tenant:
		return fail("tenant")
	case !anyIn(c.audiences(), v.cfg.Audiences):
		return fail("audience")
	case c.Exp == nil || now.After(time.Unix(int64(*c.Exp), 0).Add(v.cfg.Skew)):
		return fail("expired")
	case c.Nbf != nil && now.Add(v.cfg.Skew).Before(time.Unix(int64(*c.Nbf), 0)):
		return fail("not yet valid")
	case len(c.Scp) > 0 && string(c.Scp) != "null":
		return fail("delegated token (scp present)")
	case c.Idtyp != "" && c.Idtyp != "app":
		return fail("idtyp " + c.Idtyp)
	case c.Azp == "":
		return fail("no azp")
	}
	return c.Azp, nil
}

func anyIn(have, want []string) bool {
	for _, h := range have {
		if contains(want, h) {
			return true
		}
	}
	return false
}

// key returns the signing key for kid, fetching JWKS lazily: on first use,
// when older than jwksMaxAge, or when the kid is unknown — at most once per
// jwksMinRefresh. A boot without network still starts; requests fail closed
// until a fetch succeeds, and a failed refresh keeps serving known keys.
func (v *entraJWTVerifier) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	lookup := func() (*rsa.PublicKey, bool, bool) {
		k, ok := v.keys[kid]
		fresh := !v.fetchedAt.IsZero() && v.now().Sub(v.fetchedAt) <= jwksMaxAge
		return k, ok, fresh
	}
	v.mu.Lock()
	k, ok, fresh := lookup()
	v.mu.Unlock()
	if ok && fresh {
		return k, nil
	}

	v.fetchMu.Lock()
	defer v.fetchMu.Unlock()
	v.mu.Lock()
	k, ok, fresh = lookup() // another caller may have refreshed meanwhile
	throttled := v.now().Sub(v.triedAt) < jwksMinRefresh
	if !(ok && fresh) && !throttled {
		v.triedAt = v.now()
	}
	v.mu.Unlock()
	if ok && (fresh || throttled) {
		return k, nil
	}
	if throttled {
		return nil, fmt.Errorf("unknown kid")
	}

	keys, err := v.fetchJWKS(ctx)
	v.mu.Lock()
	defer v.mu.Unlock()
	if err == nil {
		v.keys, v.fetchedAt = keys, v.now()
	}
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jwks: %v", err)
	}
	return nil, fmt.Errorf("unknown kid")
}

func (v *entraJWTVerifier) fetchJWKS(ctx context.Context) (map[string]*rsa.PublicKey, error) {
	var disc struct {
		Issuer  string `json:"issuer"`
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, v.cfg.issuer()+"/.well-known/openid-configuration", &disc); err != nil {
		return nil, err
	}
	if disc.Issuer != v.cfg.issuer() {
		return nil, fmt.Errorf("discovery issuer %q does not match %q", disc.Issuer, v.cfg.issuer())
	}
	if !strings.HasPrefix(disc.JWKSURI, strings.TrimRight(v.cfg.AuthorityHost, "/")+"/") {
		return nil, fmt.Errorf("jwks_uri is not on the authority host")
	}
	var set struct {
		Keys []struct {
			Kty string `json:"kty"`
			Use string `json:"use"`
			Kid string `json:"kid"`
			N   string `json:"n"`
			E   string `json:"e"`
		} `json:"keys"`
	}
	if err := v.getJSON(ctx, disc.JWKSURI, &set); err != nil {
		return nil, err
	}
	out := map[string]*rsa.PublicKey{}
	for _, k := range set.Keys {
		if k.Kty != "RSA" || (k.Use != "" && k.Use != "sig") || k.Kid == "" {
			continue
		}
		nb, err1 := base64.RawURLEncoding.DecodeString(k.N)
		eb, err2 := base64.RawURLEncoding.DecodeString(k.E)
		if err1 != nil || err2 != nil || len(eb) == 0 || len(eb) > 4 {
			continue
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: int(new(big.Int).SetBytes(eb).Int64())}
		if pub.N.BitLen() < 2048 {
			continue
		}
		out[k.Kid] = pub
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("jwks has no usable RSA keys")
	}
	return out, nil
}

func (v *entraJWTVerifier) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s failed", u)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: %s", u, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

// bearerEntraToken returns a Bearer that is neither a gwk_ key nor a gwc_
// connector token — the only shape the entra-jwt path considers.
func bearerEntraToken(r *http.Request) (string, bool) {
	tok, ok := bearerToken(r)
	if !ok || strings.HasPrefix(tok, gatewayKeyPrefix) || strings.HasPrefix(tok, connectorKeyPrefix) {
		return "", false
	}
	return tok, true
}

// authorizeEntra maps a verified token's azp to a principal (by appId) and
// that principal's active subscriptions. Bad token → 401; unknown app → 403.
func (l *Lane) authorizeEntra(ctx context.Context, st *GatewayStore, raw string, covers func(*Bundle) bool) (*grant, *denial) {
	appID, err := l.entra.Verify(ctx, raw)
	if err != nil {
		// The reason only (issuer, audience, expired, …) — never the token.
		log.Printf("lane: owner=%s entra-jwt rejected: %v", st.owner, err)
		return nil, &denial{http.StatusUnauthorized, "invalid access token"}
	}
	p, err := st.PrincipalByAppID(appID)
	if err != nil {
		return nil, &denial{http.StatusForbidden, "no principal for this app"}
	}
	return l.grantForPrincipal(st, p, covers)
}
