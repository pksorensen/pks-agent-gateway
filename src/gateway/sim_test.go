package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestSim returns a simulator backed by a temp-dir store.
func newTestSim(t *testing.T, enabled bool) *simulator {
	t.Helper()
	return newSimulator(NewStore(t.TempDir(), "default"), enabled)
}

// failingNext is a proxy stand-in that fails the test if ever invoked — the
// structural leak guard.
func failingNext(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("LEAK: request %s %s reached the upstream proxy", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusBadGateway)
	})
}

func messagesBody(model, userText string, stream bool) string {
	b, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 64, "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": userText}},
	})
	return string(b)
}

// messagesBodyWithTools mimics a real Claude Code agent-loop request, which
// always advertises tools (toolless requests are classified as side-calls).
func messagesBodyWithTools(model, userText string, stream bool) string {
	b, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 64, "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": userText}},
		"tools": []map[string]any{
			{"name": "Bash", "description": "shell", "input_schema": map[string]any{"type": "object"}},
		},
	})
	return string(b)
}

func simRequest(method, path, body string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func TestParseSimDirective(t *testing.T) {
	cases := []struct {
		raw     string
		want    simDirective
		wantErr bool
	}{
		{"echo", simDirective{Mode: "echo"}, false},
		{"ECHO", simDirective{Mode: "echo"}, false},
		{"echo:extra", simDirective{}, true},
		{"scenario:tool-loop-then-stop", simDirective{Mode: "scenario", Arg: "tool-loop-then-stop"}, false},
		{"scenario:", simDirective{}, true},
		{"replay:my-cassette", simDirective{Mode: "replay", Arg: "my-cassette"}, false},
		{"replay:../../evil", simDirective{Mode: "replay", Arg: "evil"}, false}, // traversal slugified away
		{"bogus", simDirective{}, true},
		{"", simDirective{}, true},
	}
	for _, c := range cases {
		got, err := parseSimDirective(c.raw)
		if c.wantErr != (err != nil) {
			t.Errorf("parseSimDirective(%q): err = %v, wantErr = %v", c.raw, err, c.wantErr)
			continue
		}
		if !c.wantErr && got != c.want {
			t.Errorf("parseSimDirective(%q) = %+v, want %+v", c.raw, got, c.want)
		}
	}
}

func TestSimKeySniffPrecedence(t *testing.T) {
	// Header beats x-api-key beats bearer.
	r := simRequest("POST", "/v1/messages", "", map[string]string{
		"X-Gateway-Sim": "scenario:from-header",
		"x-api-key":     "sim-scenario:from-key",
		"Authorization": "Bearer sim-scenario:from-bearer",
	})
	if raw, ok := simKeyFromRequest(r); !ok || raw != "scenario:from-header" {
		t.Fatalf("want header directive, got %q ok=%v", raw, ok)
	}

	r = simRequest("POST", "/v1/messages", "", map[string]string{
		"x-api-key":     "sim-echo",
		"Authorization": "Bearer sim-scenario:from-bearer",
	})
	if raw, ok := simKeyFromRequest(r); !ok || raw != "echo" {
		t.Fatalf("want x-api-key directive, got %q ok=%v", raw, ok)
	}

	r = simRequest("POST", "/v1/messages", "", map[string]string{
		"Authorization": "Bearer sim-replay:tape",
	})
	if raw, ok := simKeyFromRequest(r); !ok || raw != "replay:tape" {
		t.Fatalf("want bearer directive, got %q ok=%v", raw, ok)
	}

	// Real keys are not sim keys.
	r = simRequest("POST", "/v1/messages", "", map[string]string{
		"x-api-key":     "sk-ant-api03-realkey",
		"Authorization": "Bearer real-jwt",
	})
	if _, ok := simKeyFromRequest(r); ok {
		t.Fatal("real key must not be detected as sim key")
	}
}

func TestGateNeverProxiesSimKeys(t *testing.T) {
	cases := []struct {
		name       string
		enabled    bool
		path       string
		key        string
		wantStatus int
	}{
		{"disabled → 403 not proxied", false, "/v1/messages", "sim-echo", http.StatusForbidden},
		{"malformed directive → 400", true, "/v1/messages", "sim-bogus", http.StatusBadRequest},
		{"unknown endpoint → 404", true, "/v1/models", "sim-echo", http.StatusNotFound},
		{"unknown api path with sim key → 404", true, "/api/nonexistent", "sim-echo", http.StatusNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sim := newTestSim(t, c.enabled)
			gate := sim.Gate(failingNext(t))
			w := httptest.NewRecorder()
			gate.ServeHTTP(w, simRequest("POST", c.path, messagesBody("claude-opus-4-8", "hi", false),
				map[string]string{"x-api-key": c.key}))
			if w.Code != c.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, c.wantStatus, w.Body.String())
			}
			var errResp struct {
				Type  string `json:"type"`
				Error struct {
					Type    string `json:"type"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &errResp); err != nil || errResp.Type != "error" {
				t.Fatalf("expected anthropic-shaped error JSON, got: %s", w.Body.String())
			}
		})
	}
}

func TestGatePassthroughWithoutSimKey(t *testing.T) {
	sim := newTestSim(t, true)
	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusTeapot)
	})
	w := httptest.NewRecorder()
	sim.Gate(next).ServeHTTP(w, simRequest("POST", "/v1/messages", "{}",
		map[string]string{"x-api-key": "sk-ant-real"}))
	if !called || w.Code != http.StatusTeapot {
		t.Fatalf("real-keyed request must pass through (called=%v status=%d)", called, w.Code)
	}
}

func TestEchoNonStreaming(t *testing.T) {
	sim := newTestSim(t, true)
	gate := sim.Gate(failingNext(t))
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBody("claude-opus-4-8", "hello sim", false),
		map[string]string{"x-api-key": "sim-echo"}))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", w.Code, w.Body.String())
	}
	var msg struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Model   string `json:"model"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatalf("bad JSON: %v", err)
	}
	if msg.Type != "message" || msg.Role != "assistant" || msg.Model != "claude-opus-4-8" {
		t.Fatalf("bad envelope: %+v", msg)
	}
	if len(msg.Content) != 1 || msg.Content[0].Text != "hello sim" {
		t.Fatalf("echo text wrong: %+v", msg.Content)
	}
	if msg.StopReason != "end_turn" {
		t.Fatalf("stop_reason = %q", msg.StopReason)
	}
	body := messagesBody("claude-opus-4-8", "hello sim", false)
	if want := len(body) / 4; msg.Usage.InputTokens != want {
		t.Fatalf("input_tokens = %d, want %d", msg.Usage.InputTokens, want)
	}
	if msg.Usage.OutputTokens != 2 { // "hello sim" = 2 words
		t.Fatalf("output_tokens = %d, want 2", msg.Usage.OutputTokens)
	}
}

func TestAnthropicPathPrefixAlias(t *testing.T) {
	sim := newTestSim(t, true)
	gate := sim.Gate(failingNext(t))
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/anthropic/v1/messages", messagesBody("claude-sonnet-5", "hi", false),
		map[string]string{"Authorization": "Bearer sim-echo"}))
	if w.Code != http.StatusOK {
		t.Fatalf("expected /anthropic alias to serve, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCountTokensDeterministic(t *testing.T) {
	sim := newTestSim(t, true)
	gate := sim.Gate(failingNext(t))
	body := messagesBody("claude-opus-4-8", "count me", false)
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages/count_tokens", body,
		map[string]string{"x-api-key": "sim-echo"}))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	var resp map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if want := len(body) / 4; resp["input_tokens"] != want {
		t.Fatalf("input_tokens = %d, want %d", resp["input_tokens"], want)
	}

	// count_tokens never advances session state.
	if infos := sim.snapshotSessions(); len(infos) != 0 {
		t.Fatalf("count_tokens must not create sessions, got %+v", infos)
	}
}

