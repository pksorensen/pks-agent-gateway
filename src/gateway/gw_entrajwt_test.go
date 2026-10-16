package main

import (
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeEntraIssuer serves v2 discovery + JWKS for one tenant and signs tokens.
type fakeEntraIssuer struct {
	*httptest.Server
	key       *rsa.PrivateKey
	kid       string
	jwksHits  atomic.Int32
	tenant    string
	gatewayID string // the gateway app's client id = accepted aud
}

func newFakeEntraIssuer(t *testing.T) *fakeEntraIssuer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeEntraIssuer{key: key, kid: "kid-1", tenant: testTenant, gatewayID: "99999999-aaaa-bbbb-cccc-dddddddddddd"}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /"+testTenant+"/v2.0/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{
			"issuer":   f.URL + "/" + testTenant + "/v2.0",
			"jwks_uri": f.URL + "/" + testTenant + "/discovery/v2.0/keys",
		})
	})
	mux.HandleFunc("GET /"+testTenant+"/discovery/v2.0/keys", func(w http.ResponseWriter, r *http.Request) {
		f.jwksHits.Add(1)
		e := big.NewInt(int64(f.key.E)).Bytes()
		json.NewEncoder(w).Encode(map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "use": "sig", "kid": f.kid,
			"n": base64.RawURLEncoding.EncodeToString(f.key.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString(e),
		}}})
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

// claims returns a valid app-only v2 token's claims for caller appID.
func (f *fakeEntraIssuer) claims(appID string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{
		"iss": f.URL + "/" + f.tenant + "/v2.0", "aud": f.gatewayID, "tid": f.tenant,
		"azp": appID, "azpacr": "2", "ver": "2.0", "idtyp": "app", "roles": []string{"Speech.Transcribe"},
		"iat": now, "nbf": now, "exp": now + 3600, "oid": "obj-" + appID, "sub": "obj-" + appID,
	}
}

func signJWT(t *testing.T, key *rsa.PrivateKey, hdr, claims map[string]any) string {
	t.Helper()
	hb, _ := json.Marshal(hdr)
	cb, _ := json.Marshal(claims)
	in := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
	d := sha256.Sum256([]byte(in))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, d[:])
	if err != nil {
		t.Fatal(err)
	}
	return in + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (f *fakeEntraIssuer) token(t *testing.T, mut func(c map[string]any)) string {
	c := f.claims("caller-app")
	if mut != nil {
		mut(c)
	}
	return signJWT(t, f.key, map[string]any{"alg": "RS256", "typ": "JWT", "kid": f.kid}, c)
}

func bearer(tok string) map[string]string { return map[string]string{"Authorization": "Bearer " + tok} }

func newEntraGateway(t *testing.T) (*gw, *fakeEntraIssuer, *fakeUpstream) {
	iss := newFakeEntraIssuer(t)
	up := newFakeUpstream(t)
	g := newTestGateway(t, func(c *laneConfig) {
		c.entraJWT = &entraJWTConfig{Tenant: testTenant, Audiences: []string{iss.gatewayID}, AuthorityHost: iss.URL}
	})
	g.must(201, "POST", "/api/v1/owners/test/credentials", map[string]any{
		"id": "speech-key", "source": "sealed", "value": canary, "inject": map[string]any{"header": "api-key"}})
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "speech", "upstream": up.URL, "credential": "speech-key",
		"operations": []map[string]any{{"id": "transcribe", "method": "POST", "path": "/transcribe"}}})
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "other", "upstream": up.URL,
		"operations": []map[string]any{{"id": "x", "method": "GET", "path": "/x"}}})
	g.must(201, "POST", "/api/v1/owners/test/bundles", map[string]any{"id": "speech", "apis": []string{"speech"}})
	g.must(201, "POST", "/api/v1/owners/test/principals", map[string]any{"id": "gjelvand-map", "appId": "caller-app"})
	out := g.must(201, "POST", "/api/v1/owners/test/subscriptions", map[string]any{"bundle": "speech", "principal": "gjelvand-map", "keyless": true})
	if strings.Contains(out, "gwk_") || !strings.Contains(out, `"keyless": true`) {
		t.Fatalf("keyless subscription: %s", out)
	}
	return g, iss, up
}

