package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// fakeSSEUpstream serves a fixed Anthropic-shaped SSE stream with per-frame
// flushes, mimicking api.anthropic.com.
func fakeSSEUpstream(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	msg := simMessage{
		ID: "msg_rec_1", Model: "claude-opus-4-8", StopReason: "end_turn",
		Blocks:      []simBlock{{Type: "text", Text: "recorded response text"}},
		InputTokens: 10, OutputTokens: 3,
	}
	rec := httptest.NewRecorder()
	if err := writeMessageSSE(rec, t.Context(), msg, pacing{ChunkSize: 8}); err != nil {
		t.Fatal(err)
	}
	sseBody := rec.Body.String()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Gateway-Record"); got != "" {
			t.Errorf("X-Gateway-Record header leaked upstream: %q", got)
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		fl := w.(http.Flusher)
		for _, frame := range strings.Split(sseBody, "\n\n") {
			if strings.TrimSpace(frame) == "" {
				continue
			}
			_, _ = io.WriteString(w, frame+"\n\n")
			fl.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv, sseBody
}

func recordExchange(t *testing.T, sim *simulator, upstreamURL, cassetteName, body string) string {
	t.Helper()
	u, err := url.Parse(upstreamURL)
	if err != nil {
		t.Fatal(err)
	}
	gate := httptest.NewServer(sim.Gate(newProxy(u, "")))
	t.Cleanup(gate.Close)

	req, _ := http.NewRequest("POST", gate.URL+"/v1/messages?beta=true", strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-ant-realkey-secret")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("X-Gateway-Record", cassetteName)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("record passthrough status %d: %s", resp.StatusCode, got)
	}
	return string(got)
}

func TestRecordPassthroughCapturesExchange(t *testing.T) {
	upstream, sseBody := fakeSSEUpstream(t)
	sim := newTestSim(t, true)

	reqBody := messagesBodyWithTools("claude-opus-4-8", "record me", true)
	clientGot := recordExchange(t, sim, upstream.URL, "my-tape", reqBody)

	// Client received the streamed bytes unmodified.
	if clientGot != sseBody {
		t.Fatalf("client body differs from upstream stream:\n got: %q\nwant: %q", clientGot, sseBody)
	}

	entries, err := sim.store.ReadCassette("my-tape")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 cassette entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Seq != 0 || e.Method != "POST" || e.Path != "/v1/messages" || e.Query != "beta=true" || !e.SSE {
		t.Fatalf("entry meta wrong: %+v", e)
	}
	if e.Fingerprint == nil || e.Fingerprint.Model != "claude-opus-4-8" || e.Fingerprint.MessageCount != 1 || !e.Fingerprint.Stream {
		t.Fatalf("fingerprint wrong: %+v", e.Fingerprint)
	}

	// Credential headers never hit the disk; allowlisted ones do.
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "sk-ant-realkey-secret") {
		t.Fatal("api key leaked into cassette")
	}
	if e.RequestHeaders["anthropic-version"] != "2023-06-01" {
		t.Fatalf("allowlisted header missing: %+v", e.RequestHeaders)
	}
	if _, ok := e.RequestHeaders["x-api-key"]; ok {
		t.Fatal("x-api-key must not be recorded")
	}

	// Response bytes round-trip through base64.
	decoded, err := base64.StdEncoding.DecodeString(e.ResponseBodyB64)
	if err != nil || string(decoded) != sseBody {
		t.Fatalf("response bytes did not round-trip (err=%v)", err)
	}
}

