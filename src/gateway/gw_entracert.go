package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// entra-cert credentials (ADR 0006): a client-credentials token minted with a
// certificate client assertion. The private key is read from a 0600 file
// under GATEWAY_IDENTITY_DIR on every mint and never lives in config, env or
// the store — only its path does.

// assertionLifetime bounds the client assertion JWT; Entra accepts up to an
// hour, a short one limits replay of a captured assertion.
const assertionLifetime = 10 * time.Minute

// assertionIdentity is a loaded key + certificate pair.
type assertionIdentity struct {
	key     *rsa.PrivateKey
	certDER []byte
}

// x5t is base64url(SHA-1(cert DER)) — the thumbprint Entra matches against
// the app's keyCredentials.
func (a *assertionIdentity) x5t() string {
	sum := sha1.Sum(a.certDER)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func (a *assertionIdentity) x5tS256() string {
	sum := sha256.Sum256(a.certDER)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// resolveIdentityPath confines p to dir: relative paths join dir, absolute
// ones must already be inside it, and symlinks may not lead out.
func resolveIdentityPath(dir, p string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("GATEWAY_IDENTITY_DIR is not set")
	}
	base, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	full := p
	if !filepath.IsAbs(full) {
		full = filepath.Join(base, p)
	}
	full = filepath.Clean(full)
	if !within(base, full) {
		return "", fmt.Errorf("%q is outside the identity dir", p)
	}
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return "", fmt.Errorf("identity dir: %v", err)
	}
	realFull, err := filepath.EvalSymlinks(full)
	if err != nil {
		return "", fmt.Errorf("%q: %v", p, errors.Unwrap(err))
	}
	if !within(realBase, realFull) {
		return "", fmt.Errorf("%q resolves outside the identity dir", p)
	}
	return realFull, nil
}

func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// loadAssertionIdentity reads and checks the key and certificate. The key
// file must be a regular file readable by its owner only.
func loadAssertionIdentity(dir, keyFile, certFile string) (*assertionIdentity, error) {
	kp, err := resolveIdentityPath(dir, keyFile)
	if err != nil {
		return nil, fmt.Errorf("keyFile: %v", err)
	}
	cp, err := resolveIdentityPath(dir, certFile)
	if err != nil {
		return nil, fmt.Errorf("certFile: %v", err)
	}
	keyPEM, err := readPrivateFile(kp)
	if err != nil {
		return nil, fmt.Errorf("keyFile: %v", err)
	}
	key, err := parseRSAPrivateKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("keyFile: %v", err)
	}
	certPEM, err := os.ReadFile(cp)
	if err != nil {
		return nil, fmt.Errorf("certFile: %v", errors.Unwrap(err))
	}
	blk, _ := pem.Decode(certPEM)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("certFile: no PEM CERTIFICATE block")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("certFile: %v", err)
	}
	pub, ok := cert.PublicKey.(*rsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		return nil, fmt.Errorf("certFile does not match keyFile")
	}
	return &assertionIdentity{key: key, certDER: blk.Bytes}, nil
}

// readPrivateFile refuses a key that group or others could read.
func readPrivateFile(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, errors.Unwrap(err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file")
	}
	if perm := fi.Mode().Perm(); perm&0o077 != 0 {
		return nil, fmt.Errorf("mode %04o is too open; chmod 600", perm)
	}
	return io.ReadAll(io.LimitReader(f, 64<<10))
}

func parseRSAPrivateKey(data []byte) (*rsa.PrivateKey, error) {
	blk, _ := pem.Decode(data)
	if blk == nil {
		return nil, fmt.Errorf("no PEM block")
	}
	switch blk.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(blk.Bytes)
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
		if err != nil {
			return nil, err
		}
		rk, ok := k.(*rsa.PrivateKey)
		if !ok {
			return nil, fmt.Errorf("key is not RSA")
		}
		return rk, nil
	}
	return nil, fmt.Errorf("unsupported PEM block %q", blk.Type)
}

