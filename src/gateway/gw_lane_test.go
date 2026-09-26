package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// End-to-end tests for the subscription lane: the production mux (dev-mode
// auth), a recording fake upstream, and a canary secret that must reach the
// upstream and nowhere else.

const canary = "CANARY-upstream-secret-7f3a"

type upstreamHit struct {
	Method, Path, Query string
	Header              http.Header
	Body                string
}

type fakeUpstream struct {
	*httptest.Server
	mu   sync.Mutex
	hits []upstreamHit
}

func newFakeUpstream(t *testing.T) *fakeUpstream {
	f := &fakeUpstream{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, upstreamHit{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone(), string(b)})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "path": r.URL.Path})
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeUpstream) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func (f *fakeUpstream) last() upstreamHit {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits[len(f.hits)-1]
}

type gw struct {
	t        *testing.T
	srv      *httptest.Server
	lane     *Lane
	dataDir  string
	passthru *atomic.Int32
}

func testSealer(t *testing.T) *Sealer {
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	t.Setenv("GATEWAY_SEAL_KEY", hex.EncodeToString(k))
	s, err := NewSealerFromEnv()
	if err != nil || s == nil {
		t.Fatalf("sealer: %v", err)
	}
	return s
}

func newTestGateway(t *testing.T, mutate func(*laneConfig)) *gw {
	t.Helper()
	dir := t.TempDir()
	cfg := laneConfig{dataDir: dir, sealer: testSealer(t)}
	if mutate != nil {
		mutate(&cfg)
	}
	mux := http.NewServeMux()
	lane, err := mountSubscriptionLane(mux, DevModeMiddleware(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	var passthru atomic.Int32
	mux.Handle("/", gatewayKeyGuard(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		passthru.Add(1)
		w.WriteHeader(299)
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	g := &gw{t: t, srv: srv, lane: lane, dataDir: dir, passthru: &passthru}
	g.must(201, "POST", "/api/v1/owners", map[string]any{"id": "test"})
	return g
}

// do sends a request and returns status + body.
func (g *gw) do(method, path string, body any, hdr map[string]string) (int, string) {
	g.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, g.srv.URL+path, rd)
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		g.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (g *gw) must(want int, method, path string, body any) string {
	g.t.Helper()
	code, out := g.do(method, path, body, nil)
	if code != want {
		g.t.Fatalf("%s %s: got %d want %d: %s", method, path, code, want, out)
	}
	return out
}

func (g *gw) key(k string) map[string]string { return map[string]string{"Api-Key": k} }

// provision creates credential → api → bundle → principal → subscription and
// returns the primary + secondary keys.
func (g *gw) provision(upstream string) (string, string) {
	g.must(201, "POST", "/api/v1/owners/test/credentials", map[string]any{
		"id": "speech-key", "source": "sealed", "value": canary, "inject": map[string]any{"header": "api-key"}})
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{
		"id": "speech", "name": "Speech", "upstream": upstream + "/openai/deployments/whisper",
		"credential": "speech-key",
		"operations": []map[string]any{
			{"id": "transcribe", "method": "POST", "path": "/audio/transcriptions", "summary": "Transcribe audio"},
			{"id": "get_item", "method": "GET", "path": "/items/{id}", "summary": "Get an item"},
		}})
	g.must(201, "POST", "/api/v1/owners/test/bundles", map[string]any{"id": "speech-basic", "apis": []string{"speech"}})
	g.must(201, "POST", "/api/v1/owners/test/principals", map[string]any{"id": "kim", "sub": "kim-sub", "email": "kim@example.com"})
	var issued issuedKeys
	out := g.must(201, "POST", "/api/v1/owners/test/subscriptions", map[string]any{"bundle": "speech-basic", "principal": "kim"})
	if err := json.Unmarshal([]byte(out), &issued); err != nil || issued.PrimaryKey == "" || issued.SecondaryKey == "" {
		g.t.Fatalf("subscription keys missing: %s", out)
	}
	if issued.Subscription.ID != "kim-speech-basic" {
		g.t.Fatalf("subscription id = %q", issued.Subscription.ID)
	}
	return issued.PrimaryKey, issued.SecondaryKey
}

// assertNoCanary scans every file the gateway wrote plus the given texts.
func (g *gw) assertNoCanary(texts ...string) {
	g.t.Helper()
	for _, s := range texts {
		if strings.Contains(s, canary) {
			g.t.Fatalf("canary secret leaked into response: %s", s)
		}
	}
	_ = filepath.Walk(g.dataDir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			b, _ := os.ReadFile(p)
			if bytes.Contains(b, []byte(canary)) {
				g.t.Fatalf("canary secret stored in plaintext: %s", p)
			}
		}
		return nil
	})
}

func TestLaneHTTPSwapsCredential(t *testing.T) {
	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)
	primary, secondary := g.provision(up.URL)

	code, out := g.do("POST", "/apis/test/speech/audio/transcriptions?api-version=2024-06-01", `{"audio":"x"}`, g.key(primary))
	if code != 200 {
		t.Fatalf("call: %d %s", code, out)
	}
	hit := up.last()
	if hit.Path != "/openai/deployments/whisper/audio/transcriptions" || hit.Query != "api-version=2024-06-01" {
		t.Fatalf("upstream got %s ?%s", hit.Path, hit.Query)
	}
	if hit.Header.Get("api-key") != canary {
		t.Fatalf("credential not injected: api-key=%q", hit.Header.Get("api-key"))
	}
	for k, vs := range hit.Header {
		for _, v := range vs {
			if strings.Contains(v, "gwk_") {
				t.Fatalf("subscription key reached upstream in %s", k)
			}
		}
	}
	if hit.Body != `{"audio":"x"}` {
		t.Fatalf("body = %q", hit.Body)
	}

	// Bearer and the secondary slot work too.
	if code, _ := g.do("GET", "/apis/test/speech/items/42", nil, map[string]string{"Authorization": "Bearer " + secondary}); code != 200 {
		t.Fatalf("secondary via bearer: %d", code)
	}
	if up.last().Header.Get("Authorization") != "" {
		t.Fatal("caller Authorization forwarded upstream")
	}

	// Undeclared operation: local 404, never forwarded.
	before := up.count()
	if code, _ := g.do("GET", "/apis/test/speech/admin/secrets", nil, g.key(primary)); code != 404 {
		t.Fatalf("undeclared op: %d", code)
	}
	if code, _ := g.do("DELETE", "/apis/test/speech/items/42", nil, g.key(primary)); code != 404 {
		t.Fatalf("undeclared method: %d", code)
	}
	if up.count() != before {
		t.Fatal("undeclared operation was forwarded upstream")
	}

	// Bad, missing and forged keys.
	if code, _ := g.do("GET", "/apis/test/speech/items/1", nil, nil); code != 401 {
		t.Fatalf("no key: %d", code)
	}
	if code, _ := g.do("GET", "/apis/test/speech/items/1", nil, g.key(primary+"x")); code != 401 {
		t.Fatalf("tampered key: %d", code)
	}
	if code, _ := g.do("GET", "/apis/test/nope/items/1", nil, g.key("gwk_kim-speech-basic_forged")); code != 401 {
		t.Fatalf("unknown api + bad key should look like a bad key: %d", code)
	}
	if code, _ := g.do("GET", "/apis/test/nope/items/1", nil, g.key(primary)); code != 404 {
		t.Fatalf("unknown api + good key: %d", code)
	}

	g.assertNoCanary(out)
	usage := g.must(200, "GET", "/api/v1/owners/test/usage?records=1", nil)
	if !strings.Contains(usage, `"calls": 2`) || !strings.Contains(usage, `"operation": "transcribe"`) {
		t.Fatalf("usage: %s", usage)
	}
	g.assertNoCanary(usage)
}

