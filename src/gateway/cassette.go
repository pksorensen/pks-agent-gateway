package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
)

// Record & replay ("cassettes") for the simulator.
//
// Recording rides the passthrough proxy: a real-keyed request carrying
// X-Gateway-Record: <name> (wire it via ANTHROPIC_CUSTOM_HEADERS) is proxied
// normally while a teeResponseWriter captures the raw response bytes; the
// exchange is appended to {testbench}/cassettes/{name}.jsonl afterwards.
// Replay (sim-replay:<name>) serves those entries back sequence-based per
// session, with loose fingerprint checks that log drift warnings but never
// hard-fail — Claude Code's system prompts contain dates/cwd/git noise, so
// exact request matching would be brittle.

// cassetteEntry is one recorded exchange (a JSONL line).
type cassetteEntry struct {
	Seq        int       `json:"seq"`
	RecordedAt time.Time `json:"recordedAt"`
	Method     string    `json:"method"`
	Path       string    `json:"path"`
	Query      string    `json:"query,omitempty"`
	Status     int       `json:"status"`
	// RequestHeaders is ALLOWLIST ONLY — x-api-key / authorization /
	// x-gateway-* are never written to disk.
	RequestHeaders  map[string]string `json:"requestHeaders,omitempty"`
	RequestBody     json.RawMessage   `json:"requestBody,omitempty"`
	Fingerprint     *fingerprint      `json:"fingerprint,omitempty"`
	ResponseHeaders map[string]string `json:"responseHeaders,omitempty"`
	ResponseBodyB64 string            `json:"responseBodyB64"`
	SSE             bool              `json:"sse"`
}

// fingerprint is the loose request shape used for replay drift detection.
type fingerprint struct {
	Model        string   `json:"model"`
	MessageCount int      `json:"messageCount"`
	Roles        []string `json:"roles"`
	LastRole     string   `json:"lastRole"`
	HasTools     bool     `json:"hasTools"`
	Stream       bool     `json:"stream"`
}

var recordedRequestHeaders = []string{"anthropic-version", "anthropic-beta", "content-type"}
var recordedResponseHeaders = []string{"content-type"}

// teeResponseWriter streams every write straight through to the client (so
// the proxy's FlushInterval=-1 semantics are preserved) while keeping a copy
// for the cassette. It deliberately does NOT implement io.ReaderFrom, forcing
// httputil's copy loop through Write.
type teeResponseWriter struct {
	http.ResponseWriter
	status int
	buf    bytes.Buffer
}

func (t *teeResponseWriter) WriteHeader(code int) {
	t.status = code
	t.ResponseWriter.WriteHeader(code)
}

func (t *teeResponseWriter) Write(b []byte) (int, error) {
	n, err := t.ResponseWriter.Write(b)
	if n > 0 {
		t.buf.Write(b[:n])
	}
	return n, err
}

func (t *teeResponseWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recordPassthrough proxies the request via next while capturing the exchange
// into the named cassette. Recording failures never break the live request.
func (s *simulator) recordPassthrough(w http.ResponseWriter, r *http.Request, name string, next http.Handler) {
	if name == "" {
		next.ServeHTTP(w, r)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxSimBodyBytes+1))
	if err != nil || len(body) > maxSimBodyBytes {
		log.Printf("cassette %q: request body unreadable or > %d bytes — proxying without recording", name, maxSimBodyBytes)
		if err == nil {
			// Reassemble the partially-read body so the proxied request is intact.
			r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), r.Body))
			next.ServeHTTP(w, r)
		} else {
			http.Error(w, "gateway: failed to read request body", http.StatusBadRequest)
		}
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	// The recording header is gateway-internal — don't leak it upstream.
	r.Header.Del("X-Gateway-Record")

	tee := &teeResponseWriter{ResponseWriter: w, status: http.StatusOK}
	next.ServeHTTP(tee, r)

	entry := &cassetteEntry{
		RecordedAt:      time.Now().UTC(),
		Method:          r.Method,
		Path:            r.URL.Path,
		Query:           r.URL.RawQuery,
		Status:          tee.status,
		RequestHeaders:  pickHeaders(r.Header, recordedRequestHeaders),
		ResponseHeaders: pickHeaders(tee.Header(), recordedResponseHeaders),
		ResponseBodyB64: base64.StdEncoding.EncodeToString(tee.buf.Bytes()),
		SSE:             strings.Contains(tee.Header().Get("Content-Type"), "text/event-stream"),
	}
	if json.Valid(body) {
		entry.RequestBody = json.RawMessage(body)
		entry.Fingerprint = fingerprintOf(body)
	}
	if err := s.store.AppendCassetteEntry(name, entry); err != nil {
		log.Printf("cassette %q: append failed: %v", name, err)
	}
}