func TestSessionKeyResolutionOrder(t *testing.T) {
	mkReq := func(headers map[string]string, metadataUserID string) (*http.Request, *anthropicRequest) {
		payload := map[string]any{
			"model":    "claude-opus-4-8",
			"messages": []map[string]any{{"role": "user", "content": "stable first message"}},
		}
		if metadataUserID != "" {
			payload["metadata"] = map[string]string{"user_id": metadataUserID}
		}
		b, _ := json.Marshal(payload)
		r := simRequest("POST", "/v1/messages", string(b), headers)
		var req anthropicRequest
		_ = json.Unmarshal(b, &req)
		return r, &req
	}

	r, req := mkReq(map[string]string{"X-Gateway-Sim-Session": "explicit"}, "meta-user")
	if k := sessionKeyFor(r, req); k != "explicit" {
		t.Fatalf("explicit header should win, got %q", k)
	}

	r, req = mkReq(nil, "meta-user")
	if k := sessionKeyFor(r, req); k != "meta-user" {
		t.Fatalf("metadata.user_id should win, got %q", k)
	}

	r, req = mkReq(nil, "")
	k1 := sessionKeyFor(r, req)
	if !strings.HasPrefix(k1, "conv-") {
		t.Fatalf("conversation hash expected, got %q", k1)
	}
	// Stable across calls with the same first user message.
	r2, req2 := mkReq(nil, "")
	if k2 := sessionKeyFor(r2, req2); k2 != k1 {
		t.Fatalf("conversation hash not stable: %q vs %q", k1, k2)
	}

	var empty anthropicRequest
	if k := sessionKeyFor(httptest.NewRequest("POST", "/v1/messages", nil), &empty); k != "default" {
		t.Fatalf("fallback should be default, got %q", k)
	}
}