func TestLaneSubscriptionLifecycle(t *testing.T) {
	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)
	primary, _ := g.provision(up.URL)
	call := func() int { c, _ := g.do("GET", "/apis/test/speech/items/1", nil, g.key(primary)); return c }

	g.must(200, "POST", "/api/v1/owners/test/subscriptions/kim-speech-basic/suspend", nil)
	if c := call(); c != 403 {
		t.Fatalf("suspended: %d", c)
	}
	g.must(200, "POST", "/api/v1/owners/test/subscriptions/kim-speech-basic/reactivate", nil)
	if c := call(); c != 200 {
		t.Fatalf("reactivated: %d", c)
	}

	// Regenerating primary kills the old primary.
	var re issuedKeys
	_ = json.Unmarshal([]byte(g.must(200, "POST", "/api/v1/owners/test/subscriptions/kim-speech-basic/regenerate", map[string]string{"slot": "primary"})), &re)
	if c, _ := g.do("GET", "/apis/test/speech/items/1", nil, g.key(primary)); c != 401 {
		t.Fatalf("old primary after regenerate: %d", c)
	}
	primary = re.PrimaryKey
	if c := call(); c != 200 {
		t.Fatalf("new primary: %d", c)
	}

	// Removing the API from the bundle revokes coverage.
	g.must(200, "DELETE", "/api/v1/owners/test/bundles/speech-basic/apis/speech", nil)
	if c := call(); c != 403 {
		t.Fatalf("api removed from bundle: %d", c)
	}
	g.must(200, "PUT", "/api/v1/owners/test/bundles/speech-basic/apis/speech", nil)

	g.must(200, "POST", "/api/v1/owners/test/principals/kim/disable", nil)
	if c := call(); c != 403 {
		t.Fatalf("disabled principal: %d", c)
	}
	g.must(200, "POST", "/api/v1/owners/test/principals/kim/enable", nil)

	g.must(200, "POST", "/api/v1/owners/test/subscriptions/kim-speech-basic/cancel", nil)
	if c := call(); c != 403 {
		t.Fatalf("cancelled: %d", c)
	}
	g.must(409, "POST", "/api/v1/owners/test/subscriptions/kim-speech-basic/reactivate", nil)

	// Delete guards: referenced things can't be removed.
	g.must(409, "DELETE", "/api/v1/owners/test/apis/speech", nil)
	g.must(409, "DELETE", "/api/v1/owners/test/credentials/speech-key", nil)
}

