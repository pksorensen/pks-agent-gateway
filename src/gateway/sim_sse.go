package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Shape-faithful Anthropic Messages API wire emitter for the simulator.
//
// The streaming sequence mirrors the real API exactly so both Claude Code and
// the Vercel AI SDK accept it:
//
//	message_start → ping → per block (content_block_start,
//	content_block_delta…, content_block_stop) → message_delta (stop_reason +
//	usage) → message_stop
//
// Every event is flushed immediately; deltas are chunked on rune boundaries so
// each data line is valid UTF-8 JSON.

// simBlock is one content block of a simulated assistant message.
type simBlock struct {
	Type      string // "text" | "thinking" | "tool_use"
	Text      string
	Thinking  string
	Signature string // thinking only; defaults to "sim-signature"
	ToolID    string
	ToolName  string
	ToolInput json.RawMessage
}

// simMessage is a fully-resolved simulated assistant message, ready to emit as
// SSE or plain JSON.
type simMessage struct {
	ID           string
	Model        string
	StopReason   string
	Blocks       []simBlock
	InputTokens  int
	OutputTokens int
}

// pacing controls how a streamed message is chunked and delayed.
type pacing struct {
	ChunkSize        int // runes per delta; default 16
	DelayMs          int // sleep per delta; capped at 2000
	PingEveryNChunks int // 0 = only the initial ping
}

func (p pacing) normalized() pacing {
	if p.ChunkSize <= 0 {
		p.ChunkSize = 16
	}
	if p.DelayMs < 0 {
		p.DelayMs = 0
	}
	if p.DelayMs > 2000 {
		p.DelayMs = 2000
	}
	return p
}

// sseUsage is the usage object embedded in message_start / message_delta /
// non-streaming messages. Cache fields are always present (and zero) to match
// the real API's shape.
type sseUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func writeSSEHeaders(w http.ResponseWriter, msgID string) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	h.Set("request-id", "req_"+msgID)
}

// writeSSEEvent writes one "event: X\ndata: {…}\n\n" frame and flushes.
func writeSSEEvent(w io.Writer, f http.Flusher, event string, data any) error {
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// chunkRunes splits s into chunks of at most n runes, preserving UTF-8
// boundaries so each chunk is independently valid inside a JSON string.
func chunkRunes(s string, n int) []string {
	if s == "" {
		return nil
	}
	runes := []rune(s)
	var chunks []string
	for i := 0; i < len(runes); i += n {
		end := i + n
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

// pacingSleep sleeps for the configured delay, aborting early if ctx is done.
// Returns false when the client has disconnected.
func pacingSleep(ctx context.Context, delayMs int) bool {
	if delayMs <= 0 {
		return ctx.Err() == nil
	}
	select {
	case <-ctx.Done():
		return false
	case <-time.After(time.Duration(delayMs) * time.Millisecond):
		return true
	}
}

// jsonBlock renders a simBlock as its final (non-streaming) JSON form.
func jsonBlock(b simBlock) map[string]any {
	switch b.Type {
	case "thinking":
		sig := b.Signature
		if sig == "" {
			sig = "sim-signature"
		}
		return map[string]any{"type": "thinking", "thinking": b.Thinking, "signature": sig}
	case "tool_use":
		input := b.ToolInput
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return map[string]any{"type": "tool_use", "id": b.ToolID, "name": b.ToolName, "input": input}
	default:
		return map[string]any{"type": "text", "text": b.Text}
	}
}

// zeroBlock renders the empty content_block_start form of a simBlock.
func zeroBlock(b simBlock) map[string]any {
	switch b.Type {
	case "thinking":
		return map[string]any{"type": "thinking", "thinking": "", "signature": ""}
	case "tool_use":
		return map[string]any{"type": "tool_use", "id": b.ToolID, "name": b.ToolName, "input": map[string]any{}}
	default:
		return map[string]any{"type": "text", "text": ""}
	}
}

func messageStartPayload(msg simMessage) map[string]any {
	return map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msg.ID,
			"type":          "message",
			"role":          "assistant",
			"model":         msg.Model,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         sseUsage{InputTokens: msg.InputTokens, OutputTokens: 1},
		},
	}
}

// writeMessageSSE emits msg as a complete Anthropic streaming response.
// Client disconnects (ctx canceled) abort silently: the response is already
// underway, and the session cursor advanced at request receipt.
func writeMessageSSE(w http.ResponseWriter, ctx context.Context, msg simMessage, p pacing) error {
	p = p.normalized()
	flusher, _ := w.(http.Flusher)
	writeSSEHeaders(w, msg.ID)
	w.WriteHeader(http.StatusOK)

	if err := writeSSEEvent(w, flusher, "message_start", messageStartPayload(msg)); err != nil {
		return err
	}
	if err := writeSSEEvent(w, flusher, "ping", map[string]any{"type": "ping"}); err != nil {
		return err
	}

	chunksSincePing := 0
	emitDelta := func(index int, delta map[string]any) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": index, "delta": delta,
		}); err != nil {
			return err
		}
		chunksSincePing++
		if p.PingEveryNChunks > 0 && chunksSincePing >= p.PingEveryNChunks {
			chunksSincePing = 0
			if err := writeSSEEvent(w, flusher, "ping", map[string]any{"type": "ping"}); err != nil {
				return err
			}
		}
		if !pacingSleep(ctx, p.DelayMs) {
			return ctx.Err()
		}
		return nil
	}

	for i, b := range msg.Blocks {
		if err := writeSSEEvent(w, flusher, "content_block_start", map[string]any{
			"type": "content_block_start", "index": i, "content_block": zeroBlock(b),
		}); err != nil {
			return err
		}

		switch b.Type {
		case "thinking":
			for _, c := range chunkRunes(b.Thinking, p.ChunkSize) {
				if err := emitDelta(i, map[string]any{"type": "thinking_delta", "thinking": c}); err != nil {
					return err
				}
			}
			sig := b.Signature
			if sig == "" {
				sig = "sim-signature"
			}
			if err := emitDelta(i, map[string]any{"type": "signature_delta", "signature": sig}); err != nil {
				return err
			}
		case "tool_use":
			input := b.ToolInput
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			compact, err := compactJSON(input)
			if err != nil {
				return err
			}
			for _, c := range chunkRunes(compact, p.ChunkSize) {
				if err := emitDelta(i, map[string]any{"type": "input_json_delta", "partial_json": c}); err != nil {
					return err
				}
			}
		default:
			for _, c := range chunkRunes(b.Text, p.ChunkSize) {
				if err := emitDelta(i, map[string]any{"type": "text_delta", "text": c}); err != nil {
					return err
				}
			}
		}

		if err := writeSSEEvent(w, flusher, "content_block_stop", map[string]any{
			"type": "content_block_stop", "index": i,
		}); err != nil {
			return err
		}
	}

	if err := writeSSEEvent(w, flusher, "message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": msg.StopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": msg.OutputTokens},
	}); err != nil {
		return err
	}
	return writeSSEEvent(w, flusher, "message_stop", map[string]any{"type": "message_stop"})
}

