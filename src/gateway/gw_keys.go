package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
)

// Subscription keys (ADR 0003): gwk_<subscriptionId>_<secret>.
//
// The id segment makes lookup O(1); the secret is 32 random bytes. Only the
// sha256 of the whole key is stored, one per slot (primary/secondary), and
// comparison is constant-time.

const (
	gatewayKeyPrefix   = "gwk_"
	connectorKeyPrefix = "gwc_"
)

func newSubscriptionKey(subID string) (key, hash string) { return newKey(gatewayKeyPrefix, subID) }

// newKey mints <prefix><id>_<secret>; gwk_ for subscriptions, gwc_ for connectors.
func newKey(prefix, id string) (key, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	key = prefix + id + "_" + base64.RawURLEncoding.EncodeToString(b)
	return key, hashKey(key)
}

func hashKey(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

// parseSubscriptionKey extracts the subscription id from a gateway key.
func parseSubscriptionKey(key string) (subID string, ok bool) { return parseKey(gatewayKeyPrefix, key) }

func parseKey(prefix, key string) (id string, ok bool) {
	if !strings.HasPrefix(key, prefix) {
		return "", false
	}
	rest := key[len(prefix):]
	// Split on the FIRST underscore: ids never contain one (idPattern), but
	// the base64url secret can.
	i := strings.Index(rest, "_")
	if i <= 0 || i == len(rest)-1 {
		return "", false
	}
	return rest[:i], true
}

// keyMatches reports whether key hashes to either stored slot.
func keyMatches(sub *Subscription, key string) bool {
	return hashMatches(key, sub.PrimaryHash, sub.SecondaryHash)
}

func hashMatches(key string, slots ...string) bool {
	h := []byte(hashKey(key))
	m := 0
	for _, slot := range slots {
		m |= subtle.ConstantTimeCompare(h, []byte(slot))
	}
	return m == 1
}

// keyHeaders are every place a subscription key may be presented (FR-11).
// All of them are stripped before a request leaves the gateway.
var keyHeaders = []string{"Api-Key", "Ocp-Apim-Subscription-Key", "X-Api-Key"}

// presentedGatewayKey returns a gwk_ key from the request, if any.
func presentedGatewayKey(r *http.Request) (string, bool) {
	for _, h := range keyHeaders {
		if v := strings.TrimSpace(r.Header.Get(h)); strings.HasPrefix(v, gatewayKeyPrefix) {
			return v, true
		}
	}
	if tok, ok := bearerToken(r); ok && strings.HasPrefix(tok, gatewayKeyPrefix) {
		return tok, true
	}
	return "", false
}

// stripPresentedCredentials removes the caller's key and bearer so they never
// reach an upstream.
func stripPresentedCredentials(h http.Header) {
	for _, k := range keyHeaders {
		h.Del(k)
	}
	h.Del("Authorization")
	// The runner applies policy from X-Gateway-Context (ADR 0004); a
	// client-supplied one would be a forgery.
	h.Del(gatewayContextHeader)
}

// gatewayKeyGuard wraps the legacy catch-all: a gwk_ key there is a 401,
// never passthrough (ADR 0001 rule 1).
func gatewayKeyGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := presentedGatewayKey(r); ok || presentsConnectorToken(r) {
			jsonError(w, "gateway key used outside /apis/ or /mcp/", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// presentsConnectorToken reports a gwc_ connector token anywhere a key could go.
func presentsConnectorToken(r *http.Request) bool {
	for _, h := range keyHeaders {
		if strings.HasPrefix(strings.TrimSpace(r.Header.Get(h)), connectorKeyPrefix) {
			return true
		}
	}
	tok, ok := bearerToken(r)
	return ok && strings.HasPrefix(tok, connectorKeyPrefix)
}