func TestEntraJWTRejectMatrix(t *testing.T) {
	g, iss, up := newEntraGateway(t)
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	hs256 := func() string {
		hb, _ := json.Marshal(map[string]any{"alg": "HS256", "kid": iss.kid})
		cb, _ := json.Marshal(iss.claims("caller-app"))
		in := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb)
		m := hmac.New(sha256.New, iss.key.N.Bytes())
		m.Write([]byte(in))
		return in + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	}
	none := func() string {
		hb, _ := json.Marshal(map[string]any{"alg": "none"})
		cb, _ := json.Marshal(iss.claims("caller-app"))
		return base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(cb) + "."
	}

	cases := []struct {
		name string
		tok  string
		want int
	}{
		{"good", iss.token(t, nil), 200},
		{"wrong iss (v1)", iss.token(t, func(c map[string]any) { c["iss"] = "https://sts.windows.net/" + testTenant + "/"; c["ver"] = "1.0" }), 401},
		{"wrong iss (v2 other tenant)", iss.token(t, func(c map[string]any) { c["iss"] = iss.URL + "/00000000-0000-0000-0000-000000000000/v2.0" }), 401},
		{"wrong tid", iss.token(t, func(c map[string]any) { c["tid"] = "00000000-0000-0000-0000-000000000000" }), 401},
		{"wrong aud", iss.token(t, func(c map[string]any) { c["aud"] = "api://something-else" }), 401},
		{"expired", iss.token(t, func(c map[string]any) { c["exp"] = time.Now().Add(-10 * time.Minute).Unix() }), 401},
		{"not yet valid", iss.token(t, func(c map[string]any) { c["nbf"] = time.Now().Add(10 * time.Minute).Unix() }), 401},
		{"no exp", iss.token(t, func(c map[string]any) { delete(c, "exp") }), 401},
		{"bad sig", signJWT(t, otherKey, map[string]any{"alg": "RS256", "kid": iss.kid}, iss.claims("caller-app")), 401},
		{"alg none", none(), 401},
		{"alg HS256", hs256(), 401},
		{"delegated (scp)", iss.token(t, func(c map[string]any) { c["scp"] = "user_impersonation" }), 401},
		{"idtyp user", iss.token(t, func(c map[string]any) { c["idtyp"] = "user" }), 401},
		{"garbage", "not.a.jwt", 401},
		{"unknown appid", iss.token(t, func(c map[string]any) { c["azp"] = "stranger-app" }), 403},
	}
	for _, tc := range cases {
		code, out := g.do("POST", "/apis/test/speech/transcribe", "{}", bearer(tc.tok))
		if code != tc.want {
			t.Errorf("%s: got %d want %d: %s", tc.name, code, tc.want, out)
		}
	}

	// The good call reached upstream with the API credential, not the token.
	hit := up.last()
	if hit.Header.Get("api-key") != canary || hit.Header.Get("Authorization") != "" {
		t.Fatalf("upstream headers: api-key=%q auth=%q", hit.Header.Get("api-key"), hit.Header.Get("Authorization"))
	}

	// Not in the bundle → 403; disabled principal → 403.
	good := iss.token(t, nil)
	if code, _ := g.do("GET", "/apis/test/other/x", nil, bearer(good)); code != 403 {
		t.Fatalf("api outside bundle: %d", code)
	}
	g.must(200, "POST", "/api/v1/owners/test/principals/gjelvand-map/disable", nil)
	if code, _ := g.do("POST", "/apis/test/speech/transcribe", "{}", bearer(good)); code != 403 {
		t.Fatalf("disabled principal: %d", code)
	}
	g.must(200, "POST", "/api/v1/owners/test/principals/gjelvand-map/enable", nil)

	// Usage is attributed to the principal.
	usage := g.must(200, "GET", "/api/v1/owners/test/usage?records=1", nil)
	if !strings.Contains(usage, `"principal": "gjelvand-map"`) {
		t.Fatalf("usage: %s", usage)
	}
	g.assertNoCanary(usage)
}