func TestSideCallLaneDoesNotAdvanceMainIndex(t *testing.T) {
	sim := newTestSim(t, true)
	gate := sim.Gate(failingNext(t))

	do := func(model string, withTools bool) {
		payload := map[string]any{
			"model":    model,
			"messages": []map[string]any{{"role": "user", "content": "x"}},
			"metadata": map[string]string{"user_id": "sess-1"},
		}
		if withTools {
			payload["tools"] = []map[string]any{
				{"name": "Bash", "description": "shell", "input_schema": map[string]any{"type": "object"}},
			}
		}
		b, _ := json.Marshal(payload)
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, simRequest("POST", "/v1/messages", string(b),
			map[string]string{"x-api-key": "sim-scenario:tool-loop-then-stop"}))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}

	do("claude-haiku-4-5-20251001", true) // side call by model regex
	do("claude-opus-4-8", false)          // side call: toolless (session-title shape)
	do("claude-opus-4-8", true)           // main lane request 0

	infos := sim.snapshotSessions()
	if len(infos) != 1 {
		t.Fatalf("want 1 session, got %d", len(infos))
	}
	if infos[0].Index != 1 || infos[0].SideIndex != 2 {
		t.Fatalf("lanes wrong: index=%d sideIndex=%d (want 1/2)", infos[0].Index, infos[0].SideIndex)
	}
}

