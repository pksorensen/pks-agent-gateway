package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsUpstream is a fake realtime endpoint: it greets, then echoes every frame
// back with its type intact and "echo:" prefixed.
type wsUpstream struct {
	*httptest.Server
	mu      sync.Mutex
	upgrade []*http.Request // upgrade requests as received
	dials   atomic.Int32    // every request that reached the upstream
	closed  chan struct{}   // one value per session that ended upstream-side
}

func newWSUpstream(t *testing.T, tls bool) *wsUpstream {
	u := &wsUpstream{closed: make(chan struct{}, 16)}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.dials.Add(1)
		u.mu.Lock()
		u.upgrade = append(u.upgrade, r.Clone(context.Background()))
		u.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { u.closed <- struct{}{} }()
		ctx := context.Background()
		if err := c.Write(ctx, websocket.MessageText, []byte(`{"type":"session.created"}`)); err != nil {
			return
		}
		for {
			typ, b, err := c.Read(ctx)
			if err != nil {
				return
			}
			if err := c.Write(ctx, typ, append([]byte("echo:"), b...)); err != nil {
				return
			}
		}
	})
	if tls {
		u.Server = httptest.NewTLSServer(h)
	} else {
		u.Server = httptest.NewServer(h)
	}
	t.Cleanup(u.Close)
	return u
}

func (u *wsUpstream) last() *http.Request {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.upgrade[len(u.upgrade)-1]
}

var realtimeOp = map[string]any{"id": "realtime", "method": "GET", "path": "/mai/v1/realtime",
	"websocket": true, "meter": "audio"}

// provisionStream registers the streaming API with a bearer credential and
// returns a gateway key for a principal with limits.
func provisionStream(g *gw, upstream string, op map[string]any, limits map[string]any) string {
	g.must(201, "POST", "/api/v1/owners/test/credentials", map[string]any{
		"id": "foundry", "source": "sealed", "value": canary, "inject": map[string]any{"bearer": true}})
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "speech-stream", "upstream": upstream,
		"credential": "foundry", "operations": []map[string]any{op}})
	g.must(201, "POST", "/api/v1/owners/test/bundles", map[string]any{"id": "ai", "apis": []string{"speech-stream"}})
	g.must(201, "POST", "/api/v1/owners/test/principals", map[string]any{"id": "site", "kind": "service", "limits": limits})
	var issued issuedKeys
	_ = json.Unmarshal([]byte(g.must(201, "POST", "/api/v1/owners/test/subscriptions",
		map[string]any{"bundle": "ai", "principal": "site"})), &issued)
	return issued.PrimaryKey
}

const realtimePath = "/apis/test/speech-stream/mai/v1/realtime?intent=transcription"

func (g *gw) dialWS(key string) (*websocket.Conn, *http.Response, error) {
	h := http.Header{}
	if key != "" {
		h.Set("Api-Key", key)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return websocket.Dial(ctx, "ws"+strings.TrimPrefix(g.srv.URL, "http")+realtimePath, &websocket.DialOptions{HTTPHeader: h})
}

func readText(t *testing.T, c *websocket.Conn) (websocket.MessageType, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	typ, b, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return typ, string(b)
}

func usageOf(g *gw) []UsageRecord {
	day := time.Now().UTC().Format("2006-01-02")
	st, _ := g.lane.root.Owner("test")
	recs, _ := st.ReadUsage(day, day)
	return recs
}

// waitUsage waits for n usage records: a session's record is written after
// the proxy returns, which is after the client sees the close.
func waitUsage(t *testing.T, g *gw, n int) []UsageRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if recs := usageOf(g); len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage records: %d, want %d", len(usageOf(g)), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestWebSocketProxiedOverHTTPS(t *testing.T) {
	up := newWSUpstream(t, true)
	g := newTestGateway(t, nil)
	g.lane.transport = up.Client().Transport // trusts the test certificate
	key := provisionStream(g, up.URL, realtimeOp, nil)

	c, resp, err := g.dialWS(key)
	if err != nil {
		t.Fatalf("dial: %v (%v)", err, resp)
	}
	if typ, msg := readText(t, c); typ != websocket.MessageText || msg != `{"type":"session.created"}` {
		t.Fatalf("server-first frame: %v %q", typ, msg)
	}
	ctx := context.Background()
	text := `{"type":"input_audio_buffer.append","audio":"AAEC/w=="}`
	_ = c.Write(ctx, websocket.MessageText, []byte(text))
	if typ, msg := readText(t, c); typ != websocket.MessageText || msg != "echo:"+text {
		t.Fatalf("text echo: %v %q", typ, msg)
	}
	bin := []byte{0, 1, 2, 0xff, 0xfe}
	_ = c.Write(ctx, websocket.MessageBinary, bin)
	if typ, msg := readText(t, c); typ != websocket.MessageBinary || msg != "echo:"+string(bin) {
		t.Fatalf("binary echo: %v %q", typ, msg)
	}
	_ = c.Close(websocket.StatusNormalClosure, "")

	r := up.last()
	if r.URL.Path != "/mai/v1/realtime" || r.URL.RawQuery != "intent=transcription" {
		t.Fatalf("upstream got %s?%s", r.URL.Path, r.URL.RawQuery)
	}
	if r.Header.Get("Authorization") != "Bearer "+canary || r.Header.Get("Api-Key") != "" {
		t.Fatalf("upstream credentials: auth=%q api-key=%q", r.Header.Get("Authorization"), r.Header.Get("Api-Key"))
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		t.Fatalf("upstream Upgrade = %q", r.Header.Get("Upgrade"))
	}

	rec := waitUsage(t, g, 1)[0]
	if rec.Status != 101 || rec.Operation != "realtime" || rec.BytesIn == 0 || rec.BytesOut == 0 || rec.AudioSeconds < 1 {
		t.Fatalf("usage: %+v", rec)
	}
}

