package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// LLM simulator ("test bench"): serves a deterministic Anthropic Messages API
// so Claude Code / Messages-API clients can be exercised end-to-end without a
// real subscription.
//
// Mode selection is per-request via the API key: any key starting with "sim-"
// (or an explicit X-Gateway-Sim header) is served locally. Directive grammar
// (key form / header form):
//
//	sim-echo                | X-Gateway-Sim: echo
//	sim-scenario:<name>     | X-Gateway-Sim: scenario:<name>
//	sim-replay:<cassette>   | X-Gateway-Sim: replay:<cassette>
//
// SECURITY INVARIANT: a request carrying a sim key is NEVER forwarded
// upstream — the sim branch of Gate has no code path to the proxy. This holds
// even when GATEWAY_SIM_ENABLED is off (the flag gates serving, not
// recognition: disabled → local 403), so a misconfigured client can never
// leak prompt content to Anthropic under a bogus key.
//
// Deterministic token contract (assertable by OTEL/stats e2e tests):
//
//	input_tokens  = max(1, len(raw request body)/4)
//	output_tokens = max(1, word count of emitted blocks) — text words,
//	                thinking words, and compact tool_use input JSON words
//	count_tokens  = the input_tokens rule applied to the count_tokens body
const simKeyPrefix = "sim-"

const maxSimBodyBytes = 20 << 20 // request bodies larger than this get 413

const simSessionIdleSweep = time.Hour

type simDirective struct {
	Mode string // echo | scenario | replay
	Arg  string // slugified scenario/cassette name (empty for echo)
}

func (d simDirective) String() string {
	if d.Arg == "" {
		return d.Mode
	}
	return d.Mode + ":" + d.Arg
}

// simSession tracks per-session playback state. Sessions are keyed by
// directive + session identity (see sessionKeyFor).
type simSession struct {
	mu        sync.Mutex
	Key       string
	Directive string
	Index     int // main-lane /v1/messages counter
	SideIndex int // side-call lane counter
	Created   time.Time
	LastSeen  time.Time
	Warnings  []string // drift warnings, capped
}

const maxSessionWarnings = 50

func (s *simSession) addWarnings(ws []string) {
	for _, w := range ws {
		if len(s.Warnings) >= maxSessionWarnings {
			return
		}
		s.Warnings = append(s.Warnings, w)
	}
}

type simulator struct {
	store   *Store
	enabled bool

	mu       sync.Mutex
	sessions map[string]*simSession

	builtins map[string]*Scenario

	cassMu    sync.Mutex
	cassettes map[string]*cassette // parsed cache, invalidated on ModTime change
}

func newSimulator(store *Store, enabled bool) *simulator {
	return &simulator{
		store:     store,
		enabled:   enabled,
		sessions:  make(map[string]*simSession),
		builtins:  builtinScenarios(),
		cassettes: make(map[string]*cassette),
	}
}

// Gate wraps the proxy catch-all. Sim-keyed requests are served (or refused)
// locally and can never reach next; passthrough requests optionally record
// into a cassette; everything else is untouched.
func (s *simulator) Gate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if raw, ok := simKeyFromRequest(r); ok {
			if !s.enabled {
				writeAnthropicError(w, http.StatusForbidden, "permission_error",
					"gateway simulator is disabled (set GATEWAY_SIM_ENABLED=1)", 0)
				return
			}
			d, err := parseSimDirective(raw)
			if err != nil {
				writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), 0)
				return
			}
			s.serveSim(w, r, d)
			return
		}

		if name := r.Header.Get("X-Gateway-Record"); name != "" && s.enabled {
			s.recordPassthrough(w, r, slugify(name), next)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// simKeyFromRequest sniffs the sim directive, in precedence order:
// X-Gateway-Sim header → x-api-key → Authorization Bearer token.
func simKeyFromRequest(r *http.Request) (string, bool) {
	if v := strings.TrimSpace(r.Header.Get("X-Gateway-Sim")); v != "" {
		return v, true
	}
	if v := r.Header.Get("x-api-key"); v != "" {
		if rest, ok := stripSimPrefix(v); ok {
			return rest, true
		}
	}
	if tok, ok := bearerToken(r); ok {
		if rest, ok2 := stripSimPrefix(tok); ok2 {
			return rest, true
		}
	}
	return "", false
}

func stripSimPrefix(v string) (string, bool) {
	if len(v) >= len(simKeyPrefix) && strings.EqualFold(v[:len(simKeyPrefix)], simKeyPrefix) {
		return v[len(simKeyPrefix):], true
	}
	return "", false
}

func parseSimDirective(raw string) (simDirective, error) {
	mode, arg, _ := strings.Cut(raw, ":")
	mode = strings.ToLower(strings.TrimSpace(mode))
	arg = strings.TrimSpace(arg)
	switch mode {
	case "echo":
		if arg != "" {
			return simDirective{}, fmt.Errorf("sim directive %q: echo takes no argument", raw)
		}
		return simDirective{Mode: "echo"}, nil
	case "scenario", "replay":
		slug := slugify(arg)
		if slug == "" {
			return simDirective{}, fmt.Errorf("sim directive %q needs a name, e.g. sim-%s:tool-loop-then-stop", raw, mode)
		}
		return simDirective{Mode: mode, Arg: slug}, nil
	default:
		return simDirective{}, fmt.Errorf(
			"unknown sim directive %q (expected echo, scenario:<name>, or replay:<cassette>)", raw)
	}
}

// serveSim dispatches a sim-keyed request. Anything unrecognised gets a local
// 404 — by construction there is no path to the upstream proxy from here.
func (s *simulator) serveSim(w http.ResponseWriter, r *http.Request, d simDirective) {
	// The www production-floor route posts to {endpoint}/anthropic/v1/messages;
	// accept that alias for sim traffic.
	path := strings.TrimPrefix(r.URL.Path, "/anthropic")
	switch {
	case r.Method == http.MethodPost && path == "/v1/messages":
		s.serveMessages(w, r, d)
	case r.Method == http.MethodPost && path == "/v1/messages/count_tokens":
		s.serveCountTokens(w, r)
	default:
		writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			fmt.Sprintf("gateway sim: unknown endpoint %s %s", r.Method, r.URL.Path), 0)
	}
}

