package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestIdentity runs `identity init` into a dir OUTSIDE the gateway's data
// dir, so assertNoCanary-style walks never see the key.
func newTestIdentity(t *testing.T) (string, *identityInfo) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "identity")
	info, created, err := initIdentity(dir, 2048, "test-gateway", 2, time.Now())
	if err != nil || !created {
		t.Fatalf("initIdentity: created=%v err=%v", created, err)
	}
	return dir, info
}

func TestIdentityInitNeverOverwrites(t *testing.T) {
	dir, first := newTestIdentity(t)
	fi, err := os.Stat(first.KeyPath)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode = %v, %v", fi.Mode().Perm(), err)
	}
	keyBefore, _ := os.ReadFile(first.KeyPath)

	var out, errOut bytes.Buffer
	if code := runIdentityCmd([]string{"init", "--dir", dir}, &out, &errOut); code != 0 {
		t.Fatalf("second init exit %d: %s", code, errOut.String())
	}
	keyAfter, _ := os.ReadFile(first.KeyPath)
	if !bytes.Equal(keyBefore, keyAfter) {
		t.Fatal("second init rewrote the key")
	}
	if !strings.Contains(out.String(), "not overwritten") || !strings.Contains(out.String(), first.Thumbprint) {
		t.Fatalf("second init output: %s", out.String())
	}
	if strings.Contains(out.String(), "PRIVATE KEY") {
		t.Fatal("init printed the private key")
	}

	// Half a pair is refused rather than "repaired".
	_ = os.Remove(filepath.Join(dir, identityCertFile))
	if _, _, err := initIdentity(dir, 2048, "x", 2, time.Now()); err != errIdentityPartial {
		t.Fatalf("partial identity: %v", err)
	}
}

func TestIdentityPathConfinement(t *testing.T) {
	dir, _ := newTestIdentity(t)
	outside := filepath.Join(t.TempDir(), "evil.pem")
	_ = os.WriteFile(outside, []byte("x"), 0o600)
	_ = os.Symlink(outside, filepath.Join(dir, "link.pem"))
	for _, p := range []string{"../evil.pem", outside, "link.pem", "/etc/passwd"} {
		if _, err := resolveIdentityPath(dir, p); err == nil {
			t.Errorf("%q escaped the identity dir", p)
		}
	}
	if _, err := resolveIdentityPath(dir, identityKeyFile); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveIdentityPath(dir, filepath.Join(dir, identityKeyFile)); err != nil {
		t.Fatalf("absolute path inside dir: %v", err)
	}
}

// fakeEntraIdP verifies the client assertion the way Entra does: RS256 over
// the registered certificate, x5t matching, aud = this token endpoint.
type fakeEntraIdP struct {
	*httptest.Server
	mints atomic.Int32
	fails atomic.Int32
	delay time.Duration
	mu    sync.Mutex
	last  map[string]any
}

func newFakeEntraIdP(t *testing.T, tenant, clientID string, info *identityInfo) *fakeEntraIdP {
	cert, err := x509.ParseCertificate(info.CertDER)
	if err != nil {
		t.Fatal(err)
	}
	pub := cert.PublicKey.(*rsa.PublicKey)
	f := &fakeEntraIdP{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(why string) {
			f.fails.Add(1)
			t.Logf("fake IdP rejected: %s", why)
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_client","error_codes":[700027]}`)
		}
		_ = r.ParseForm()
		endpoint := f.URL + "/" + tenant + "/oauth2/v2.0/token"
		if r.URL.Path != "/"+tenant+"/oauth2/v2.0/token" {
			fail("path " + r.URL.Path)
			return
		}
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("client_id") != clientID ||
			r.Form.Get("client_assertion_type") != "urn:ietf:params:oauth:client-assertion-type:jwt-bearer" ||
			r.Form.Get("client_secret") != "" || r.Form.Get("scope") != "https://cognitiveservices.azure.com/.default" {
			fail("form " + r.Form.Encode())
			return
		}
		parts := strings.Split(r.Form.Get("client_assertion"), ".")
		if len(parts) != 3 {
			fail("not a JWT")
			return
		}
		var hdr map[string]string
		hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
		_ = json.Unmarshal(hb, &hdr)
		if hdr["alg"] != "RS256" || hdr["x5t"] != info.X5t {
			fail("header " + string(hb))
			return
		}
		sig, _ := base64.RawURLEncoding.DecodeString(parts[2])
		digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
		if rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) != nil {
			fail("signature")
			return
		}
		var cl map[string]any
		cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
		_ = json.Unmarshal(cb, &cl)
		now := float64(time.Now().Unix())
		exp, _ := cl["exp"].(float64)
		nbf, _ := cl["nbf"].(float64)
		if cl["aud"] != endpoint || cl["iss"] != clientID || cl["sub"] != clientID || cl["jti"] == "" ||
			exp < now || nbf > now+1 || exp-nbf > 600 {
			fail("claims " + string(cb))
			return
		}
		f.mu.Lock()
		f.last = cl
		f.mu.Unlock()
		time.Sleep(f.delay)
		n := f.mints.Add(1)
		io.WriteString(w, `{"access_token":"cert-token-`+string(rune('0'+n))+`","expires_in":3600}`)
	}))
	t.Cleanup(f.Close)
	return f
}