// writeMessageJSON emits msg as a non-streaming Messages API response.
func writeMessageJSON(w http.ResponseWriter, msg simMessage) {
	content := make([]any, 0, len(msg.Blocks))
	for _, b := range msg.Blocks {
		content = append(content, jsonBlock(b))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("request-id", "req_"+msg.ID)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id":            msg.ID,
		"type":          "message",
		"role":          "assistant",
		"model":         msg.Model,
		"content":       content,
		"stop_reason":   msg.StopReason,
		"stop_sequence": nil,
		"usage":         sseUsage{InputTokens: msg.InputTokens, OutputTokens: msg.OutputTokens},
	})
}

// writeMidStreamErrorSSE starts a streaming response, emits afterChunks text
// deltas of filler output, then injects an SSE error event and ends the stream
// — exercising client stream-error paths the way a real mid-generation
// overload does.
func writeMidStreamErrorSSE(w http.ResponseWriter, ctx context.Context, msg simMessage, afterChunks int, spec *ErrorSpec, p pacing) error {
	p = p.normalized()
	flusher, _ := w.(http.Flusher)
	writeSSEHeaders(w, msg.ID)
	w.WriteHeader(http.StatusOK)

	if err := writeSSEEvent(w, flusher, "message_start", messageStartPayload(msg)); err != nil {
		return err
	}
	if err := writeSSEEvent(w, flusher, "content_block_start", map[string]any{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]any{"type": "text", "text": ""},
	}); err != nil {
		return err
	}
	for i := 0; i < afterChunks; i++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := writeSSEEvent(w, flusher, "content_block_delta", map[string]any{
			"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": fmt.Sprintf("[sim partial %d] ", i)},
		}); err != nil {
			return err
		}
		if !pacingSleep(ctx, p.DelayMs) {
			return ctx.Err()
		}
	}
	return writeSSEEvent(w, flusher, "error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": spec.errorType(), "message": spec.Message},
	})
}

// compactJSON renders raw JSON in its compact form so input_json_delta chunks
// concatenate to a deterministic byte sequence.
func compactJSON(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", fmt.Errorf("tool input is not valid JSON: %w", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