func pickHeaders(h http.Header, allow []string) map[string]string {
	out := map[string]string{}
	for _, k := range allow {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func fingerprintOf(body []byte) *fingerprint {
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil
	}
	fp := &fingerprint{
		Model:        req.Model,
		MessageCount: len(req.Messages),
		HasTools:     len(req.Tools) > 0,
		Stream:       req.Stream,
	}
	for _, m := range req.Messages {
		fp.Roles = append(fp.Roles, m.Role)
	}
	if n := len(req.Messages); n > 0 {
		fp.LastRole = req.Messages[n-1].Role
	}
	return fp
}

// --- Replay ---

// cassette is the parsed, lane-classified form of a cassette file.
type cassette struct {
	name    string
	modTime time.Time
	main    []cassetteEntry // /v1/messages, non-sidecall models
	side    []cassetteEntry // /v1/messages, sidecall (haiku) models
}

var replaySidecallRegex = regexp.MustCompile(defaultSidecallRegex)

// replayIsSidecall mirrors Scenario.isSidecall for the replay engine: a
// toolless request (Claude Code's title/summary utility calls) or a
// sidecall-model match lands in the side lane.
func replayIsSidecall(model string, hasTools bool) bool {
	return !hasTools || replaySidecallRegex.MatchString(model)
}

// loadCassette returns the parsed cassette, cached until the file changes.
func (s *simulator) loadCassette(name string) (*cassette, error) {
	mod, err := s.store.CassetteModTime(name)
	if err != nil {
		return nil, err
	}

	s.cassMu.Lock()
	defer s.cassMu.Unlock()
	if c, ok := s.cassettes[name]; ok && c.modTime.Equal(mod) {
		return c, nil
	}

	entries, err := s.store.ReadCassette(name)
	if err != nil {
		return nil, err
	}
	c := &cassette{name: name, modTime: mod}
	for _, e := range entries {
		path := strings.TrimPrefix(e.Path, "/anthropic")
		if path != "/v1/messages" {
			continue // count_tokens and other paths are synthesized, not replayed
		}
		model := ""
		hasTools := false
		if e.Fingerprint != nil {
			model = e.Fingerprint.Model
			hasTools = e.Fingerprint.HasTools
		}
		if replayIsSidecall(model, hasTools) {
			c.side = append(c.side, e)
		} else {
			c.main = append(c.main, e)
		}
	}
	s.cassettes[name] = c
	return c, nil
}

// serveReplay plays back the Nth recorded main-lane exchange for the Nth
// main-lane request of this session (side-calls likewise on their own lane).
func (s *simulator) serveReplay(w http.ResponseWriter, r *http.Request, d simDirective, req *anthropicRequest, rawBody []byte) {
	cas, err := s.loadCassette(d.Arg)
	if err != nil {
		if os.IsNotExist(err) {
			writeAnthropicError(w, http.StatusNotFound, "not_found_error",
				fmt.Sprintf("gateway sim: cassette %q not found — record one with the X-Gateway-Record header first", d.Arg), 0)
			return
		}
		writeAnthropicError(w, http.StatusInternalServerError, "api_error",
			fmt.Sprintf("gateway sim: cassette %q unreadable: %v", d.Arg, err), 0)
		return
	}

	sess := s.session(d, sessionKeyFor(r, req))
	isSide := replayIsSidecall(req.Model, len(req.Tools) > 0)

	sess.mu.Lock()
	var idx int
	if isSide {
		idx = sess.SideIndex
		sess.SideIndex++
	} else {
		idx = sess.Index
		sess.Index++
	}
	sess.mu.Unlock()

	lane := cas.main
	if isSide {
		lane = cas.side
	}

	if idx >= len(lane) {
		// Exhausted: side lane degrades to a canned reply, main lane ends the
		// turn with a visible notice + warning.
		if isSide {
			blocks := []simBlock{{Type: "text", Text: "ok"}}
			s.emit(w, r, req.Stream, simMessage{
				ID: simMessageID(sess.Key+"-side", idx), Model: req.Model, StopReason: "end_turn",
				Blocks: blocks, InputTokens: estimateInputTokens(rawBody), OutputTokens: countOutputTokens(blocks),
			}, pacing{})
			return
		}
		warn := fmt.Sprintf("request %d: cassette %q exhausted (%d main entries)", idx, cas.name, len(lane))
		sess.mu.Lock()
		sess.addWarnings([]string{warn})
		sess.mu.Unlock()
		log.Printf("sim replay %q session %q: %s", cas.name, sess.Key, warn)
		blocks := []simBlock{{Type: "text", Text: fmt.Sprintf("[sim] cassette %q exhausted", cas.name)}}
		s.emit(w, r, req.Stream, simMessage{
			ID: simMessageID(sess.Key, idx), Model: req.Model, StopReason: "end_turn",
			Blocks: blocks, InputTokens: estimateInputTokens(rawBody), OutputTokens: countOutputTokens(blocks),
		}, pacing{})
		return
	}

	entry := lane[idx]
	if warns := fingerprintDrift(entry.Fingerprint, fingerprintOf(rawBody), idx); len(warns) > 0 {
		sess.mu.Lock()
		sess.addWarnings(warns)
		sess.mu.Unlock()
		for _, warn := range warns {
			log.Printf("sim replay %q session %q drift: %s", cas.name, sess.Key, warn)
		}
	}

	respBytes, err := base64.StdEncoding.DecodeString(entry.ResponseBodyB64)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error",
			fmt.Sprintf("gateway sim: cassette %q entry %d: bad base64: %v", cas.name, entry.Seq, err), 0)
		return
	}

	// Recorded errors replay verbatim (status + body), whatever the caller's
	// stream flag — matching how a real error would arrive pre-stream.
	if entry.Status != http.StatusOK {
		replayVerbatim(w, entry, respBytes)
		return
	}

	// Stream-adaptation matrix.
	switch {
	case req.Stream && entry.SSE:
		replayRawSSE(w, r, entry, respBytes)
	case req.Stream && !entry.SSE:
		msg, err := messageFromJSON(respBytes)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error",
				fmt.Sprintf("gateway sim: cassette %q entry %d: %v", cas.name, entry.Seq, err), 0)
			return
		}
		_ = writeMessageSSE(w, r.Context(), msg, pacing{})
	case !req.Stream && entry.SSE:
		msg, err := sseToMessage(respBytes)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error",
				fmt.Sprintf("gateway sim: cassette %q entry %d: %v", cas.name, entry.Seq, err), 0)
			return
		}
		writeMessageJSON(w, msg)
	default:
		replayVerbatim(w, entry, respBytes)
	}
}