const testTenant = "11111111-2222-3333-4444-555555555555"

func TestLaneEntraCertCredential(t *testing.T) {
	idDir, info := newTestIdentity(t)
	idp := newFakeEntraIdP(t, testTenant, "upstream-app", info)
	idp.delay = 50 * time.Millisecond
	t.Setenv("ENTRA_AUTHORITY_HOST", idp.URL)

	up := newFakeUpstream(t)
	g := newTestGateway(t, func(c *laneConfig) { c.identityDir = idDir })
	cred := map[string]any{"id": "foundry", "source": "entra-cert", "tenant": testTenant, "clientId": "upstream-app",
		"scope": "https://cognitiveservices.azure.com/.default", "keyFile": "key.pem", "certFile": "cert.pem"}
	view := g.must(201, "POST", "/api/v1/owners/test/credentials", cred)
	if !strings.Contains(view, `"hasValue": true`) || strings.Contains(view, "PRIVATE") {
		t.Fatalf("credential view: %s", view)
	}
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "foundry", "upstream": up.URL, "credential": "foundry",
		"operations": []map[string]any{{"id": "transcribe", "method": "POST", "path": "/transcribe"}}})
	g.must(201, "POST", "/api/v1/owners/test/bundles", map[string]any{"id": "b", "apis": []string{"foundry"}})
	g.must(201, "POST", "/api/v1/owners/test/principals", map[string]any{"id": "bot", "kind": "service"})
	var issued issuedKeys
	_ = json.Unmarshal([]byte(g.must(201, "POST", "/api/v1/owners/test/subscriptions", map[string]any{"bundle": "b", "principal": "bot"})), &issued)

	// Ten concurrent first calls share one mint (single-flight).
	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = g.do("POST", "/apis/test/foundry/transcribe", "{}", g.key(issued.PrimaryKey))
		}(i)
	}
	wg.Wait()
	for i, c := range codes {
		if c != 200 {
			t.Fatalf("call %d: %d (IdP fails=%d)", i, c, idp.fails.Load())
		}
	}
	if idp.mints.Load() != 1 {
		t.Fatalf("minted %d tokens, want 1", idp.mints.Load())
	}
	if up.last().Header.Get("Authorization") != "Bearer cert-token-1" {
		t.Fatalf("token not injected: %q", up.last().Header.Get("Authorization"))
	}

	// Nothing key-shaped lands in the store.
	_ = filepath.Walk(g.dataDir, func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte("PRIVATE KEY")) {
				t.Fatalf("key material in store file %s", p)
			}
		}
		return nil
	})

	// Rotation drops the cached token: the next call mints again.
	g.must(200, "POST", "/api/v1/owners/test/credentials/foundry/rotate", map[string]any{})
	if c, _ := g.do("POST", "/apis/test/foundry/transcribe", "{}", g.key(issued.PrimaryKey)); c != 200 || idp.mints.Load() != 2 {
		t.Fatalf("after rotate: %d, mints=%d", c, idp.mints.Load())
	}

	// A key that becomes group-readable fails closed at the next mint.
	g.must(200, "POST", "/api/v1/owners/test/credentials/foundry/rotate", map[string]any{})
	_ = os.Chmod(info.KeyPath, 0o640)
	if c, out := g.do("POST", "/apis/test/foundry/transcribe", "{}", g.key(issued.PrimaryKey)); c != 502 || strings.Contains(out, "0640") {
		t.Fatalf("open key mode: %d %s", c, out)
	}
	if v := g.must(200, "GET", "/api/v1/owners/test/credentials/foundry", nil); !strings.Contains(v, `"hasValue": false`) {
		t.Fatalf("view should report the unusable key: %s", v)
	}
}