func TestWebSocketOpRejectsPlainRequestsAndBadCredentials(t *testing.T) {
	up := newWSUpstream(t, false)
	g := newTestGateway(t, nil)
	key := provisionStream(g, up.URL, realtimeOp, nil)

	if c, out := g.do("GET", realtimePath, nil, g.key(key)); c != http.StatusUpgradeRequired {
		t.Fatalf("plain GET: %d %s", c, out)
	}
	if c, _ := g.do("POST", realtimePath, "{}", g.key(key)); c != 404 {
		t.Fatalf("POST to a GET websocket op: %d", c)
	}
	for _, k := range []string{"", "gwk_test_not-a-real-key"} {
		_, resp, err := g.dialWS(k)
		if err == nil || resp == nil || resp.StatusCode != 401 {
			t.Fatalf("key %q: err=%v resp=%v", k, err, resp)
		}
	}
	if n := up.dials.Load(); n != 0 {
		t.Fatalf("upstream dialed %d times for rejected requests", n)
	}
}

func TestWebSocketConcurrentStreamCap(t *testing.T) {
	up := newWSUpstream(t, false)
	g := newTestGateway(t, nil)
	key := provisionStream(g, up.URL, realtimeOp, map[string]any{"maxStreams": 1})

	first, _, err := g.dialWS(key)
	if err != nil {
		t.Fatal(err)
	}
	readText(t, first)
	_, resp, err := g.dialWS(key)
	if err == nil || resp == nil || resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("second stream: err=%v resp=%v", err, resp)
	}
	if n := up.dials.Load(); n != 1 {
		t.Fatalf("refused stream reached upstream: dials=%d", n)
	}
	_ = first.Close(websocket.StatusNormalClosure, "")
	waitUsage(t, g, 2) // the 429 and the closed session: the slot is free again
	third, _, err := g.dialWS(key)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	_ = third.Close(websocket.StatusNormalClosure, "")
}

func TestWebSocketDefaultStreamCapIsTwo(t *testing.T) {
	up := newWSUpstream(t, false)
	g := newTestGateway(t, nil)
	key := provisionStream(g, up.URL, realtimeOp, nil)
	for i := 0; i < 2; i++ {
		c, _, err := g.dialWS(key)
		if err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
		defer c.CloseNow()
	}
	if _, resp, err := g.dialWS(key); err == nil || resp == nil || resp.StatusCode != 429 {
		t.Fatalf("third stream: err=%v resp=%v", err, resp)
	}
}