func fingerprintDrift(recorded, incoming *fingerprint, idx int) []string {
	if recorded == nil || incoming == nil {
		return nil
	}
	var warns []string
	drift := func(field string, rec, inc any) {
		warns = append(warns, fmt.Sprintf("request %d: %s drift — recorded %v, got %v", idx, field, rec, inc))
	}
	if recorded.Model != incoming.Model {
		drift("model", recorded.Model, incoming.Model)
	}
	if recorded.MessageCount != incoming.MessageCount {
		drift("messageCount", recorded.MessageCount, incoming.MessageCount)
	}
	if recorded.LastRole != incoming.LastRole {
		drift("lastRole", recorded.LastRole, incoming.LastRole)
	}
	if recorded.HasTools != incoming.HasTools {
		drift("hasTools", recorded.HasTools, incoming.HasTools)
	}
	return warns
}

func replayVerbatim(w http.ResponseWriter, entry cassetteEntry, body []byte) {
	ct := "application/json"
	if entry.ResponseHeaders != nil && entry.ResponseHeaders["content-type"] != "" {
		ct = entry.ResponseHeaders["content-type"]
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(entry.Status)
	_, _ = w.Write(body)
}

// replayRawSSE replays recorded SSE bytes frame-by-frame with a flush per
// frame, preserving the original event framing exactly.
func replayRawSSE(w http.ResponseWriter, r *http.Request, entry cassetteEntry, body []byte) {
	flusher, _ := w.(http.Flusher)
	ct := "text/event-stream; charset=utf-8"
	if entry.ResponseHeaders != nil && entry.ResponseHeaders["content-type"] != "" {
		ct = entry.ResponseHeaders["content-type"]
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	frames := strings.Split(string(body), "\n\n")
	for i, frame := range frames {
		if r.Context().Err() != nil {
			return
		}
		if strings.TrimSpace(frame) == "" {
			continue
		}
		suffix := "\n\n"
		if i == len(frames)-1 && !strings.HasSuffix(string(body), "\n\n") {
			suffix = ""
		}
		if _, err := io.WriteString(w, frame+suffix); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// messageFromJSON parses a non-streaming Messages API response body into a
// simMessage (for JSON→SSE adaptation).
func messageFromJSON(body []byte) (simMessage, error) {
	var raw struct {
		ID         string `json:"id"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			Thinking  string          `json:"thinking"`
			Signature string          `json:"signature"`
			ID        string          `json:"id"`
			Name      string          `json:"name"`
			Input     json.RawMessage `json:"input"`
		} `json:"content"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return simMessage{}, fmt.Errorf("recorded JSON response is not a message: %w", err)
	}
	msg := simMessage{
		ID: raw.ID, Model: raw.Model, StopReason: raw.StopReason,
		InputTokens: raw.Usage.InputTokens, OutputTokens: raw.Usage.OutputTokens,
	}
	if msg.StopReason == "" {
		msg.StopReason = "end_turn"
	}
	for _, c := range raw.Content {
		switch c.Type {
		case "text":
			msg.Blocks = append(msg.Blocks, simBlock{Type: "text", Text: c.Text})
		case "thinking":
			msg.Blocks = append(msg.Blocks, simBlock{Type: "thinking", Thinking: c.Thinking, Signature: c.Signature})
		case "tool_use":
			msg.Blocks = append(msg.Blocks, simBlock{Type: "tool_use", ToolID: c.ID, ToolName: c.Name, ToolInput: c.Input})
		}
	}
	return msg, nil
}

// sseToMessage assembles a recorded SSE stream back into the complete message
// (for SSE→JSON adaptation).
func sseToMessage(body []byte) (simMessage, error) {
	var msg simMessage
	sawStart := false

	for _, frame := range strings.Split(string(body), "\n\n") {
		var data string
		for _, line := range strings.Split(frame, "\n") {
			if strings.HasPrefix(line, "data:") {
				data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			}
		}
		if data == "" {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				ID    string `json:"id"`
				Model string `json:"model"`
				Usage struct {
					InputTokens int `json:"input_tokens"`
				} `json:"usage"`
			} `json:"message"`
			Index        int `json:"index"`
			ContentBlock struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
				Thinking    string `json:"thinking"`
				Signature   string `json:"signature"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
			Error *struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue // tolerate unknown/odd frames
		}

		switch ev.Type {
		case "message_start":
			sawStart = true
			msg.ID = ev.Message.ID
			msg.Model = ev.Message.Model
			msg.InputTokens = ev.Message.Usage.InputTokens
		case "content_block_start":
			for len(msg.Blocks) <= ev.Index {
				msg.Blocks = append(msg.Blocks, simBlock{})
			}
			b := &msg.Blocks[ev.Index]
			b.Type = ev.ContentBlock.Type
			b.ToolID = ev.ContentBlock.ID
			b.ToolName = ev.ContentBlock.Name
		case "content_block_delta":
			if ev.Index >= len(msg.Blocks) {
				continue
			}
			b := &msg.Blocks[ev.Index]
			switch ev.Delta.Type {
			case "text_delta":
				b.Text += ev.Delta.Text
			case "input_json_delta":
				b.ToolInput = append(b.ToolInput, []byte(ev.Delta.PartialJSON)...)
			case "thinking_delta":
				b.Thinking += ev.Delta.Thinking
			case "signature_delta":
				b.Signature = ev.Delta.Signature
			}
		case "message_delta":
			if ev.Delta.StopReason != "" {
				msg.StopReason = ev.Delta.StopReason
			}
			if ev.Usage.OutputTokens > 0 {
				msg.OutputTokens = ev.Usage.OutputTokens
			}
		case "error":
			return simMessage{}, fmt.Errorf("recorded stream ends in error: %s", ev.Error.Message)
		}
	}

	if !sawStart {
		return simMessage{}, fmt.Errorf("recorded SSE stream has no message_start event")
	}
	if msg.StopReason == "" {
		msg.StopReason = "end_turn"
	}
	// Empty tool inputs stream as no deltas; normalize to {}.
	for i := range msg.Blocks {
		if msg.Blocks[i].Type == "tool_use" && len(msg.Blocks[i].ToolInput) == 0 {
			msg.Blocks[i].ToolInput = json.RawMessage("{}")
		}
	}
	return msg, nil
}
