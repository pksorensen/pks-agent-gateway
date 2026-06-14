// Package client provides an HTTP client that automatically attaches Bearer
// tokens and handles 401 by attempting a refresh before giving up.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/auth"
	"github.com/pksorensen/pks-agent-gateway/src/cli/internal/config"
)

// Client is an authenticated HTTP client for the gateway API.
type Client struct {
	BaseURL string
	cfg     *config.Config
}

// New creates a Client for the given base URL. cfg is used to resolve OIDC
// credentials when token refresh is needed.
func New(baseURL string, cfg *config.Config) *Client {
	return &Client{BaseURL: baseURL, cfg: cfg}
}

// Do performs an HTTP request, attaching the stored Bearer token. On a 401 it
// attempts one refresh; if that also fails it prints a human-readable message
// and returns an error.
func (c *Client) Do(ctx context.Context, method, path string, body interface{}) (*http.Response, error) {
	token, err := c.accessToken(ctx)
	if err != nil {
		return nil, err
	}

	resp, err := c.doOnce(ctx, method, path, body, token)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		// Try to refresh.
		newToken, refreshErr := c.refreshToken(ctx)
		if refreshErr != nil {
			fmt.Fprintln(os.Stderr, "Session expired — run `gateway-cli login`")
			return nil, errors.New("unauthenticated")
		}
		return c.doOnce(ctx, method, path, body, newToken)
	}
	return resp, nil
}

func (c *Client) doOnce(ctx context.Context, method, path string, body interface{}, token string) (*http.Response, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return http.DefaultClient.Do(req)
}

// GetJSON performs a GET and decodes the JSON response into out.
func (c *Client) GetJSON(ctx context.Context, path string, out interface{}) error {
	resp, err := c.Do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET %s: %s — %s", path, resp.Status, string(b))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// PostJSON performs a POST with a JSON body and decodes the JSON response into out.
func (c *Client) PostJSON(ctx context.Context, path string, body interface{}, out interface{}) error {
	resp, err := c.Do(ctx, http.MethodPost, path, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("POST %s: %s — %s", path, resp.Status, string(b))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

// accessToken returns the stored access token (without refresh attempt).
func (c *Client) accessToken(_ context.Context) (string, error) {
	cred, err := auth.LoadCred()
	if err != nil {
		return "", err
	}
	if cred == nil || cred.RefreshToken == "" {
		return "", nil // unauthenticated; let the 401 handler decide
	}
	// We don't persist the access token — always use refresh to get a fresh one.
	// This is simpler than tracking expiry and avoids stale-token edge cases.
	return "", nil
}

// refreshToken uses the stored refresh token to obtain a new access token,
// saves the rotated refresh token, and returns the new access token.
func (c *Client) refreshToken(ctx context.Context) (string, error) {
	cred, err := auth.LoadCred()
	if err != nil || cred == nil || cred.RefreshToken == "" {
		return "", errors.New("no refresh token stored")
	}
	if c.cfg.OIDCIssuer == "" || c.cfg.OIDCClientID == "" {
		return "", errors.New("OIDC issuer/clientId not configured — run `gateway-cli login`")
	}
	tokens, err := auth.Refresh(ctx, c.cfg.OIDCIssuer, c.cfg.OIDCClientID, cred.RefreshToken)
	if err != nil {
		return "", err
	}
	// Persist the (possibly rotated) refresh token and updated identity.
	cred.RefreshToken = tokens.Refresh
	if tokens.Sub != "" {
		cred.Sub = tokens.Sub
	}
	if tokens.Email != "" {
		cred.Email = tokens.Email
	}
	_ = auth.SaveCred(cred)
	return tokens.Access, nil
}

// BearerToken returns a fresh access token for use outside the Do helper
// (e.g. when constructing WebSocket URLs).
func (c *Client) BearerToken(ctx context.Context) (string, error) {
	return c.refreshToken(ctx)
}
