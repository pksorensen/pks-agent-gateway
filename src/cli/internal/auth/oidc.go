// Package auth implements PKCE Authorization Code loopback flow.
// Ported from projects/pks-agent-share/src/agent-share/oidc.go.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const scopes = "openid profile email offline_access"

// stderr is a package-level writer so tests can redirect it.
var stderr = os.Stderr

// TokenError is a structured error from the OAuth token endpoint.
// Callers can check Code == "invalid_grant" to distinguish dead refresh tokens
// from transient network failures.
type TokenError struct {
	Status int
	Code   string
	Desc   string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("token endpoint %d: %s %s", e.Status, e.Code, e.Desc)
}

// Tokens is the result of a successful login or refresh.
type Tokens struct {
	Access  string
	Refresh string
	Sub     string
	Email   string
	Name    string
}

// discover fetches /.well-known/openid-configuration and returns the
// authorization and token endpoints.
func discover(ctx context.Context, iss string) (authEndpoint, tokenEndpoint string, err error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(iss, "/")+"/.well-known/openid-configuration", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", "", err
	}
	return doc.AuthorizationEndpoint, doc.TokenEndpoint, nil
}

// Login runs the Authorization Code + PKCE loopback flow: it prints the URL
// (and tries to open the browser), listens on a random loopback port for the
// callback, then exchanges the code for tokens.
func Login(ctx context.Context, issuer, clientID string) (*Tokens, error) {
	authEndpoint, tokenEndpoint, err := discover(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("discovery: %w", err)
	}

	verifier := randURL(64)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	state := randURL(24)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	redirect := fmt.Sprintf("http://127.0.0.1:%d/callback/", port)

	authURL := authEndpoint + "?" + url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {redirect},
		"scope":                 {scopes},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}.Encode()

	fmt.Fprintf(stderr, "\nSign in:\n  %s\n\n", authURL)
	tryOpen(authURL)

	codeCh := make(chan string, 1)
	errCh := make(chan error, 1)
	srv := &http.Server{}
	mux := http.NewServeMux()
	mux.HandleFunc("/callback/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != state {
			errCh <- errors.New("state mismatch")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body style='font-family:system-ui;background:#0a0a0a;color:#ededef;text-align:center;padding-top:80px'><h2>gateway-cli</h2><p>Logged in. You can close this tab.</p></body></html>")
		codeCh <- r.URL.Query().Get("code")
	})
	srv.Handler = mux
	go srv.Serve(ln) //nolint:errcheck
	defer srv.Close()

	var code string
	select {
	case code = <-codeCh:
	case err = <-errCh:
		return nil, err
	case <-time.After(5 * time.Minute):
		return nil, errors.New("timed out waiting for login")
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if code == "" {
		return nil, errors.New("no authorization code returned")
	}

	return exchange(ctx, tokenEndpoint, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirect},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	})
}

// Refresh swaps a refresh token for fresh tokens. If the IdP rotates refresh
// tokens, the new one is returned; otherwise the original is kept.
func Refresh(ctx context.Context, issuer, clientID, refreshToken string) (*Tokens, error) {
	_, tokenEndpoint, err := discover(ctx, issuer)
	if err != nil {
		return nil, err
	}
	t, err := exchange(ctx, tokenEndpoint, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {clientID},
	})
	if err != nil {
		return nil, err
	}
	if t.Refresh == "" {
		t.Refresh = refreshToken
	}
	return t, nil
}

func exchange(ctx context.Context, tokenEndpoint string, form url.Values) (*Tokens, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, tokenEndpoint, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		Error        string `json:"error"`
		ErrorDesc    string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 || body.AccessToken == "" {
		return nil, &TokenError{Status: resp.StatusCode, Code: body.Error, Desc: body.ErrorDesc}
	}
	sub, email, name := claims(body.AccessToken)
	return &Tokens{
		Access:  body.AccessToken,
		Refresh: body.RefreshToken,
		Sub:     sub,
		Email:   email,
		Name:    name,
	}, nil
}

// claims decodes the JWT payload without verification (the server verifies)
// solely to extract display identity fields.
func claims(jwt string) (sub, email, name string) {
	parts := strings.Split(jwt, ".")
	if len(parts) < 2 {
		return
	}
	b, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return
	}
	var c struct {
		Sub               string `json:"sub"`
		Email             string `json:"email"`
		Name              string `json:"name"`
		PreferredUsername string `json:"preferred_username"`
	}
	_ = json.Unmarshal(b, &c)
	name = c.Name
	if name == "" {
		name = c.PreferredUsername
	}
	return c.Sub, c.Email, name
}

func randURL(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func tryOpen(u string) {
	var cmd string
	var args []string
	switch runtime.GOOS {
	case "darwin":
		cmd, args = "open", []string{u}
	case "windows":
		cmd, args = "cmd", []string{"/c", "start", "", u}
	default:
		cmd, args = "xdg-open", []string{u}
	}
	_ = exec.Command(cmd, args...).Start()
}