func TestReplayAdaptationMatrix(t *testing.T) {
	upstream, sseBody := fakeSSEUpstream(t)
	sim := newTestSim(t, true)
	reqBody := messagesBodyWithTools("claude-opus-4-8", "record me", true)
	_ = recordExchange(t, sim, upstream.URL, "matrix", reqBody)

	// Also append a non-SSE (JSON) entry by hand for the JSON cells.
	jsonMsgRec := httptest.NewRecorder()
	writeMessageJSON(jsonMsgRec, simMessage{
		ID: "msg_rec_2", Model: "claude-opus-4-8", StopReason: "end_turn",
		Blocks:      []simBlock{{Type: "text", Text: "json recorded"}},
		InputTokens: 5, OutputTokens: 2,
	})
	jsonEntryBody, _ := json.Marshal(map[string]any{
		"model": "claude-opus-4-8", "stream": false,
		"messages": []map[string]any{{"role": "user", "content": "second"}},
		"tools": []map[string]any{
			{"name": "Bash", "description": "shell", "input_schema": map[string]any{"type": "object"}},
		},
	})
	if err := sim.store.AppendCassetteEntry("matrix", &cassetteEntry{
		Method: "POST", Path: "/v1/messages", Status: 200,
		RequestBody:     jsonEntryBody,
		Fingerprint:     fingerprintOf(jsonEntryBody),
		ResponseHeaders: map[string]string{"content-type": "application/json"},
		ResponseBodyB64: base64.StdEncoding.EncodeToString(jsonMsgRec.Body.Bytes()),
		SSE:             false,
	}); err != nil {
		t.Fatal(err)
	}

	gate := sim.Gate(failingNext(t))
	replayHdrs := func(session string) map[string]string {
		return map[string]string{
			"x-api-key":             "sim-replay:matrix",
			"X-Gateway-Sim-Session": session,
		}
	}

	// Cell 1: stream wanted, entry is SSE → raw byte replay.
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBodyWithTools("claude-opus-4-8", "record me", true), replayHdrs("s1")))
	if w.Body.String() != sseBody {
		t.Fatalf("raw SSE replay differs:\n got %q\nwant %q", w.Body.String(), sseBody)
	}

	// Cell 2 (same session, second request): stream wanted, entry is JSON → synthesized SSE.
	w = httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBodyWithTools("claude-opus-4-8", "second", true), replayHdrs("s1")))
	events := parseSSE(t, w.Body.String())
	text := ""
	for _, e := range events {
		if e.Event == "content_block_delta" {
			var d struct {
				Delta struct {
					Text string `json:"text"`
				} `json:"delta"`
			}
			_ = json.Unmarshal([]byte(e.Data), &d)
			text += d.Delta.Text
		}
	}
	if text != "json recorded" {
		t.Fatalf("JSON→SSE adaptation text: %q", text)
	}

	// Cell 3: non-stream wanted, entry is SSE → assembled JSON message.
	w = httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBodyWithTools("claude-opus-4-8", "record me", false), replayHdrs("s2")))
	var m struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("SSE→JSON: %v — %s", err, w.Body.String())
	}
	if len(m.Content) != 1 || m.Content[0].Text != "recorded response text" || m.StopReason != "end_turn" {
		t.Fatalf("SSE→JSON assembly wrong: %s", w.Body.String())
	}
	if m.Usage.InputTokens != 10 || m.Usage.OutputTokens != 3 {
		t.Fatalf("SSE→JSON usage: %+v", m.Usage)
	}

	// Cell 4 (same session s2, second request): non-stream wanted, entry is JSON → verbatim.
	w = httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBodyWithTools("claude-opus-4-8", "second", false), replayHdrs("s2")))
	if !strings.Contains(w.Body.String(), "json recorded") {
		t.Fatalf("verbatim JSON replay: %s", w.Body.String())
	}
}

func TestReplaySequenceDriftAndExhaustion(t *testing.T) {
	upstream, _ := fakeSSEUpstream(t)
	sim := newTestSim(t, true)
	_ = recordExchange(t, sim, upstream.URL, "seq", messagesBodyWithTools("claude-opus-4-8", "one", true))

	gate := sim.Gate(failingNext(t))
	hdrs := map[string]string{
		"x-api-key":             "sim-replay:seq",
		"X-Gateway-Sim-Session": "seq-sess",
	}

	// Request 0 with a DIFFERENT model → served, but drift warning recorded.
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBodyWithTools("claude-sonnet-5", "one", false), hdrs))
	if w.Code != http.StatusOK {
		t.Fatalf("drifted replay refused: %d", w.Code)
	}

	// Request 1: cassette exhausted → end_turn notice, not an error.
	w = httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBodyWithTools("claude-sonnet-5", "two", false), hdrs))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "exhausted") {
		t.Fatalf("exhausted replay: %d %s", w.Code, w.Body.String())
	}

	infos := sim.snapshotSessions()
	if len(infos) != 1 {
		t.Fatalf("sessions: %+v", infos)
	}
	hasModelDrift := false
	hasExhausted := false
	for _, warn := range infos[0].Warnings {
		if strings.Contains(warn, "model drift") {
			hasModelDrift = true
		}
		if strings.Contains(warn, "exhausted") {
			hasExhausted = true
		}
	}
	if !hasModelDrift || !hasExhausted {
		t.Fatalf("warnings missing (modelDrift=%v exhausted=%v): %v", hasModelDrift, hasExhausted, infos[0].Warnings)
	}
}

