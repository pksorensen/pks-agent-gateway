package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/pksorensen/pks-agent-tunnel/src/protocol"
)

// --- Tenancy (ADR 0005) ---

// newTenancyServer mounts the management plane with claims taken from the
// X-Test-Sub header (no roles), so ownership — not GatewayAdmin — decides.
func newTenancyServer(t *testing.T) *httptest.Server {
	t.Helper()
	api := newGatewayAPI(NewGatewayRoot(t.TempDir()), NewCredentialResolver(nil), nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c := &UserClaims{Sub: r.Header.Get("X-Test-Sub")}
		api.ServeHTTP(w, r.WithContext(withClaims(r.Context(), c)))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func asSub(t *testing.T, srv *httptest.Server, sub, method, path string, body any) (int, string) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req, _ := http.NewRequest(method, srv.URL+path, rd)
	req.Header.Set("X-Test-Sub", sub)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTenancyOwnerIsolation(t *testing.T) {
	srv := newTenancyServer(t)
	expect := func(want int, sub, method, path string, body any) string {
		t.Helper()
		c, out := asSub(t, srv, sub, method, path, body)
		if c != want {
			t.Fatalf("%s %s %s: got %d want %d: %s", sub, method, path, c, want, out)
		}
		return out
	}
	expect(201, "ann-sub", "POST", "/api/v1/owners", map[string]any{"id": "acme"})
	expect(409, "kim-sub", "POST", "/api/v1/owners", map[string]any{"id": "acme"})
	expect(400, "kim-sub", "POST", "/api/v1/owners", map[string]any{"id": "Bad Owner"})

	// kim is not an admin of acme: every management call is 403, unclaimed is 404.
	expect(403, "kim-sub", "GET", "/api/v1/owners/acme/apis", nil)
	expect(403, "kim-sub", "POST", "/api/v1/owners/acme/apis", map[string]any{"id": "a", "upstream": "http://x"})
	expect(404, "kim-sub", "GET", "/api/v1/owners/ghost/apis", nil)
	expect(404, "kim-sub", "GET", "/api/v1/owners/..%2fetc/apis", nil)

	// kim claims their own owner and works there.
	expect(201, "kim-sub", "POST", "/api/v1/owners", map[string]any{"id": "kim"})
	expect(201, "kim-sub", "POST", "/api/v1/owners/kim/apis", map[string]any{"id": "a", "upstream": "http://x"})
	if out := expect(200, "kim-sub", "GET", "/api/v1/owners", nil); strings.Contains(out, "acme") || !strings.Contains(out, `"kim"`) {
		t.Fatalf("owners list leaks or misses: %s", out)
	}

	// ann adds kim as an admin of acme; then removes them again.
	expect(200, "ann-sub", "PUT", "/api/v1/owners/acme/admins/kim-sub", nil)
	expect(200, "kim-sub", "GET", "/api/v1/owners/acme/apis", nil)
	expect(200, "ann-sub", "DELETE", "/api/v1/owners/acme/admins/kim-sub", nil)
	expect(403, "kim-sub", "GET", "/api/v1/owners/acme/apis", nil)
	expect(409, "ann-sub", "DELETE", "/api/v1/owners/acme/admins/ann-sub", nil)
}

func TestTenancyKeyBoundToOwner(t *testing.T) {
	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)
	primary, _ := g.provision(up.URL)

	// A second owner with an API of the same id: test's key is invalid there.
	g.must(201, "POST", "/api/v1/owners", map[string]any{"id": "other"})
	g.must(201, "POST", "/api/v1/owners/other/apis", map[string]any{"id": "speech", "upstream": up.URL,
		"operations": []map[string]any{{"id": "get_item", "method": "GET", "path": "/items/{id}"}}})
	if c, _ := g.do("GET", "/apis/other/speech/items/1", nil, g.key(primary)); c != 401 {
		t.Fatalf("key used at another owner: %d", c)
	}
	if c, _ := g.do("GET", "/apis/test/speech/items/1", nil, g.key(primary)); c != 200 {
		t.Fatalf("key at own owner: %d", c)
	}

	// Key-authenticated catalog.
	c, out := g.do("GET", "/apis/test/_catalog", nil, g.key(primary))
	if c != 200 || !strings.Contains(out, `"/apis/test/speech"`) || !strings.Contains(out, `"kim-speech-basic"`) {
		t.Fatalf("_catalog: %d %s", c, out)
	}
	if c, _ := g.do("GET", "/apis/other/_catalog", nil, g.key(primary)); c != 401 {
		t.Fatalf("_catalog at another owner: %d", c)
	}
	if c, _ := g.do("GET", "/apis/test/_catalog", nil, nil); c != 401 {
		t.Fatalf("_catalog without key: %d", c)
	}
	g.assertNoCanary(out)
}

// --- Connectors (ADR 0004) ---

// fakeConnector speaks the agent-tunnel host side of the protocol and
// forwards every stream to target.
type fakeConnector struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error // register outcome
}

