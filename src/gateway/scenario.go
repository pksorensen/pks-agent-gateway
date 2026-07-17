package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Scripted scenario engine for the simulator.
//
// A scenario is a JSON document (stored at
// {dataDir}/owners/{owner}/testbench/scenarios/{name}.json, managed via
// /api/testbench/scenarios) describing the sequence of responses the sim
// serves to a session. Step selection is strictly sequence-based — the Nth
// main-lane request gets the step covering index N — because Claude Code's
// requests are nondeterministic in their details (dates, cwd, git status in
// the system prompt). Per-step matchers are drift ASSERTIONS: a mismatch logs
// a warning on the session (inspectable via /api/testbench/sessions) but
// still serves the step.

const defaultSidecallRegex = `(?i)haiku`

// Scenario is the on-disk scripted-scenario document.
type Scenario struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// OnExhausted controls behavior when a session advances past the last
	// step: "end_turn" (default — serve a synthetic exhausted notice and end
	// the turn), "last" (keep re-serving the final step), or "error" (HTTP 500).
	OnExhausted string        `json:"onExhausted,omitempty"`
	Sidecall    *SidecallSpec `json:"sidecall,omitempty"`
	Steps       []Step        `json:"steps"`
}

// SidecallSpec routes utility requests to a canned response lane that never
// consumes main scenario steps. Two signals classify a side-call, either one
// matches: the model (haiku by default), or the request advertising NO tools
// — Claude Code's session-title/summary calls use the MAIN model but never
// send a tools array, while every real agent-loop request does (verified
// against claude 2.1.207).
type SidecallSpec struct {
	ModelRegex string `json:"modelRegex,omitempty"` // default (?i)haiku
	Text       string `json:"text,omitempty"`       // default "ok"
	// Toolless controls the no-tools-means-sidecall heuristic; default true.
	// Set false for clients whose MAIN requests legitimately carry no tools.
	Toolless *bool `json:"toolless,omitempty"`
}

// Step serves `repeat` consecutive main-lane requests (default 1, -1 =
// forever) with one response.
type Step struct {
	Match    *StepMatch   `json:"match,omitempty"`
	Repeat   int          `json:"repeat,omitempty"`
	Response StepResponse `json:"response"`
}

// StepMatch is a drift assertion, not a router — see package comment above.
type StepMatch struct {
	RequestIndex        *int   `json:"requestIndex,omitempty"`
	LastMessageContains string `json:"lastMessageContains,omitempty"`
	ModelRegex          string `json:"modelRegex,omitempty"`
}

// StepResponse holds exactly one of Message or Error.
type StepResponse struct {
	Message *MessageSpec `json:"message,omitempty"`
	Error   *ErrorSpec   `json:"error,omitempty"`
}

// MessageSpec describes a simulated assistant message.
type MessageSpec struct {
	Content    []BlockSpec `json:"content"`
	StopReason string      `json:"stopReason,omitempty"` // auto: tool_use if any tool_use block, else end_turn
	Model      string      `json:"model,omitempty"`      // default: echo the request model
	Usage      *UsageSpec  `json:"usage,omitempty"`      // 0 = deterministic auto (see token contract in sim.go)
	// Streaming pacing:
	ChunkSize        int `json:"chunkSize,omitempty"`        // runes per delta, default 16
	DelayMs          int `json:"delayMs,omitempty"`          // per-delta sleep, capped 2000
	PingEveryNChunks int `json:"pingEveryNChunks,omitempty"` // 0 = only the initial ping
}

// BlockSpec is one content block. Substitutions resolved per request in
// Text/Thinking/Name: $lastUserText, $model, $requestIndex, and
// $toolMatching:<substr> (Name only — resolves to the first request tool whose
// name contains the substring, falling back to the substring itself).
type BlockSpec struct {
	Type     string          `json:"type"` // text | thinking | tool_use
	Text     string          `json:"text,omitempty"`
	Thinking string          `json:"thinking,omitempty"`
	Name     string          `json:"name,omitempty"`  // tool_use
	Input    json.RawMessage `json:"input,omitempty"` // tool_use
}