func TestReplayMissingCassette404(t *testing.T) {
	sim := newTestSim(t, true)
	gate := sim.Gate(failingNext(t))
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBody("m", "x", false),
		map[string]string{"x-api-key": "sim-replay:nope"}))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d, want 404", w.Code)
	}
}

func TestReplayRecordedErrorVerbatim(t *testing.T) {
	sim := newTestSim(t, true)
	reqBody, _ := json.Marshal(map[string]any{
		"model": "claude-opus-4-8",
		"messages": []map[string]any{
			{"role": "user", "content": "x"},
		},
		"tools": []map[string]any{
			{"name": "Bash", "description": "shell", "input_schema": map[string]any{"type": "object"}},
		},
	})
	errBody := `{"type":"error","error":{"type":"overloaded_error","message":"recorded 529"}}`
	if err := sim.store.AppendCassetteEntry("errtape", &cassetteEntry{
		Method: "POST", Path: "/v1/messages", Status: 529,
		RequestBody:     reqBody,
		Fingerprint:     fingerprintOf(reqBody),
		ResponseHeaders: map[string]string{"content-type": "application/json"},
		ResponseBodyB64: base64.StdEncoding.EncodeToString([]byte(errBody)),
	}); err != nil {
		t.Fatal(err)
	}

	gate := sim.Gate(failingNext(t))
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBodyWithTools("claude-opus-4-8", "x", true),
		map[string]string{"x-api-key": "sim-replay:errtape"}))
	if w.Code != 529 || !strings.Contains(w.Body.String(), "recorded 529") {
		t.Fatalf("recorded error replay: %d %s", w.Code, w.Body.String())
	}
}

func TestSSEToMessageRoundTrip(t *testing.T) {
	src := testMessage()
	rec := httptest.NewRecorder()
	if err := writeMessageSSE(rec, t.Context(), src, pacing{ChunkSize: 5}); err != nil {
		t.Fatal(err)
	}
	got, err := sseToMessage(rec.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != src.ID || got.Model != src.Model || got.StopReason != src.StopReason {
		t.Fatalf("envelope mismatch: %+v", got)
	}
	if got.InputTokens != src.InputTokens || got.OutputTokens != src.OutputTokens {
		t.Fatalf("usage mismatch: %+v", got)
	}
	if len(got.Blocks) != len(src.Blocks) {
		t.Fatalf("block count %d vs %d", len(got.Blocks), len(src.Blocks))
	}
	if got.Blocks[0].Thinking != src.Blocks[0].Thinking {
		t.Fatalf("thinking: %q", got.Blocks[0].Thinking)
	}
	if got.Blocks[1].Text != src.Blocks[1].Text {
		t.Fatalf("text: %q", got.Blocks[1].Text)
	}
	var wantInput, gotInput map[string]any
	_ = json.Unmarshal(src.Blocks[2].ToolInput, &wantInput)
	if err := json.Unmarshal(got.Blocks[2].ToolInput, &gotInput); err != nil {
		t.Fatalf("tool input not reassembled: %q", got.Blocks[2].ToolInput)
	}
	if fmt.Sprint(gotInput) != fmt.Sprint(wantInput) {
		t.Fatalf("tool input: %v vs %v", gotInput, wantInput)
	}
	if got.Blocks[2].ToolName != src.Blocks[2].ToolName {
		t.Fatalf("tool name: %q", got.Blocks[2].ToolName)
	}
}

func TestTeeResponseWriterStreamsAndCaptures(t *testing.T) {
	rec := httptest.NewRecorder()
	tee := &teeResponseWriter{ResponseWriter: rec, status: http.StatusOK}

	tee.WriteHeader(http.StatusAccepted)
	_, _ = tee.Write([]byte("chunk1 "))
	tee.Flush()
	_, _ = tee.Write([]byte("chunk2"))

	if rec.Code != http.StatusAccepted || rec.Body.String() != "chunk1 chunk2" {
		t.Fatalf("passthrough broken: %d %q", rec.Code, rec.Body.String())
	}
	if tee.status != http.StatusAccepted || tee.buf.String() != "chunk1 chunk2" {
		t.Fatalf("capture broken: %d %q", tee.status, tee.buf.String())
	}
	if !rec.Flushed {
		t.Fatal("Flush not passed through")
	}
}