func startFakeConnector(t *testing.T, gwURL, owner, token, slot, target string) *fakeConnector {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	fc := &fakeConnector{cancel: cancel, done: make(chan struct{})}
	t.Cleanup(func() { cancel(); <-fc.done })

	ws, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(gwURL, "http")+"/connect/v1/control", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sess, err := yamux.Client(websocket.NetConn(ctx, ws, websocket.MessageBinary), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctrl, _ := sess.OpenStream()
	reg, _ := json.Marshal(protocol.RegisterFrame{Type: protocol.FrameRegister, Tunnel: "ignored", Owner: owner, Token: token,
		Slots: []protocol.SlotSpec{{Name: slot, Kind: protocol.SlotKindHTTP}}})
	_, _ = ctrl.Write(append(reg, '\n'))
	br := bufio.NewReader(ctrl)
	line, err := br.ReadBytes('\n')
	if ft, _ := protocol.DecodeEnvelope(line); err != nil || ft != protocol.FrameRegisterAck {
		fc.err = errString("register refused: " + strings.TrimSpace(string(line)))
		sess.Close()
		close(fc.done)
		return fc
	}
	// Heartbeat once to prove ping/pong keeps the nonce.
	ping, _ := json.Marshal(protocol.PingFrame{Type: protocol.FramePing, Nonce: 7})
	_, _ = ctrl.Write(append(ping, '\n'))
	pl, _ := br.ReadBytes('\n')
	var pong protocol.PongFrame
	if json.Unmarshal(pl, &pong) != nil || pong.Type != protocol.FramePong || pong.Nonce != 7 {
		t.Fatalf("pong: %s", pl)
	}
	go func() {
		defer close(fc.done)
		defer sess.Close()
		go func() { <-ctx.Done(); sess.Close() }()
		for {
			st, err := sess.AcceptStream()
			if err != nil {
				return
			}
			go func(st net.Conn) {
				defer st.Close()
				sbr := bufio.NewReader(st)
				if _, err := sbr.ReadBytes('\n'); err != nil {
					return
				}
				up, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer up.Close()
				go func() { _, _ = io.Copy(up, sbr) }()
				_, _ = io.Copy(st, up)
			}(st)
		}
	}()
	return fc
}

type errString string

func (e errString) Error() string { return string(e) }

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestConnectorRunnerUpstream(t *testing.T) {
	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)

	var issued issuedConnectorTokens
	out := g.must(201, "POST", "/api/v1/owners/test/connectors", map[string]any{"id": "scaleway", "name": "Scaleway on Kim's box"})
	if err := json.Unmarshal([]byte(out), &issued); err != nil || !strings.HasPrefix(issued.PrimaryToken, "gwc_scaleway_") {
		t.Fatalf("connector tokens: %s", out)
	}
	if strings.Contains(g.must(200, "GET", "/api/v1/owners/test/connectors/scaleway", nil), "Hash") {
		t.Fatal("connector view exposes hashes")
	}

	// Validation: runner:// may not carry a credential; connector must exist.
	g.must(201, "POST", "/api/v1/owners/test/credentials", map[string]any{"id": "c", "source": "sealed", "value": canary, "inject": map[string]any{"bearer": true}})
	g.must(400, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "scw", "upstream": "runner://scaleway/api", "credential": "c"})
	g.must(400, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "scw", "upstream": "runner://ghost/api"})
	g.must(400, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "scw", "upstream": "runner://scaleway"})
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "scw", "upstream": "runner://scaleway/api/v1",
		"operations": []map[string]any{{"id": "list_servers", "method": "GET", "path": "/servers"}}})
	g.must(201, "POST", "/api/v1/owners/test/mcp-servers", map[string]any{"id": "scw-tools", "api": "scw", "operations": []string{"*"}})
	g.must(201, "POST", "/api/v1/owners/test/bundles", map[string]any{"id": "infra", "apis": []string{"scw"}, "mcpServers": []string{"scw-tools"}})
	g.must(201, "POST", "/api/v1/owners/test/principals", map[string]any{"id": "kim", "sub": "kim-sub"})
	var sub issuedKeys
	_ = json.Unmarshal([]byte(g.must(201, "POST", "/api/v1/owners/test/subscriptions", map[string]any{"bundle": "infra", "principal": "kim"})), &sub)
	key := g.key(sub.PrimaryKey)

	// Offline: a local 503, never a passthrough.
	if c, out := g.do("GET", "/apis/test/scw/servers", nil, key); c != 503 || !strings.Contains(out, "connector offline") {
		t.Fatalf("offline: %d %s", c, out)
	}
	if g.passthru.Load() != 0 || up.count() != 0 {
		t.Fatal("offline call leaked")
	}

	// A bad token and a token at the wrong owner are refused.
	target := strings.TrimPrefix(up.URL, "http://")
	if fc := startFakeConnector(t, g.srv.URL, "test", issued.PrimaryToken+"x", "api", target); fc.err == nil {
		t.Fatal("forged connector token registered")
	}
	g.must(201, "POST", "/api/v1/owners", map[string]any{"id": "other"})
	if fc := startFakeConnector(t, g.srv.URL, "other", issued.PrimaryToken, "api", target); fc.err == nil {
		t.Fatal("connector token registered under another owner")
	}

	fc := startFakeConnector(t, g.srv.URL, "test", issued.PrimaryToken, "api", target)
	if fc.err != nil {
		t.Fatal(fc.err)
	}
	waitFor(t, "presence", func() bool {
		s := g.must(200, "GET", "/api/v1/owners/test/connectors/scaleway", nil)
		return strings.Contains(s, `"connected": true`)
	})

	// HTTP over the tunnel: path mapped under the base path, key stripped,
	// context header added, and nothing forwarded for undeclared operations.
	c, out := g.do("GET", "/apis/test/scw/servers?zone=fr-par-1", nil, map[string]string{"Api-Key": sub.PrimaryKey, gatewayContextHeader: "sub=forged"})
	if c != 200 {
		t.Fatalf("runner call: %d %s", c, out)
	}
	hit := up.last()
	if hit.Path != "/v1/servers" || hit.Query != "zone=fr-par-1" {
		t.Fatalf("runner upstream got %s ?%s", hit.Path, hit.Query)
	}
	if hit.Header.Get("Api-Key") != "" || strings.Contains(hit.Header.Get(gatewayContextHeader), "forged") {
		t.Fatalf("runner upstream headers: %v", hit.Header)
	}
	if got := hit.Header.Get(gatewayContextHeader); got != "sub=kim-infra; principal=kim; bundle=infra; op=list_servers" {
		t.Fatalf("context header: %q", got)
	}
	if c, _ := g.do("DELETE", "/apis/test/scw/servers", nil, key); c != 404 {
		t.Fatalf("undeclared op over runner: %d", c)
	}

	// MCP tool call over the same connector.
	c, out = g.do("POST", "/mcp/test/scw-tools", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "list_servers", "arguments": map[string]any{}}}, key)
	if c != 200 || !strings.Contains(out, `/v1/servers`) || strings.Contains(out, `"isError":true`) {
		t.Fatalf("mcp over runner: %d %s", c, out)
	}
	if up.last().Header.Get(gatewayContextHeader) == "" {
		t.Fatal("mcp call lacks context header")
	}

	// Rotation: regenerating the unused secondary keeps the primary session.
	g.must(200, "POST", "/api/v1/owners/test/connectors/scaleway/regenerate", map[string]any{"slot": "secondary"})
	if c, _ := g.do("GET", "/apis/test/scw/servers", nil, key); c != 200 {
		t.Fatalf("secondary regenerate dropped the primary session: %d", c)
	}

	// Deleting a connector in use is refused; disabling drops the session.
	g.must(409, "DELETE", "/api/v1/owners/test/connectors/scaleway", nil)
	g.must(200, "POST", "/api/v1/owners/test/connectors/scaleway/disable", nil)
	<-fc.done
	if c, _ := g.do("GET", "/apis/test/scw/servers", nil, key); c != 503 {
		t.Fatalf("after disable: %d", c)
	}
	g.assertNoCanary()
}