// --- Messages API request model (parsed subset) ---

type anthropicRequest struct {
	Model     string          `json:"model"`
	MaxTokens int             `json:"max_tokens"`
	Stream    bool            `json:"stream"`
	System    json.RawMessage `json:"system"` // string or []block — kept raw
	Messages  []anthropicMsg  `json:"messages"`
	Tools     []anthropicTool `json:"tools"`
	Metadata  struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

type anthropicMsg struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // string or []block
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// text extracts the concatenated text of a message: plain string content, or
// text blocks plus tool_result text.
func (m anthropicMsg) text() string {
	var s string
	if json.Unmarshal(m.Content, &s) == nil {
		return s
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Content json.RawMessage `json:"content"` // tool_result: string or []block
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	var b strings.Builder
	for _, bl := range blocks {
		switch bl.Type {
		case "text":
			b.WriteString(bl.Text)
		case "tool_result":
			var cs string
			if json.Unmarshal(bl.Content, &cs) == nil {
				b.WriteString(cs)
				continue
			}
			var inner []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(bl.Content, &inner) == nil {
				for _, ib := range inner {
					if ib.Type == "text" {
						b.WriteString(ib.Text)
					}
				}
			}
		}
	}
	return b.String()
}

// lastUserText returns the text of the last user-role message.
func lastUserText(req *anthropicRequest) string {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			if t := strings.TrimSpace(req.Messages[i].text()); t != "" {
				return t
			}
		}
	}
	return "hello from gateway sim"
}

// lastMessageText returns the text of the final message regardless of role
// (used by drift matchers, which may target tool_results).
func lastMessageText(req *anthropicRequest) string {
	if len(req.Messages) == 0 {
		return ""
	}
	return req.Messages[len(req.Messages)-1].text()
}

var errBodyTooLarge = errors.New("request body too large")

// parseAnthropicRequest reads and parses a Messages API request body, capped
// at maxSimBodyBytes.
func parseAnthropicRequest(r *http.Request) (*anthropicRequest, []byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSimBodyBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxSimBodyBytes {
		return nil, nil, errBodyTooLarge
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, nil, fmt.Errorf("invalid request JSON: %w", err)
	}
	return &req, body, nil
}

// --- Session identity ---

// sessionKeyFor resolves the session identity, first hit wins:
//  1. X-Gateway-Sim-Session header (explicit, for tests/curl)
//  2. metadata.user_id (Claude Code sets it on every call — it embeds the
//     session UUID, so main-loop and haiku side-calls share one session)
//  3. FNV-1a of the first user message (stable: conversations grow by append)
//  4. "default"
func sessionKeyFor(r *http.Request, req *anthropicRequest) string {
	if v := strings.TrimSpace(r.Header.Get("X-Gateway-Sim-Session")); v != "" {
		return v
	}
	if req.Metadata.UserID != "" {
		return req.Metadata.UserID
	}
	for _, m := range req.Messages {
		if m.Role == "user" {
			h := fnv.New64a()
			_, _ = h.Write([]byte(m.text()))
			return fmt.Sprintf("conv-%x", h.Sum64())
		}
	}
	return "default"
}

