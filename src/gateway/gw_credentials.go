package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Credentials (ADR 0002): what the gateway injects upstream in place of the
// subscriber's key. The management API never returns a value.

type CredentialSourceKind string

const (
	CredEnv    CredentialSourceKind = "env"
	CredSealed CredentialSourceKind = "sealed"
	CredVault  CredentialSourceKind = "vault"
	CredEntra  CredentialSourceKind = "entra"
)

type Credential struct {
	ID     string               `json:"id"`
	Source CredentialSourceKind `json:"source"`
	Inject Injection            `json:"inject"`

	Var    string `json:"var,omitempty"`    // env
	Sealed string `json:"sealed,omitempty"` // sealed: base64(nonce||ciphertext); never serialised to clients
	Item   string `json:"item,omitempty"`   // vault: <owner>/<item>

	// entra: client-credentials token for Scope. The client secret is itself a
	// credential (env or sealed) referenced by id.
	Tenant       string `json:"tenant,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"` // credential id
	Scope        string `json:"scope,omitempty"`

	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Injection says where the resolved value goes on the upstream request.
// Exactly one of Header / Query / Bearer.
type Injection struct {
	Header string `json:"header,omitempty"`
	Query  string `json:"query,omitempty"`
	Bearer bool   `json:"bearer,omitempty"`
}

func (i Injection) valid() bool {
	n := 0
	if i.Header != "" {
		n++
	}
	if i.Query != "" {
		n++
	}
	if i.Bearer {
		n++
	}
	return n == 1
}

// apply writes value onto the outbound request.
func (i Injection) apply(r *http.Request, value string) {
	switch {
	case i.Bearer:
		r.Header.Set("Authorization", "Bearer "+value)
	case i.Header != "":
		r.Header.Set(i.Header, value)
	case i.Query != "":
		q := r.URL.Query()
		q.Set(i.Query, value)
		r.URL.RawQuery = q.Encode()
	}
}

// credentialView is what the management API returns: metadata only.
type credentialView struct {
	ID        string               `json:"id"`
	Source    CredentialSourceKind `json:"source"`
	Inject    Injection            `json:"inject"`
	Var       string               `json:"var,omitempty"`
	Item      string               `json:"item,omitempty"`
	Tenant    string               `json:"tenant,omitempty"`
	ClientID  string               `json:"clientId,omitempty"`
	Scope     string               `json:"scope,omitempty"`
	HasValue  bool                 `json:"hasValue"`
	CreatedAt time.Time            `json:"createdAt"`
	UpdatedAt time.Time            `json:"updatedAt"`
}

func (c *Credential) view() credentialView {
	v := credentialView{ID: c.ID, Source: c.Source, Inject: c.Inject, Var: c.Var, Item: c.Item,
		Tenant: c.Tenant, ClientID: c.ClientID, Scope: c.Scope, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	switch c.Source {
	case CredSealed:
		v.HasValue = c.Sealed != ""
	case CredEnv:
		v.HasValue = os.Getenv(c.Var) != ""
	default:
		v.HasValue = true
	}
	return v
}

// errCredential is returned (wrapped) for every resolution failure; the lane
// maps it to a generic 502 and logs the cause without any value.
var errCredential = errors.New("credential unavailable")

// Sealer encrypts sealed credential values under GATEWAY_SEAL_KEY.
type Sealer struct{ aead cipher.AEAD }

// NewSealerFromEnv reads GATEWAY_SEAL_KEY (32 bytes, hex or base64). Returns
// nil (no error) when unset — sealing is then refused at write time.
func NewSealerFromEnv() (*Sealer, error) {
	raw := strings.TrimSpace(os.Getenv("GATEWAY_SEAL_KEY"))
	if raw == "" {
		return nil, nil
	}
	key, err := hex.DecodeString(raw)
	if err != nil || len(key) != 32 {
		key, err = base64.StdEncoding.DecodeString(raw)
	}
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("GATEWAY_SEAL_KEY must be 32 bytes, hex or base64")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

func (s *Sealer) Seal(plain, credID string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	// The credential id is additional data: a sealed blob can't be moved onto
	// another credential.
	ct := s.aead.Seal(nonce, nonce, []byte(plain), []byte(credID))
	return base64.StdEncoding.EncodeToString(ct), nil
}

func (s *Sealer) Open(sealed, credID string) (string, error) {
	b, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(b) < s.aead.NonceSize() {
		return "", fmt.Errorf("sealed value corrupt")
	}
	n := s.aead.NonceSize()
	pt, err := s.aead.Open(nil, b[:n], b[n:], []byte(credID))
	if err != nil {
		return "", fmt.Errorf("sealed value does not open (wrong GATEWAY_SEAL_KEY?)")
	}
	return string(pt), nil
}

// CredentialResolver resolves credentials to values, caching minted tokens.
type CredentialResolver struct {
	sealer *Sealer
	client *http.Client
	// entraAuthority is the token-endpoint host root; overridable for tests.
	entraAuthority string

	mu    sync.Mutex
	cache map[string]cachedToken
}

type cachedToken struct {
	value     string
	refreshAt time.Time
}

func NewCredentialResolver(sealer *Sealer) *CredentialResolver {
	return &CredentialResolver{
		sealer:         sealer,
		client:         &http.Client{Timeout: 30 * time.Second},
		entraAuthority: env("ENTRA_AUTHORITY_HOST", "https://login.microsoftonline.com"),
		cache:          map[string]cachedToken{},
	}
}

// Resolve returns the value for owner st's credential id. depth guards
// entra→secret chains. Credentials never resolve across owners.
func (cr *CredentialResolver) Resolve(ctx context.Context, st *GatewayStore, id string) (string, error) {
	return cr.resolve(ctx, st, id, 0)
}

// cacheKey scopes cached tokens by owner: two owners may both name a
// credential "foundry".
func cacheKey(st *GatewayStore, id string) string { return st.owner + "/" + id }

func (cr *CredentialResolver) resolve(ctx context.Context, st *GatewayStore, id string, depth int) (string, error) {
	if depth > 2 {
		return "", fmt.Errorf("%w: credential chain too deep at %q", errCredential, id)
	}
	c, err := st.GetCredential(id)
	if err != nil {
		return "", fmt.Errorf("%w: %q: %v", errCredential, id, err)
	}
	switch c.Source {
	case CredEnv:
		v := os.Getenv(c.Var)
		if v == "" {
			return "", fmt.Errorf("%w: %q: env var %s is empty", errCredential, id, c.Var)
		}
		return v, nil
	case CredSealed:
		if cr.sealer == nil {
			return "", fmt.Errorf("%w: %q: GATEWAY_SEAL_KEY not set", errCredential, id)
		}
		v, err := cr.sealer.Open(c.Sealed, c.ID)
		if err != nil {
			return "", fmt.Errorf("%w: %q: %v", errCredential, id, err)
		}
		return v, nil
	case CredEntra:
		return cr.entraToken(ctx, st, c, depth)
	case CredVault:
		// ADR 0002: agent-grant release with a <1h TTL. Not wired yet.
		return "", fmt.Errorf("%w: %q: vault source not implemented yet", errCredential, id)
	}
	return "", fmt.Errorf("%w: %q: unknown source %q", errCredential, id, c.Source)
}

func (cr *CredentialResolver) entraToken(ctx context.Context, st *GatewayStore, c *Credential, depth int) (string, error) {
	key := cacheKey(st, c.ID)
	cr.mu.Lock()
	if t, ok := cr.cache[key]; ok && time.Now().Before(t.refreshAt) {
		cr.mu.Unlock()
		return t.value, nil
	}
	cr.mu.Unlock()

	secret, err := cr.resolve(ctx, st, c.ClientSecret, depth+1)
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.ClientID},
		"client_secret": {secret},
		"scope":         {c.Scope},
	}
	endpoint := strings.TrimRight(cr.entraAuthority, "/") + "/" + url.PathEscape(c.Tenant) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%w: %q: %v", errCredential, c.ID, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := cr.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: %q: token endpoint: %v", errCredential, c.ID, err)
	}
	defer resp.Body.Close()
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil || resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		return "", fmt.Errorf("%w: %q: token endpoint %s %s", errCredential, c.ID, resp.Status, tok.Error)
	}
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	refreshAt := time.Now().Add(ttl - 5*time.Minute)
	if ttl <= 5*time.Minute {
		refreshAt = time.Now().Add(ttl / 2)
	}
	cr.mu.Lock()
	cr.cache[key] = cachedToken{value: tok.AccessToken, refreshAt: refreshAt}
	cr.mu.Unlock()
	return tok.AccessToken, nil
}

// Forget drops a cached token (after rotation).
func (cr *CredentialResolver) Forget(st *GatewayStore, id string) {
	cr.mu.Lock()
	delete(cr.cache, cacheKey(st, id))
	cr.mu.Unlock()
}