func TestLaneMCPOverAPI(t *testing.T) {
	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)
	primary, _ := g.provision(up.URL)
	g.must(201, "POST", "/api/v1/owners/test/mcp-servers", map[string]any{
		"id": "speech-tools", "api": "speech", "operations": []string{"*"}, "description": "Speech tools"})

	rpc := func(method string, params any) (int, map[string]any) {
		code, out := g.do("POST", "/mcp/test/speech-tools", map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params}, g.key(primary))
		var m map[string]any
		_ = json.Unmarshal([]byte(out), &m)
		return code, m
	}

	// The subscription's bundle doesn't include the server yet.
	if c, _ := rpc("initialize", map[string]any{}); c != 403 {
		t.Fatalf("mcp not in bundle: %d", c)
	}
	g.must(200, "PUT", "/api/v1/owners/test/bundles/speech-basic/mcp-servers/speech-tools", nil)

	if c, m := rpc("initialize", map[string]any{"protocolVersion": "2025-06-18"}); c != 200 || m["result"] == nil {
		t.Fatalf("initialize: %d %v", c, m)
	}
	_, m := rpc("tools/list", nil)
	tools := m["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools/list: %v", tools)
	}

	_, m = rpc("tools/call", map[string]any{"name": "get_item", "arguments": map[string]any{"id": "42", "verbose": true}})
	res := m["result"].(map[string]any)
	if res["isError"] != false {
		t.Fatalf("tools/call: %v", m)
	}
	hit := up.last()
	if hit.Path != "/openai/deployments/whisper/items/42" || hit.Query != "verbose=true" || hit.Header.Get("api-key") != canary {
		t.Fatalf("upstream got %s ?%s api-key=%q", hit.Path, hit.Query, hit.Header.Get("api-key"))
	}

	_, m = rpc("tools/call", map[string]any{"name": "transcribe", "arguments": map[string]any{"body": map[string]any{"audio": "x"}}})
	if up.last().Body != `{"audio":"x"}` {
		t.Fatalf("tool body = %q (%v)", up.last().Body, m)
	}

	if _, m = rpc("tools/call", map[string]any{"name": "nope"}); m["error"] == nil {
		t.Fatalf("unknown tool should be a JSON-RPC error: %v", m)
	}
	if c, _ := g.do("POST", "/mcp/test/speech-tools", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, g.key(primary)); c != 202 {
		t.Fatalf("notification: %d", c)
	}
	if c, _ := g.do("GET", "/mcp/test/speech-tools", nil, g.key(primary)); c != 405 {
		t.Fatalf("GET /mcp: %d", c)
	}
	g.assertNoCanary()
	usage := g.must(200, "GET", "/api/v1/owners/test/usage?records=1", nil)
	if !strings.Contains(usage, `"tool": "get_item"`) {
		t.Fatalf("tool usage not recorded: %s", usage)
	}
}