// session returns (creating if needed) the session for directive+key, and
// opportunistically sweeps idle sessions.
func (s *simulator) session(d simDirective, key string) *simSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := d.String() + "|" + key
	now := time.Now().UTC()
	if sess, ok := s.sessions[id]; ok {
		sess.LastSeen = now
		return sess
	}
	for k, v := range s.sessions {
		if now.Sub(v.LastSeen) > simSessionIdleSweep {
			delete(s.sessions, k)
		}
	}
	sess := &simSession{Key: key, Directive: d.String(), Created: now, LastSeen: now}
	s.sessions[id] = sess
	return sess
}

// simSessionInfo is the /api/testbench/sessions projection.
type simSessionInfo struct {
	Key       string    `json:"key"`
	Directive string    `json:"directive"`
	Index     int       `json:"index"`
	SideIndex int       `json:"sideIndex"`
	Created   time.Time `json:"created"`
	LastSeen  time.Time `json:"lastSeen"`
	Warnings  []string  `json:"warnings,omitempty"`
}

func (s *simulator) snapshotSessions() []simSessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	infos := make([]simSessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sess.mu.Lock()
		infos = append(infos, simSessionInfo{
			Key: sess.Key, Directive: sess.Directive,
			Index: sess.Index, SideIndex: sess.SideIndex,
			Created: sess.Created, LastSeen: sess.LastSeen,
			Warnings: append([]string(nil), sess.Warnings...),
		})
		sess.mu.Unlock()
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Created.Before(infos[j].Created) })
	return infos
}

// resetSessions drops all sessions, or just those whose Key matches key.
// Returns the number removed.
func (s *simulator) resetSessions(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	removed := 0
	for id, sess := range s.sessions {
		if key == "" || sess.Key == key {
			delete(s.sessions, id)
			removed++
		}
	}
	return removed
}

// --- Token contract helpers ---

func estimateInputTokens(body []byte) int {
	n := len(body) / 4
	if n < 1 {
		n = 1
	}
	return n
}

func countOutputTokens(blocks []simBlock) int {
	n := 0
	for _, b := range blocks {
		switch b.Type {
		case "thinking":
			n += len(strings.Fields(b.Thinking))
		case "tool_use":
			if compact, err := compactJSON(orEmptyObject(b.ToolInput)); err == nil {
				n += len(strings.Fields(compact))
			}
		default:
			n += len(strings.Fields(b.Text))
		}
	}
	if n < 1 {
		n = 1
	}
	return n
}

func orEmptyObject(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("{}")
	}
	return raw
}

func simMessageID(sessionKey string, idx int) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(sessionKey))
	return fmt.Sprintf("msg_sim_%x_%d", h.Sum64(), idx)
}

// writeAnthropicError writes an Anthropic-shaped error response, optionally
// with a retry-after header (429s).
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string, retryAfterSec int) {
	w.Header().Set("Content-Type", "application/json")
	if retryAfterSec > 0 {
		w.Header().Set("retry-after", fmt.Sprintf("%d", retryAfterSec))
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}

// --- /v1/messages dispatch ---

func (s *simulator) serveMessages(w http.ResponseWriter, r *http.Request, d simDirective) {
	req, rawBody, err := parseAnthropicRequest(r)
	if err != nil {
		if errors.Is(err, errBodyTooLarge) {
			writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
				fmt.Sprintf("gateway sim: request body exceeds %d bytes", maxSimBodyBytes), 0)
			return
		}
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error(), 0)
		return
	}

	switch d.Mode {
	case "echo":
		s.serveEcho(w, r, d, req, rawBody)
	case "scenario":
		s.serveScenario(w, r, d, req, rawBody)
	case "replay":
		s.serveReplay(w, r, d, req, rawBody)
	}
}

// serveEcho replies with the last user text and end_turn — the zero-config
// probe mode. Sessions are still tracked so /api/testbench/sessions shows the
// traffic.
func (s *simulator) serveEcho(w http.ResponseWriter, r *http.Request, d simDirective, req *anthropicRequest, rawBody []byte) {
	sess := s.session(d, sessionKeyFor(r, req))
	sess.mu.Lock()
	idx := sess.Index
	sess.Index++
	sess.mu.Unlock()

	blocks := []simBlock{{Type: "text", Text: lastUserText(req)}}
	msg := simMessage{
		ID:           simMessageID(sess.Key, idx),
		Model:        req.Model,
		StopReason:   "end_turn",
		Blocks:       blocks,
		InputTokens:  estimateInputTokens(rawBody),
		OutputTokens: countOutputTokens(blocks),
	}
	s.emit(w, r, req.Stream, msg, pacing{})
}