func TestScenarioToolLoopThenStop(t *testing.T) {
	sim := newTestSim(t, true)
	gate := sim.Gate(failingNext(t))

	toolReq := func(withToolResult bool) string {
		msgs := []map[string]any{{"role": "user", "content": "do the task"}}
		if withToolResult {
			msgs = append(msgs,
				map[string]any{"role": "assistant", "content": []map[string]any{
					{"type": "tool_use", "id": "toolu_sim_0_2", "name": "mcp__vibecast__stop_broadcast", "input": map[string]any{}},
				}},
				map[string]any{"role": "user", "content": []map[string]any{
					{"type": "tool_result", "tool_use_id": "toolu_sim_0_2", "content": "stopped"},
				}},
			)
		}
		b, _ := json.Marshal(map[string]any{
			"model": "claude-opus-4-8", "stream": false,
			"metadata": map[string]string{"user_id": "task-sess"},
			"messages": msgs,
			"tools": []map[string]any{
				{"name": "Bash", "description": "shell", "input_schema": map[string]any{"type": "object"}},
				{"name": "mcp__vibecast__stop_broadcast", "description": "stop", "input_schema": map[string]any{"type": "object"}},
			},
		})
		return string(b)
	}

	// Request 0: expect tool_use for the resolved stop_broadcast tool.
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", toolReq(false),
		map[string]string{"x-api-key": "sim-scenario:tool-loop-then-stop"}))
	var first struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &first); err != nil {
		t.Fatalf("bad JSON: %v — %s", err, w.Body.String())
	}
	if first.StopReason != "tool_use" {
		t.Fatalf("stop_reason = %q, want tool_use", first.StopReason)
	}
	foundTool := false
	for _, c := range first.Content {
		if c.Type == "tool_use" {
			foundTool = true
			if c.Name != "mcp__vibecast__stop_broadcast" {
				t.Fatalf("$toolMatching resolved to %q", c.Name)
			}
			var input map[string]string
			_ = json.Unmarshal(c.Input, &input)
			if input["conclusion"] != "success" {
				t.Fatalf("tool input = %s", c.Input)
			}
		}
	}
	if !foundTool {
		t.Fatalf("no tool_use block in %s", w.Body.String())
	}

	// Request 1 (after tool_result): end_turn text.
	w = httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", toolReq(true),
		map[string]string{"x-api-key": "sim-scenario:tool-loop-then-stop"}))
	var second struct {
		StopReason string `json:"stop_reason"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &second)
	if second.StopReason != "end_turn" {
		t.Fatalf("second request stop_reason = %q, want end_turn — %s", second.StopReason, w.Body.String())
	}
}

func TestScenarioRateLimitThenSucceed(t *testing.T) {
	sim := newTestSim(t, true)
	gate := sim.Gate(failingNext(t))
	body := messagesBodyWithTools("claude-opus-4-8", "hi", false)
	hdrs := map[string]string{
		"x-api-key":             "sim-scenario:rate-limit-then-succeed",
		"X-Gateway-Sim-Session": "rl-sess",
	}

	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", body, hdrs))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("first request: status %d, want 429", w.Code)
	}
	if ra := w.Header().Get("retry-after"); ra != "1" {
		t.Fatalf("retry-after = %q, want 1", ra)
	}

	w = httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", body, hdrs))
	if w.Code != http.StatusOK {
		t.Fatalf("retry: status %d, want 200", w.Code)
	}
}

func TestUnknownScenario404(t *testing.T) {
	sim := newTestSim(t, true)
	gate := sim.Gate(failingNext(t))
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBody("m", "x", false),
		map[string]string{"x-api-key": "sim-scenario:does-not-exist"}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestTokenHelpers(t *testing.T) {
	if got := estimateInputTokens([]byte("")); got != 1 {
		t.Fatalf("empty body → %d, want 1", got)
	}
	if got := estimateInputTokens(make([]byte, 400)); got != 100 {
		t.Fatalf("400 bytes → %d, want 100", got)
	}
	blocks := []simBlock{
		{Type: "text", Text: "three words here"},
		{Type: "thinking", Thinking: "two words"},
		{Type: "tool_use", ToolInput: json.RawMessage(`{"a":"b c"}`)},
	}
	// text 3 + thinking 2 + compact {"a":"b c"} = 2 fields ("{\"a\":\"b" and "c\"}")
	if got := countOutputTokens(blocks); got != 7 {
		t.Fatalf("countOutputTokens = %d, want 7", got)
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	sim := newTestSim(t, true)
	gate := sim.Gate(failingNext(t))
	big := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"%s"}]}`,
		strings.Repeat("x", maxSimBodyBytes))
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", big,
		map[string]string{"x-api-key": "sim-echo"}))
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", w.Code)
	}
}