func TestEntraCertCredentialValidation(t *testing.T) {
	idDir, info := newTestIdentity(t)
	g := newTestGateway(t, func(c *laneConfig) { c.identityDir = idDir })
	base := func(mut func(m map[string]any)) map[string]any {
		m := map[string]any{"id": "c", "source": "entra-cert", "tenant": testTenant, "clientId": "app",
			"scope": "s/.default", "keyFile": "key.pem", "certFile": "cert.pem"}
		mut(m)
		return m
	}
	for name, body := range map[string]map[string]any{
		"traversal":    base(func(m map[string]any) { m["keyFile"] = "../identity/../../etc/passwd" }),
		"absolute out": base(func(m map[string]any) { m["certFile"] = "/etc/ssl/cert.pem" }),
		"no tenant":    base(func(m map[string]any) { delete(m, "tenant") }),
		"no scope":     base(func(m map[string]any) { delete(m, "scope") }),
		"missing file": base(func(m map[string]any) { m["keyFile"] = "nope.pem" }),
	} {
		if c, out := g.do("POST", "/api/v1/owners/test/credentials", body, nil); c != 400 {
			t.Errorf("%s: %d %s", name, c, out)
		}
	}
	_ = os.Chmod(info.KeyPath, 0o644)
	if c, out := g.do("POST", "/api/v1/owners/test/credentials", base(func(map[string]any) {}), nil); c != 400 || !strings.Contains(out, "chmod 600") {
		t.Fatalf("world-readable key accepted: %d %s", c, out)
	}
}

func TestEntraCertNeedsRealmAdmin(t *testing.T) {
	idDir, _ := newTestIdentity(t)
	root := NewGatewayRoot(t.TempDir())
	if _, err := root.Claim("acme", "owner-admin", time.Now()); err != nil {
		t.Fatal(err)
	}
	creds := NewCredentialResolver(nil)
	creds.identityDir = idDir
	a := newGatewayAPI(root, creds, nil)
	body, _ := json.Marshal(map[string]any{"id": "c", "source": "entra-cert", "tenant": testTenant, "clientId": "app",
		"scope": "s/.default", "keyFile": "key.pem", "certFile": "cert.pem"})
	req := httptest.NewRequest("POST", "/api/v1/owners/acme/credentials", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), claimsKey, &UserClaims{Sub: "owner-admin"}))
	w := httptest.NewRecorder()
	a.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("owner admin (not realm admin) created entra-cert: %d %s", w.Code, w.Body.String())
	}
}

func TestClientAssertionShape(t *testing.T) {
	idDir, info := newTestIdentity(t)
	id, err := loadAssertionIdentity(idDir, "key.pem", "cert.pem")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0)
	jwt, err := clientAssertion(id, "app", "https://login.example/t/oauth2/v2.0/token", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(jwt, ".")
	hb, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var hdr map[string]string
	_ = json.Unmarshal(hb, &hdr)
	if hdr["x5t"] != info.X5t || hdr["x5t#S256"] == "" || hdr["alg"] != "RS256" {
		t.Fatalf("header: %s", hb)
	}
	// The printed thumbprint and the x5t header are the same SHA-1.
	raw, _ := base64.RawURLEncoding.DecodeString(info.X5t)
	if strings.ToUpper(strings.TrimSpace(hexOf(raw))) != info.Thumbprint {
		t.Fatalf("thumbprint %s vs x5t %s", info.Thumbprint, info.X5t)
	}
	blk, _ := pem.Decode(mustRead(t, info.KeyPath))
	if blk.Type != "PRIVATE KEY" {
		t.Fatalf("key PEM type %q", blk.Type)
	}
}

func hexOf(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, 2*len(b))
	for _, c := range b {
		out = append(out, digits[c>>4], digits[c&15])
	}
	return string(out)
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