// serveScenario plays the scripted scenario for this session.
func (s *simulator) serveScenario(w http.ResponseWriter, r *http.Request, d simDirective, req *anthropicRequest, rawBody []byte) {
	sc, err := s.loadScenario(d.Arg)
	if err != nil {
		writeAnthropicError(w, http.StatusNotFound, "not_found_error",
			fmt.Sprintf("gateway sim: scenario %q not found — PUT /api/testbench/scenarios/%s first, or use a builtin (echo, tool-loop-then-stop, rate-limit-then-succeed)", d.Arg, d.Arg), 0)
		return
	}

	sess := s.session(d, sessionKeyFor(r, req))

	// Side-call lane: utility requests (session titles, haiku calls — see
	// SidecallSpec) get a canned reply and never consume main-lane steps.
	if sc.isSidecall(req) {
		sess.mu.Lock()
		sideIdx := sess.SideIndex
		sess.SideIndex++
		sess.mu.Unlock()
		blocks := []simBlock{{Type: "text", Text: sc.sidecallText()}}
		msg := simMessage{
			ID:           simMessageID(sess.Key+"-side", sideIdx),
			Model:        req.Model,
			StopReason:   "end_turn",
			Blocks:       blocks,
			InputTokens:  estimateInputTokens(rawBody),
			OutputTokens: countOutputTokens(blocks),
		}
		s.emit(w, r, req.Stream, msg, pacing{})
		return
	}

	sess.mu.Lock()
	idx := sess.Index
	sess.Index++
	step, exhausted := stepForIndex(sc, idx)
	var warnings []string
	if !exhausted {
		warnings = driftWarnings(step, req, idx)
		sess.addWarnings(warnings)
	}
	sess.mu.Unlock()

	for _, warn := range warnings {
		log.Printf("sim scenario %q session %q drift: %s", sc.Name, sess.Key, warn)
	}

	if exhausted {
		switch sc.OnExhausted {
		case "last":
			step = &sc.Steps[len(sc.Steps)-1]
		case "error":
			writeAnthropicError(w, http.StatusInternalServerError, "api_error",
				fmt.Sprintf("gateway sim: scenario %q exhausted after %d steps (onExhausted=error)", sc.Name, len(sc.Steps)), 0)
			return
		default: // end_turn
			sess.mu.Lock()
			sess.addWarnings([]string{fmt.Sprintf("request %d: scenario exhausted (%d steps)", idx, len(sc.Steps))})
			sess.mu.Unlock()
			log.Printf("sim scenario %q session %q exhausted at request %d", sc.Name, sess.Key, idx)
			blocks := []simBlock{{Type: "text", Text: fmt.Sprintf("[sim] scenario %q exhausted", sc.Name)}}
			msg := simMessage{
				ID: simMessageID(sess.Key, idx), Model: req.Model, StopReason: "end_turn",
				Blocks: blocks, InputTokens: estimateInputTokens(rawBody), OutputTokens: countOutputTokens(blocks),
			}
			s.emit(w, r, req.Stream, msg, pacing{})
			return
		}
	}

	if spec := step.Response.Error; spec != nil {
		if spec.AfterChunks > 0 && req.Stream {
			partial := simMessage{
				ID: simMessageID(sess.Key, idx), Model: req.Model,
				InputTokens: estimateInputTokens(rawBody),
			}
			_ = writeMidStreamErrorSSE(w, r.Context(), partial, spec.AfterChunks, spec, pacing{})
			return
		}
		writeAnthropicError(w, spec.Status, spec.errorType(), spec.Message, spec.RetryAfterSeconds)
		return
	}

	msg, pace := buildScenarioMessage(step.Response.Message, req, rawBody, simMessageID(sess.Key, idx), idx)
	s.emit(w, r, req.Stream, msg, pace)
}

// emit writes msg as SSE or plain JSON depending on the request's stream flag.
func (s *simulator) emit(w http.ResponseWriter, r *http.Request, stream bool, msg simMessage, p pacing) {
	if stream {
		_ = writeMessageSSE(w, r.Context(), msg, p)
		return
	}
	writeMessageJSON(w, msg)
}

// serveCountTokens returns a deterministic token count for any body — it
// never consumes session state.
func (s *simulator) serveCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxSimBodyBytes+1))
	if err != nil || len(body) > maxSimBodyBytes {
		writeAnthropicError(w, http.StatusRequestEntityTooLarge, "request_too_large",
			"gateway sim: count_tokens body too large", 0)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"input_tokens": estimateInputTokens(body)})
}

// loadScenario resolves a scenario by name: stored file first, builtin second.
func (s *simulator) loadScenario(name string) (*Scenario, error) {
	data, err := s.store.ReadScenarioFile(name)
	if err == nil {
		var sc Scenario
		if err := json.Unmarshal(data, &sc); err != nil {
			return nil, fmt.Errorf("scenario %q: invalid JSON: %w", name, err)
		}
		if err := sc.Validate(); err != nil {
			return nil, fmt.Errorf("scenario %q: %w", name, err)
		}
		return &sc, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	if sc, ok := s.builtins[name]; ok {
		return sc, nil
	}
	return nil, os.ErrNotExist
}