func TestUnclaimedOwnersAreNotCached(t *testing.T) {
	g := newTestGateway(t, nil)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("zzz-%d", i)
		g.do("GET", "/apis/"+id+"/x/y", nil, nil)
		g.do("GET", "/apis/"+id+"/_catalog", nil, nil)
		g.do("POST", "/mcp/"+id+"/x", "{}", nil)
		g.do("GET", "/.well-known/oauth-protected-resource/mcp/"+id+"/x", nil, nil)
	}
	g.lane.root.mu.Lock()
	n := len(g.lane.root.owners)
	g.lane.root.mu.Unlock()
	if n != 1 { // only the claimed "test"
		t.Fatalf("root caches %d owners after anonymous traffic", n)
	}
}

func TestConnectorTokenRefusedOnCatchAll(t *testing.T) {
	g := newTestGateway(t, nil)
	if c, _ := g.do("POST", "/v1/messages", "{}", map[string]string{"Authorization": "Bearer gwc_a_b"}); c != 401 {
		t.Fatalf("gwc_ on passthrough: %d", c)
	}
	if g.passthru.Load() != 0 {
		t.Fatal("gwc_ token reached the passthrough lane")
	}
}

func TestParseRunnerUpstream(t *testing.T) {
	for in, want := range map[string]*runnerTarget{
		"runner://scw/api":      {connector: "scw", slot: "api"},
		"runner://scw/api/v1/x": {connector: "scw", slot: "api", basePath: "/v1/x"},
		"runner://scw":          nil,
		"runner://SCW/api":      nil,
		"runner://scw/a--b":     nil,
		"runner://u@scw/api":    nil,
		"runner://scw/api?x=1":  nil,
		"http://scw/api":        nil,
	} {
		got, ok := parseRunnerUpstream(in)
		if (want == nil) == ok || (want != nil && got != *want) {
			t.Errorf("%s: got %+v %v", in, got, ok)
		}
	}
}
