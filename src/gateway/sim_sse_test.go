package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// parseSSE splits a recorded SSE body into (event, data) pairs and fails the
// test if any data line is not valid JSON.
func parseSSE(t *testing.T, body string) []struct{ Event, Data string } {
	t.Helper()
	var events []struct{ Event, Data string }
	for _, frame := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(frame) == "" {
			continue
		}
		var ev, data string
		for _, line := range strings.Split(frame, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				ev = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			}
		}
		if ev == "" || data == "" {
			t.Fatalf("malformed SSE frame: %q", frame)
		}
		if !json.Valid([]byte(data)) {
			t.Fatalf("data line is not valid JSON: %q", data)
		}
		events = append(events, struct{ Event, Data string }{ev, data})
	}
	return events
}

func testMessage() simMessage {
	return simMessage{
		ID:         "msg_sim_test_0",
		Model:      "claude-opus-4-8",
		StopReason: "tool_use",
		Blocks: []simBlock{
			{Type: "thinking", Thinking: "pondering the simulated universe"},
			{Type: "text", Text: "Hello wørld — streaming ünïcode text!"},
			{Type: "tool_use", ToolID: "toolu_sim_0_2", ToolName: "mcp__vibecast__stop_broadcast",
				ToolInput: json.RawMessage(`{"message":"done","conclusion":"success"}`)},
		},
		InputTokens:  42,
		OutputTokens: 11,
	}
}

func TestWriteMessageSSEShape(t *testing.T) {
	w := httptest.NewRecorder()
	msg := testMessage()
	if err := writeMessageSSE(w, context.Background(), msg, pacing{ChunkSize: 8}); err != nil {
		t.Fatal(err)
	}
	if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}

	events := parseSSE(t, w.Body.String())

	// Event-type sequence: message_start, ping, then per block
	// start/deltas/stop, then message_delta, message_stop.
	if events[0].Event != "message_start" || events[1].Event != "ping" {
		t.Fatalf("prologue wrong: %v %v", events[0].Event, events[1].Event)
	}
	last, secondLast := events[len(events)-1], events[len(events)-2]
	if secondLast.Event != "message_delta" || last.Event != "message_stop" {
		t.Fatalf("epilogue wrong: %v %v", secondLast.Event, last.Event)
	}

	// message_start carries input usage and empty content.
	var start struct {
		Message struct {
			ID    string `json:"id"`
			Model string `json:"model"`
			Usage struct {
				InputTokens int `json:"input_tokens"`
			} `json:"usage"`
			Content []any `json:"content"`
		} `json:"message"`
	}
	_ = json.Unmarshal([]byte(events[0].Data), &start)
	if start.Message.ID != msg.ID || start.Message.Model != msg.Model ||
		start.Message.Usage.InputTokens != 42 || len(start.Message.Content) != 0 {
		t.Fatalf("message_start wrong: %s", events[0].Data)
	}

	// Reassemble deltas per block index and verify they concatenate exactly.
	starts := 0
	stops := 0
	text := map[int]string{}
	thinking := map[int]string{}
	partialJSON := map[int]string{}
	signatures := map[int]string{}
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			starts++
		case "content_block_stop":
			stops++
		case "content_block_delta":
			var d struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
					Signature   string `json:"signature"`
				} `json:"delta"`
			}
			_ = json.Unmarshal([]byte(e.Data), &d)
			switch d.Delta.Type {
			case "text_delta":
				text[d.Index] += d.Delta.Text
			case "thinking_delta":
				thinking[d.Index] += d.Delta.Thinking
			case "input_json_delta":
				partialJSON[d.Index] += d.Delta.PartialJSON
			case "signature_delta":
				signatures[d.Index] = d.Delta.Signature
			default:
				t.Fatalf("unknown delta type %q", d.Delta.Type)
			}
		}
	}
	if starts != 3 || stops != 3 {
		t.Fatalf("block starts/stops = %d/%d, want 3/3", starts, stops)
	}
	if thinking[0] != msg.Blocks[0].Thinking {
		t.Fatalf("thinking reassembly: %q", thinking[0])
	}
	if signatures[0] != "sim-signature" {
		t.Fatalf("signature = %q", signatures[0])
	}
	if text[1] != msg.Blocks[1].Text {
		t.Fatalf("text reassembly: %q want %q", text[1], msg.Blocks[1].Text)
	}
	var input map[string]string
	if err := json.Unmarshal([]byte(partialJSON[2]), &input); err != nil {
		t.Fatalf("partial_json chunks do not concatenate to valid JSON: %q", partialJSON[2])
	}
	if input["conclusion"] != "success" {
		t.Fatalf("tool input reassembly: %v", input)
	}

	// message_delta carries stop_reason + final output tokens.
	var delta struct {
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	_ = json.Unmarshal([]byte(secondLast.Data), &delta)
	if delta.Delta.StopReason != "tool_use" || delta.Usage.OutputTokens != 11 {
		t.Fatalf("message_delta wrong: %s", secondLast.Data)
	}
}

func TestWriteMessageSSECanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	// Must abort without panicking; error is expected and ignored.
	_ = writeMessageSSE(w, ctx, testMessage(), pacing{ChunkSize: 4, DelayMs: 5})
	if body := w.Body.String(); strings.Contains(body, "message_stop") {
		t.Fatalf("canceled stream should not complete, got: %s", body)
	}
}

func TestWriteMessageJSONRoundTrip(t *testing.T) {
	w := httptest.NewRecorder()
	writeMessageJSON(w, testMessage())

	var msg struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens              int `json:"input_tokens"`
			OutputTokens             int `json:"output_tokens"`
			CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &msg); err != nil {
		t.Fatal(err)
	}
	if msg.Type != "message" || msg.Role != "assistant" || len(msg.Content) != 3 {
		t.Fatalf("envelope: %s", w.Body.String())
	}
	if msg.Content[0].Signature != "sim-signature" {
		t.Fatalf("thinking signature missing: %s", w.Body.String())
	}
	if msg.Content[2].Name != "mcp__vibecast__stop_broadcast" {
		t.Fatalf("tool name: %s", w.Body.String())
	}
	if msg.Usage.InputTokens != 42 || msg.Usage.OutputTokens != 11 {
		t.Fatalf("usage: %+v", msg.Usage)
	}
}

func TestMidStreamErrorSSE(t *testing.T) {
	w := httptest.NewRecorder()
	spec := &ErrorSpec{Status: 529, Message: "simulated overload"}
	msg := simMessage{ID: "msg_sim_err_0", Model: "claude-opus-4-8", InputTokens: 10}
	if err := writeMidStreamErrorSSE(w, context.Background(), msg, 3, spec, pacing{}); err != nil {
		t.Fatal(err)
	}
	events := parseSSE(t, w.Body.String())

	if events[0].Event != "message_start" {
		t.Fatalf("first event %q", events[0].Event)
	}
	last := events[len(events)-1]
	if last.Event != "error" {
		t.Fatalf("last event %q, want error", last.Event)
	}
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(last.Data), &e)
	if e.Error.Type != "overloaded_error" || e.Error.Message != "simulated overload" {
		t.Fatalf("error payload: %s", last.Data)
	}
	deltas := 0
	for _, ev := range events {
		if ev.Event == "content_block_delta" {
			deltas++
		}
	}
	if deltas != 3 {
		t.Fatalf("deltas = %d, want 3", deltas)
	}
	// No message_stop after a mid-stream error.
	for _, ev := range events {
		if ev.Event == "message_stop" {
			t.Fatal("mid-stream error must not be followed by message_stop")
		}
	}
}

func TestChunkRunesUTF8Boundaries(t *testing.T) {
	s := "æøå-日本語-🎸🎸"
	chunks := chunkRunes(s, 2)
	if strings.Join(chunks, "") != s {
		t.Fatalf("chunks do not reassemble: %v", chunks)
	}
	for _, c := range chunks {
		if !json.Valid([]byte(`"` + strings.ReplaceAll(c, `"`, `\"`) + `"`)) {
			t.Fatalf("chunk %q is not valid inside a JSON string", c)
		}
	}
}