// ErrorSpec injects an HTTP error (or, with AfterChunks > 0 on a streaming
// request, a mid-stream SSE error after N text chunks).
type ErrorSpec struct {
	Status            int    `json:"status"`
	Type              string `json:"type,omitempty"` // default derived from Status
	Message           string `json:"message"`
	RetryAfterSeconds int    `json:"retryAfterSeconds,omitempty"`
	AfterChunks       int    `json:"afterChunks,omitempty"`
}

// UsageSpec overrides the deterministic token accounting; 0 = auto.
type UsageSpec struct {
	InputTokens  int `json:"inputTokens,omitempty"`
	OutputTokens int `json:"outputTokens,omitempty"`
}

var validErrorStatuses = map[int]bool{
	400: true, 401: true, 403: true, 404: true, 413: true,
	429: true, 500: true, 529: true,
}

// errorType returns the explicit error type or one derived from the status,
// matching the real API's taxonomy.
func (e *ErrorSpec) errorType() string {
	if e.Type != "" {
		return e.Type
	}
	switch e.Status {
	case 400:
		return "invalid_request_error"
	case 401:
		return "authentication_error"
	case 403:
		return "permission_error"
	case 404:
		return "not_found_error"
	case 413:
		return "request_too_large"
	case 429:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}

// Validate checks structural correctness; used by both file loads and the PUT
// API so invalid documents are rejected with actionable messages.
func (sc *Scenario) Validate() error {
	if sc.Name == "" {
		return fmt.Errorf("scenario name is required")
	}
	if sc.Name != slugify(sc.Name) {
		return fmt.Errorf("scenario name %q must be a slug (lowercase alphanumerics and dashes)", sc.Name)
	}
	switch sc.OnExhausted {
	case "", "end_turn", "last", "error":
	default:
		return fmt.Errorf("onExhausted must be one of end_turn|last|error, got %q", sc.OnExhausted)
	}
	if sc.Sidecall != nil && sc.Sidecall.ModelRegex != "" {
		if _, err := regexp.Compile(sc.Sidecall.ModelRegex); err != nil {
			return fmt.Errorf("sidecall.modelRegex: %w", err)
		}
	}
	if len(sc.Steps) == 0 {
		return fmt.Errorf("scenario needs at least one step")
	}
	for i, st := range sc.Steps {
		if st.Repeat < -1 {
			return fmt.Errorf("step %d: repeat must be >= -1", i)
		}
		hasMsg := st.Response.Message != nil
		hasErr := st.Response.Error != nil
		if hasMsg == hasErr {
			return fmt.Errorf("step %d: response needs exactly one of message or error", i)
		}
		if st.Match != nil && st.Match.ModelRegex != "" {
			if _, err := regexp.Compile(st.Match.ModelRegex); err != nil {
				return fmt.Errorf("step %d: match.modelRegex: %w", i, err)
			}
		}
		if hasMsg {
			m := st.Response.Message
			if len(m.Content) == 0 {
				return fmt.Errorf("step %d: message.content must not be empty", i)
			}
			switch m.StopReason {
			case "", "end_turn", "tool_use", "max_tokens", "stop_sequence":
			default:
				return fmt.Errorf("step %d: unknown stopReason %q", i, m.StopReason)
			}
			for j, b := range m.Content {
				switch b.Type {
				case "text":
					// empty text is allowed (zero-delta block)
				case "thinking":
					if b.Thinking == "" {
						return fmt.Errorf("step %d block %d: thinking block needs thinking text", i, j)
					}
				case "tool_use":
					if b.Name == "" {
						return fmt.Errorf("step %d block %d: tool_use block needs a name", i, j)
					}
					if len(b.Input) > 0 && !json.Valid(b.Input) {
						return fmt.Errorf("step %d block %d: tool_use input is not valid JSON", i, j)
					}
				default:
					return fmt.Errorf("step %d block %d: unknown block type %q", i, j, b.Type)
				}
			}
		}
		if hasErr {
			e := st.Response.Error
			if !validErrorStatuses[e.Status] {
				return fmt.Errorf("step %d: error.status %d not in allowed set (400,401,403,404,413,429,500,529)", i, e.Status)
			}
			if e.Message == "" {
				return fmt.Errorf("step %d: error.message is required", i)
			}
			if e.AfterChunks < 0 {
				return fmt.Errorf("step %d: error.afterChunks must be >= 0", i)
			}
		}
	}
	return nil
}

// sidecallRegex returns the compiled sidecall matcher for the scenario
// (default haiku). Validation guarantees it compiles.
func (sc *Scenario) sidecallRegex() *regexp.Regexp {
	pattern := defaultSidecallRegex
	if sc.Sidecall != nil && sc.Sidecall.ModelRegex != "" {
		pattern = sc.Sidecall.ModelRegex
	}
	return regexp.MustCompile(pattern)
}

func (sc *Scenario) sidecallText() string {
	if sc.Sidecall != nil && sc.Sidecall.Text != "" {
		return sc.Sidecall.Text
	}
	return "ok"
}

// isSidecall classifies a request into the side-call lane: toolless request
// (unless opted out) or sidecall-model match.
func (sc *Scenario) isSidecall(req *anthropicRequest) bool {
	toolless := true
	if sc.Sidecall != nil && sc.Sidecall.Toolless != nil {
		toolless = *sc.Sidecall.Toolless
	}
	if toolless && len(req.Tools) == 0 {
		return true
	}
	return sc.sidecallRegex().MatchString(req.Model)
}

// stepForIndex maps a main-lane request index to the step that serves it,
// honoring per-step repeat counts. exhausted is true when idx is past the end.
func stepForIndex(sc *Scenario, idx int) (step *Step, exhausted bool) {
	cum := 0
	for i := range sc.Steps {
		rep := sc.Steps[i].Repeat
		if rep == 0 {
			rep = 1
		}
		if rep < 0 || idx < cum+rep {
			return &sc.Steps[i], false
		}
		cum += rep
	}
	return nil, true
}

// driftWarnings evaluates a step's match assertions against the request.
func driftWarnings(st *Step, req *anthropicRequest, idx int) []string {
	if st.Match == nil {
		return nil
	}
	var warnings []string
	m := st.Match
	if m.RequestIndex != nil && *m.RequestIndex != idx {
		warnings = append(warnings, fmt.Sprintf("request %d: match.requestIndex expected %d", idx, *m.RequestIndex))
	}
	if m.LastMessageContains != "" && !strings.Contains(lastMessageText(req), m.LastMessageContains) {
		warnings = append(warnings, fmt.Sprintf("request %d: last message does not contain %q", idx, m.LastMessageContains))
	}
	if m.ModelRegex != "" {
		if re, err := regexp.Compile(m.ModelRegex); err == nil && !re.MatchString(req.Model) {
			warnings = append(warnings, fmt.Sprintf("request %d: model %q does not match %q", idx, req.Model, m.ModelRegex))
		}
	}
	return warnings
}

// substitute resolves the scalar placeholders in scenario text.
func substitute(s string, req *anthropicRequest, idx int) string {
	if !strings.Contains(s, "$") {
		return s
	}
	s = strings.ReplaceAll(s, "$lastUserText", lastUserText(req))
	s = strings.ReplaceAll(s, "$model", req.Model)
	s = strings.ReplaceAll(s, "$requestIndex", strconv.Itoa(idx))
	return s
}

// resolveToolName resolves "$toolMatching:<substr>" against the request's tool
// list: first tool whose name contains the substring wins; falls back to the
// substring itself so scenarios still emit a plausible tool_use when the tool
// isn't advertised.
func resolveToolName(name string, req *anthropicRequest) string {
	const prefix = "$toolMatching:"
	if !strings.HasPrefix(name, prefix) {
		return name
	}
	substr := name[len(prefix):]
	for _, t := range req.Tools {
		if strings.Contains(t.Name, substr) {
			return t.Name
		}
	}
	return substr
}

// buildScenarioMessage resolves a MessageSpec into a concrete simMessage for
// this request, applying substitutions and the deterministic token contract.
func buildScenarioMessage(spec *MessageSpec, req *anthropicRequest, rawBody []byte, msgID string, idx int) (simMessage, pacing) {
	blocks := make([]simBlock, 0, len(spec.Content))
	for j, b := range spec.Content {
		switch b.Type {
		case "thinking":
			blocks = append(blocks, simBlock{Type: "thinking", Thinking: substitute(b.Thinking, req, idx)})
		case "tool_use":
			blocks = append(blocks, simBlock{
				Type:      "tool_use",
				ToolID:    fmt.Sprintf("toolu_sim_%d_%d", idx, j),
				ToolName:  resolveToolName(b.Name, req),
				ToolInput: b.Input,
			})
		default:
			blocks = append(blocks, simBlock{Type: "text", Text: substitute(b.Text, req, idx)})
		}
	}

	stopReason := spec.StopReason
	if stopReason == "" {
		stopReason = "end_turn"
		for _, b := range blocks {
			if b.Type == "tool_use" {
				stopReason = "tool_use"
				break
			}
		}
	}

	model := spec.Model
	if model == "" {
		model = req.Model
	}

	msg := simMessage{
		ID:           msgID,
		Model:        model,
		StopReason:   stopReason,
		Blocks:       blocks,
		InputTokens:  estimateInputTokens(rawBody),
		OutputTokens: countOutputTokens(blocks),
	}
	if spec.Usage != nil {
		if spec.Usage.InputTokens > 0 {
			msg.InputTokens = spec.Usage.InputTokens
		}
		if spec.Usage.OutputTokens > 0 {
			msg.OutputTokens = spec.Usage.OutputTokens
		}
	}

	return msg, pacing{ChunkSize: spec.ChunkSize, DelayMs: spec.DelayMs, PingEveryNChunks: spec.PingEveryNChunks}
}

// builtinScenarios are compiled-in, zero-setup scenarios. A stored file with
// the same name overrides its builtin.
func builtinScenarios() map[string]*Scenario {
	intp := func(i int) *int { return &i }
	return map[string]*Scenario{
		// echo: reply with the last user text forever. Also available without
		// a scenario via the sim-echo directive.
		"echo": {
			Name:        "echo",
			Description: "Replies with the last user message text, end_turn, forever.",
			Steps: []Step{{
				Repeat: -1,
				Response: StepResponse{Message: &MessageSpec{
					Content: []BlockSpec{{Type: "text", Text: "$lastUserText"}},
				}},
			}},
		},
		// tool-loop-then-stop: one tool_use round-trip against the vibecast
		// stop_broadcast MCP tool, then end_turn — cleanly completes an ALP
		// task job end-to-end.
		"tool-loop-then-stop": {
			Name:        "tool-loop-then-stop",
			Description: "Calls the stop_broadcast MCP tool, then ends the turn after the tool_result — completes an ALP task job.",
			Steps: []Step{
				{
					Match: &StepMatch{RequestIndex: intp(0)},
					Response: StepResponse{Message: &MessageSpec{
						Content: []BlockSpec{
							{Type: "thinking", Thinking: "Simulated task is complete; stopping the broadcast."},
							{Type: "text", Text: "Task done (simulated). Stopping the broadcast now."},
							{Type: "tool_use", Name: "$toolMatching:stop_broadcast",
								Input: json.RawMessage(`{"message":"Simulated completion","conclusion":"success"}`)},
						},
						StopReason: "tool_use",
					}},
				},
				{
					Repeat: -1,
					Response: StepResponse{Message: &MessageSpec{
						Content: []BlockSpec{{Type: "text", Text: "Broadcast stopped."}},
					}},
				},
			},
		},
		// rate-limit-then-succeed: SDK auto-retry re-sends the identical
		// request, which advances the session index — that is what makes the
		// second step fire.
		"rate-limit-then-succeed": {
			Name:        "rate-limit-then-succeed",
			Description: "First request gets a 429 with retry-after; the retry succeeds.",
			Steps: []Step{
				{Response: StepResponse{Error: &ErrorSpec{
					Status: 429, Message: "simulated rate limit — retry", RetryAfterSeconds: 1,
				}}},
				{
					Repeat: -1,
					Response: StepResponse{Message: &MessageSpec{
						Content: []BlockSpec{{Type: "text", Text: "recovered after simulated rate limit"}},
					}},
				},
			},
		},
	}
}