// clientAssertion builds the RS256 JWT Entra expects for
// urn:ietf:params:oauth:client-assertion-type:jwt-bearer.
func clientAssertion(id *assertionIdentity, clientID, audience string, now time.Time) (string, error) {
	jti := make([]byte, 16)
	if _, err := rand.Read(jti); err != nil {
		return "", err
	}
	header, _ := json.Marshal(map[string]string{
		"alg": "RS256", "typ": "JWT", "x5t": id.x5t(), "x5t#S256": id.x5tS256(),
	})
	claims, _ := json.Marshal(map[string]any{
		"aud": audience,
		"iss": clientID,
		"sub": clientID,
		"jti": base64.RawURLEncoding.EncodeToString(jti),
		"iat": now.Unix(),
		"nbf": now.Unix(),
		"exp": now.Add(assertionLifetime).Unix(),
	})
	signing := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, id.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// certCacheKey: entra-cert tokens are per (tenant, clientId, scope) — two
// credentials naming the same app and scope share one token.
func certCacheKey(c *Credential) string {
	return "entra-cert|" + c.Tenant + "|" + c.ClientID + "|" + c.Scope
}

// entraCertToken returns a cached token or mints one. Concurrent callers for
// the same key share a single in-flight mint.
func (cr *CredentialResolver) entraCertToken(ctx context.Context, c *Credential) (string, error) {
	key := certCacheKey(c)
	cr.mu.Lock()
	if t, ok := cr.cache[key]; ok && cr.now().Before(t.refreshAt) {
		cr.mu.Unlock()
		return t.value, nil
	}
	if f, ok := cr.inflight[key]; ok {
		cr.mu.Unlock()
		select {
		case <-f.done:
			return f.value, f.err
		case <-ctx.Done():
			return "", fmt.Errorf("%w: %q: %v", errCredential, c.ID, ctx.Err())
		}
	}
	f := &tokenFlight{done: make(chan struct{})}
	cr.inflight[key] = f
	cr.mu.Unlock()

	// Detached from the first caller's context: its cancellation must not
	// fail the callers sharing this flight.
	mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	value, refreshAt, err := cr.mintWithCertificate(mintCtx, c)
	cancel()

	cr.mu.Lock()
	if err == nil {
		cr.cache[key] = cachedToken{value: value, refreshAt: refreshAt}
	}
	delete(cr.inflight, key)
	cr.mu.Unlock()
	f.value, f.err = value, err
	close(f.done)
	return value, err
}

func (cr *CredentialResolver) mintWithCertificate(ctx context.Context, c *Credential) (string, time.Time, error) {
	id, err := loadAssertionIdentity(cr.identityDir, c.KeyFile, c.CertFile)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %q: %v", errCredential, c.ID, err)
	}
	endpoint := strings.TrimRight(cr.entraAuthority, "/") + "/" + url.PathEscape(c.Tenant) + "/oauth2/v2.0/token"
	assertion, err := clientAssertion(id, c.ClientID, endpoint, cr.now())
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %q: sign assertion: %v", errCredential, c.ID, err)
	}
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {c.ClientID},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
		"scope":                 {c.Scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %q: %v", errCredential, c.ID, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cr.client.Do(req)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("%w: %q: token endpoint unreachable", errCredential, c.ID)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		ErrorCodes  []int  `json:"error_codes"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tok); err != nil || resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		// error + error_codes (e.g. AADSTS700027 bad assertion) are safe to
		// log; error_description can echo request detail, so it is dropped.
		return "", time.Time{}, fmt.Errorf("%w: %q: token endpoint %s %s %v", errCredential, c.ID, resp.Status, tok.Error, tok.ErrorCodes)
	}
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	refreshAt := cr.now().Add(ttl - 5*time.Minute)
	if ttl <= 5*time.Minute {
		refreshAt = cr.now().Add(ttl / 2)
	}
	return tok.AccessToken, refreshAt, nil
}