func TestWebSocketSessionDurationCapClosesBothSides(t *testing.T) {
	up := newWSUpstream(t, false)
	g := newTestGateway(t, nil)
	op := map[string]any{"id": "realtime", "method": "GET", "path": "/mai/v1/realtime", "websocket": true, "maxSessionSeconds": 1}
	key := provisionStream(g, up.URL, op, nil)

	c, _, err := g.dialWS(key)
	if err != nil {
		t.Fatal(err)
	}
	readText(t, c)
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller side not closed by the cap: %v", err)
	}
	if el := time.Since(started); el < 800*time.Millisecond || el > 3*time.Second {
		t.Fatalf("closed after %v, want ~1s", el)
	}
	select {
	case <-up.closed:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream side not closed by the cap")
	}
	if rec := waitUsage(t, g, 1)[0]; rec.Status != 101 || rec.DurationMs < 900 || rec.AudioSeconds != 0 {
		t.Fatalf("unmetered session usage: %+v", rec)
	}
}

func TestWebSocketAudioQuota(t *testing.T) {
	up := newWSUpstream(t, false)
	g := newTestGateway(t, nil)
	key := provisionStream(g, up.URL, realtimeOp, map[string]any{"audioSecondsPerDay": 1})

	// A short session still charges a whole second: the day's quota is spent.
	c, _, err := g.dialWS(key)
	if err != nil {
		t.Fatal(err)
	}
	readText(t, c)
	_ = c.Close(websocket.StatusNormalClosure, "")
	if rec := waitUsage(t, g, 1)[0]; rec.AudioSeconds != 1 || rec.DurationMs >= 1000 {
		t.Fatalf("metered session usage: %+v", rec)
	}

	_, resp, err := g.dialWS(key)
	if err == nil || resp == nil || resp.StatusCode != 429 {
		t.Fatalf("exhausted quota: err=%v resp=%v", err, resp)
	}
	if n := up.dials.Load(); n != 1 {
		t.Fatalf("refused stream reached upstream: dials=%d", n)
	}

	// A restart forgets memory, not the day's usage.
	g.lane.limits = newUsageLimiter()
	if _, resp, err := g.dialWS(key); err == nil || resp == nil || resp.StatusCode != 429 {
		t.Fatalf("after restart: err=%v resp=%v", err, resp)
	}
}

func TestWebSocketSessionBoundedByRemainingQuota(t *testing.T) {
	up := newWSUpstream(t, false)
	g := newTestGateway(t, nil)
	key := provisionStream(g, up.URL, realtimeOp, map[string]any{"audioSecondsPerDay": 1})
	c, _, err := g.dialWS(key)
	if err != nil {
		t.Fatal(err)
	}
	readText(t, c)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	if _, _, err := c.Read(ctx); err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("session outran a 1 s quota: %v after %v", err, time.Since(started))
	}
}

func TestWebSocketOperationValidation(t *testing.T) {
	up := newWSUpstream(t, false)
	g := newTestGateway(t, nil)
	provisionStream(g, up.URL, realtimeOp, nil)
	for _, op := range []map[string]any{
		{"id": "x", "method": "POST", "path": "/x", "websocket": true},
		{"id": "x", "method": "GET", "path": "/x", "maxSessionSeconds": 10},
		{"id": "x", "method": "GET", "path": "/x", "websocket": true, "maxSessionSeconds": -1},
	} {
		if c, out := g.do("PUT", "/api/v1/owners/test/apis/speech-stream/operations/x", op, nil); c != 400 {
			t.Fatalf("%v: %d %s", op, c, out)
		}
	}
	if c, _ := g.do("POST", "/api/v1/owners/test/principals", map[string]any{"id": "p2", "limits": map[string]any{"maxStreams": -2}}, nil); c != 400 {
		t.Fatalf("maxStreams -2: %d", c)
	}
	// A websocket operation is never an MCP tool.
	if c, _ := g.do("POST", "/api/v1/owners/test/mcp-servers", map[string]any{"id": "m", "api": "speech-stream", "operations": []string{"realtime"}}, nil); c != 400 {
		t.Fatalf("mcp tool from websocket op: %d", c)
	}
}

// A non-websocket operation never switches protocols, even if asked to.
func TestHTTPOperationDoesNotUpgrade(t *testing.T) {
	up := newWSUpstream(t, false)
	g := newTestGateway(t, nil)
	key := provisionStream(g, up.URL, map[string]any{"id": "realtime", "method": "GET", "path": "/mai/v1/realtime"}, nil)
	if _, resp, err := g.dialWS(key); err == nil || (resp != nil && resp.StatusCode == 101) {
		t.Fatalf("plain op upgraded: %v", resp)
	}
	if r := up.last(); r.Header.Get("Upgrade") != "" {
		t.Fatalf("upstream saw Upgrade: %q", r.Header.Get("Upgrade"))
	}
}