func TestEntraJWTKeylessAndJWKSRefresh(t *testing.T) {
	g, iss, _ := newEntraGateway(t)

	// Keyless: no keys to regenerate; principal without appId can't have one.
	if code, _ := g.do("POST", "/api/v1/owners/test/subscriptions/gjelvand-map-speech/regenerate", map[string]any{"slot": "primary"}, nil); code != 409 {
		t.Fatalf("regenerate keyless: %d", code)
	}
	g.must(201, "POST", "/api/v1/owners/test/principals", map[string]any{"id": "kim", "sub": "kim-sub"})
	if code, _ := g.do("POST", "/api/v1/owners/test/subscriptions", map[string]any{"bundle": "speech", "principal": "kim", "keyless": true}, nil); code != 400 {
		t.Fatalf("keyless for human: %d", code)
	}
	if code, _ := g.do("POST", "/api/v1/owners/test/principals", map[string]any{"id": "dup", "appId": "CALLER-APP"}, nil); code != 409 {
		t.Fatalf("duplicate appId: %d", code)
	}

	// One JWKS fetch serves many calls; an unknown kid refetches once, then
	// is throttled.
	for i := 0; i < 3; i++ {
		if code, out := g.do("POST", "/apis/test/speech/transcribe", "{}", bearer(iss.token(t, nil))); code != 200 {
			t.Fatalf("call %d: %d %s", i, code, out)
		}
	}
	if n := iss.jwksHits.Load(); n != 1 {
		t.Fatalf("jwks fetched %d times, want 1", n)
	}
	unknown := signJWT(t, iss.key, map[string]any{"alg": "RS256", "kid": "rolled-kid"}, iss.claims("caller-app"))
	// Within jwksMinRefresh of the first fetch, an unknown kid does not refetch.
	if code, _ := g.do("POST", "/apis/test/speech/transcribe", "{}", bearer(unknown)); code != 401 || iss.jwksHits.Load() != 1 {
		t.Fatalf("unknown kid inside throttle window: %d, fetches %d", code, iss.jwksHits.Load())
	}
	g.lane.entra.now = func() time.Time { return time.Now().Add(jwksMinRefresh + time.Second) }
	for i := 0; i < 5; i++ {
		if code, _ := g.do("POST", "/apis/test/speech/transcribe", "{}", bearer(unknown)); code != 401 {
			t.Fatalf("unknown kid: %d", code)
		}
	}
	if n := iss.jwksHits.Load(); n != 2 {
		t.Fatalf("jwks fetched %d times after unknown kids, want 2 (throttled)", n)
	}
}

func TestEntraJWTDisabledByDefault(t *testing.T) {
	iss := newFakeEntraIssuer(t)
	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "speech", "upstream": up.URL,
		"operations": []map[string]any{{"id": "t", "method": "POST", "path": "/t"}}})
	if code, _ := g.do("POST", "/apis/test/speech/t", "{}", bearer(iss.token(t, nil))); code != 401 || up.count() != 0 {
		t.Fatalf("entra token without ENTRA_JWT_TENANT: %d, upstream hits %d", code, up.count())
	}
}

func TestEntraJWTConfigValidation(t *testing.T) {
	if (entraJWTConfig{Tenant: "contoso.onmicrosoft.com", Audiences: []string{"x"}}).validate() == nil {
		t.Fatal("non-GUID tenant accepted")
	}
	if (entraJWTConfig{Tenant: testTenant}).validate() == nil {
		t.Fatal("empty audiences accepted")
	}
}