func TestLaneRemoteMCPAllowList(t *testing.T) {
	var got atomic.Value
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.Store(r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"hi"}]}}`)
	}))
	defer remote.Close()
	t.Setenv("REMOTE_MCP_TOKEN", canary)

	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)
	primary, _ := g.provision(up.URL)
	g.must(201, "POST", "/api/v1/owners/test/credentials", map[string]any{"id": "remote-tok", "source": "env", "var": "REMOTE_MCP_TOKEN", "inject": map[string]any{"bearer": true}})
	g.must(201, "POST", "/api/v1/owners/test/mcp-servers", map[string]any{
		"id": "remote", "source": "remote", "remoteUrl": remote.URL + "/mcp", "credential": "remote-tok", "allowTools": []string{"echo"}})
	g.must(200, "PUT", "/api/v1/owners/test/bundles/speech-basic/mcp-servers/remote", nil)

	_, out := g.do("POST", "/mcp/test/remote", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "echo"}}, g.key(primary))
	if got.Load() != "Bearer "+canary || !strings.Contains(out, "hi") {
		t.Fatalf("remote call: auth=%v out=%s", got.Load(), out)
	}
	_, out = g.do("POST", "/mcp/test/remote", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "rm_rf"}}, g.key(primary))
	if !strings.Contains(out, "tool not allowed") {
		t.Fatalf("allow-list not enforced: %s", out)
	}
}

func TestLaneEntraCredential(t *testing.T) {
	var mints atomic.Int32
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/tenant-1/oauth2/v2.0/token" || r.Form.Get("client_secret") != canary ||
			r.Form.Get("scope") != "https://cognitiveservices.azure.com/.default" {
			w.WriteHeader(400)
			io.WriteString(w, `{"error":"invalid_request"}`)
			return
		}
		mints.Add(1)
		io.WriteString(w, `{"access_token":"entra-token-1","expires_in":3600}`)
	}))
	defer idp.Close()
	t.Setenv("ENTRA_AUTHORITY_HOST", idp.URL)

	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)
	g.must(201, "POST", "/api/v1/owners/test/credentials", map[string]any{"id": "sp-secret", "source": "sealed", "value": canary})
	g.must(201, "POST", "/api/v1/owners/test/credentials", map[string]any{"id": "foundry", "source": "entra", "tenant": "tenant-1",
		"clientId": "app-1", "clientSecret": "sp-secret", "scope": "https://cognitiveservices.azure.com/.default"})
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "foundry", "upstream": up.URL, "credential": "foundry",
		"operations": []map[string]any{{"id": "transcribe", "method": "POST", "path": "/transcribe"}}})
	g.must(201, "POST", "/api/v1/owners/test/bundles", map[string]any{"id": "b", "apis": []string{"foundry"}})
	g.must(201, "POST", "/api/v1/owners/test/principals", map[string]any{"id": "bot", "kind": "service"})
	var issued issuedKeys
	_ = json.Unmarshal([]byte(g.must(201, "POST", "/api/v1/owners/test/subscriptions", map[string]any{"bundle": "b", "principal": "bot"})), &issued)

	for i := 0; i < 3; i++ {
		if c, out := g.do("POST", "/apis/test/foundry/transcribe", "{}", g.key(issued.PrimaryKey)); c != 200 {
			t.Fatalf("call %d: %d %s", i, c, out)
		}
	}
	if up.last().Header.Get("Authorization") != "Bearer entra-token-1" {
		t.Fatalf("entra token not injected: %q", up.last().Header.Get("Authorization"))
	}
	if mints.Load() != 1 {
		t.Fatalf("token minted %d times, want 1 (cached)", mints.Load())
	}
	cred := g.must(200, "GET", "/api/v1/owners/test/credentials/sp-secret", nil)
	g.assertNoCanary(cred, g.must(200, "GET", "/api/v1/owners/test/credentials", nil))
}

func TestCatchAllRefusesGatewayKeys(t *testing.T) {
	g := newTestGateway(t, nil)
	if c, _ := g.do("POST", "/v1/messages", "{}", map[string]string{"x-api-key": "gwk_a_b"}); c != 401 {
		t.Fatalf("gwk_ key on passthrough: %d", c)
	}
	if c, _ := g.do("POST", "/v1/messages", "{}", map[string]string{"Authorization": "Bearer gwk_a_b"}); c != 401 {
		t.Fatalf("gwk_ bearer on passthrough: %d", c)
	}
	if g.passthru.Load() != 0 {
		t.Fatal("gwk_ key reached the passthrough lane")
	}
	if c, _ := g.do("POST", "/v1/messages", "{}", map[string]string{"x-api-key": "sk-ant-real"}); c != 299 {
		t.Fatalf("ordinary passthrough: %d", c)
	}
	// Unknown paths under the lane's namespaces stay local.
	for _, p := range []string{"/apis/", "/apis/test/", "/mcp/", "/mcp/test/x/y", "/api/v1/nope", "/api/v1/owners/test/nope", "/.well-known/oauth-protected-resource", "/connect/", "/connect/v1/control", "/connect/v1/nope"} {
		before := g.passthru.Load()
		g.do("GET", p, nil, nil)
		if g.passthru.Load() != before {
			t.Fatalf("%s fell through to the passthrough lane", p)
		}
	}
}

func TestLaneMCPOAuth(t *testing.T) {
	up := newFakeUpstream(t)
	var resource string
	g := newTestGateway(t, func(c *laneConfig) {
		c.oauthIssuer = "https://login.example.com/realms/agentics"
		c.verify = func(_ context.Context, raw string) (string, []string, error) {
			switch raw {
			case "good":
				return "kim-sub", []string{resource}, nil
			case "wrong-aud":
				return "kim-sub", []string{"https://elsewhere/mcp/x"}, nil
			case "stranger":
				return "who", []string{resource}, nil
			}
			return "", nil, errors.New("bad token")
		}
	})
	resource = g.srv.URL + "/mcp/test/speech-tools"
	g.provision(up.URL)
	g.must(201, "POST", "/api/v1/owners/test/mcp-servers", map[string]any{"id": "speech-tools", "api": "speech", "operations": []string{"get_item"}})
	g.must(200, "PUT", "/api/v1/owners/test/bundles/speech-basic/mcp-servers/speech-tools", nil)

	meta := g.must(200, "GET", "/.well-known/oauth-protected-resource/mcp/test/speech-tools", nil)
	if !strings.Contains(meta, resource) || !strings.Contains(meta, "realms/agentics") {
		t.Fatalf("metadata: %s", meta)
	}

	req, _ := http.NewRequest("POST", resource, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource/mcp/test/speech-tools") {
		t.Fatalf("challenge: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}

	ping := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	for tok, want := range map[string]int{"good": 200, "wrong-aud": 401, "stranger": 403, "garbage": 401} {
		if c, out := g.do("POST", "/mcp/test/speech-tools", ping, map[string]string{"Authorization": "Bearer " + tok}); c != want {
			t.Fatalf("token %s: %d want %d (%s)", tok, c, want, out)
		}
	}
}

func TestManagementValidation(t *testing.T) {
	g := newTestGateway(t, nil)
	g.must(400, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "Bad ID", "upstream": "http://x"})
	g.must(400, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "a", "upstream": "ftp://x"})
	g.must(400, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "a", "upstream": "http://x", "credential": "missing"})
	g.must(400, "POST", "/api/v1/owners/test/credentials", map[string]any{"id": "c", "source": "env", "var": "X", "inject": map[string]any{"header": "a", "bearer": true}})
	g.must(400, "POST", "/api/v1/owners/test/credentials", map[string]any{"id": "c", "source": "env", "value": "nope", "var": "X", "inject": map[string]any{"bearer": true}})
	g.must(201, "POST", "/api/v1/owners/test/credentials", map[string]any{"id": "no-target", "source": "env", "var": "X"})
	g.must(400, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "a", "upstream": "http://x", "credential": "no-target"})
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "a", "upstream": "http://x"})
	g.must(409, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "a", "upstream": "http://x"})
	g.must(400, "POST", "/api/v1/owners/test/mcp-servers", map[string]any{"id": "m", "api": "a", "operations": []string{"ghost"}})
	g.must(400, "POST", "/api/v1/owners/test/subscriptions", map[string]any{"bundle": "none", "principal": "none"})

	openapi := `{"openapi":"3.0.1","paths":{"/items/{id}":{"get":{"operationId":"getItem","summary":"Get",
	  "parameters":[{"name":"id","in":"path","required":true,"schema":{"type":"integer"}},{"name":"q","in":"query","schema":{"type":"string"}}]}},
	  "/items":{"post":{"requestBody":{"required":true,"content":{"application/json":{"schema":{"type":"object"}}}}}}}}`
	out := g.must(200, "POST", "/api/v1/owners/test/apis/a/openapi", openapi)
	if !strings.Contains(out, `"id": "getItem"`) || !strings.Contains(out, `"id": "post_items"`) {
		t.Fatalf("openapi import: %s", out)
	}

	// Self-service: dev-mode caller is sub "dev".
	g.must(201, "POST", "/api/v1/owners/test/principals", map[string]any{"id": "me", "sub": "dev"})
	g.must(201, "POST", "/api/v1/owners/test/bundles", map[string]any{"id": "b", "apis": []string{"a"}})
	g.must(201, "POST", "/api/v1/owners/test/subscriptions", map[string]any{"bundle": "b", "principal": "me"})
	if mine := g.must(200, "GET", "/api/v1/me/subscriptions", nil); !strings.Contains(mine, `"me-b"`) {
		t.Fatalf("me/subscriptions: %s", mine)
	}
	if cat := g.must(200, "GET", "/api/v1/owners/test/catalog", nil); !strings.Contains(cat, `"/apis/test/a"`) {
		t.Fatalf("catalog: %s", cat)
	}
	for _, f := range []string{"primaryHash", "secondaryHash"} {
		if s := g.must(200, "GET", "/api/v1/owners/test/subscriptions/me-b", nil); strings.Contains(s, f) {
			t.Fatalf("subscription view exposes %s", f)
		}
	}
}

func TestSealerBindsCredentialID(t *testing.T) {
	s := testSealer(t)
	blob, err := s.Seal("secret", "a")
	if err != nil {
		t.Fatal(err)
	}
	if v, err := s.Open(blob, "a"); err != nil || v != "secret" {
		t.Fatalf("round trip: %q %v", v, err)
	}
	if _, err := s.Open(blob, "b"); err == nil {
		t.Fatal("sealed blob opened under another credential id")
	}
}

func TestParseSubscriptionKeyWithUnderscoreSecret(t *testing.T) {
	for i := 0; i < 200; i++ {
		k, _ := newSubscriptionKey("kim-speech")
		if id, ok := parseSubscriptionKey(k); !ok || id != "kim-speech" {
			t.Fatalf("parse %q = %q %v", k, id, ok)
		}
	}
}
