package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func validScenario() *Scenario {
	return &Scenario{
		Name: "test-scenario",
		Steps: []Step{
			{Response: StepResponse{Message: &MessageSpec{
				Content: []BlockSpec{{Type: "text", Text: "hi"}},
			}}},
		},
	}
}

func TestScenarioValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Scenario)
		valid  bool
	}{
		{"valid minimal", func(sc *Scenario) {}, true},
		{"missing name", func(sc *Scenario) { sc.Name = "" }, false},
		{"non-slug name", func(sc *Scenario) { sc.Name = "Has Spaces" }, false},
		{"bad onExhausted", func(sc *Scenario) { sc.OnExhausted = "explode" }, false},
		{"valid onExhausted last", func(sc *Scenario) { sc.OnExhausted = "last" }, true},
		{"no steps", func(sc *Scenario) { sc.Steps = nil }, false},
		{"repeat below -1", func(sc *Scenario) { sc.Steps[0].Repeat = -2 }, false},
		{"both message and error", func(sc *Scenario) {
			sc.Steps[0].Response.Error = &ErrorSpec{Status: 429, Message: "x"}
		}, false},
		{"neither message nor error", func(sc *Scenario) {
			sc.Steps[0].Response.Message = nil
		}, false},
		{"empty content", func(sc *Scenario) {
			sc.Steps[0].Response.Message.Content = nil
		}, false},
		{"bad stop reason", func(sc *Scenario) {
			sc.Steps[0].Response.Message.StopReason = "banana"
		}, false},
		{"tool_use without name", func(sc *Scenario) {
			sc.Steps[0].Response.Message.Content = []BlockSpec{{Type: "tool_use"}}
		}, false},
		{"tool_use invalid input", func(sc *Scenario) {
			sc.Steps[0].Response.Message.Content = []BlockSpec{{Type: "tool_use", Name: "x", Input: json.RawMessage("{oops")}}
		}, false},
		{"unknown block type", func(sc *Scenario) {
			sc.Steps[0].Response.Message.Content = []BlockSpec{{Type: "video"}}
		}, false},
		{"bad error status", func(sc *Scenario) {
			sc.Steps[0].Response = StepResponse{Error: &ErrorSpec{Status: 418, Message: "x"}}
		}, false},
		{"error without message", func(sc *Scenario) {
			sc.Steps[0].Response = StepResponse{Error: &ErrorSpec{Status: 429}}
		}, false},
		{"valid error step", func(sc *Scenario) {
			sc.Steps[0].Response = StepResponse{Error: &ErrorSpec{Status: 529, Message: "overloaded"}}
		}, true},
		{"bad match regex", func(sc *Scenario) {
			sc.Steps[0].Match = &StepMatch{ModelRegex: "("}
		}, false},
		{"bad sidecall regex", func(sc *Scenario) {
			sc.Sidecall = &SidecallSpec{ModelRegex: "("}
		}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sc := validScenario()
			c.mutate(sc)
			err := sc.Validate()
			if c.valid && err != nil {
				t.Fatalf("want valid, got %v", err)
			}
			if !c.valid && err == nil {
				t.Fatal("want validation error, got nil")
			}
		})
	}
}

func TestBuiltinScenariosValidate(t *testing.T) {
	for name, sc := range builtinScenarios() {
		if err := sc.Validate(); err != nil {
			t.Errorf("builtin %q invalid: %v", name, err)
		}
	}
}

func TestStepForIndexRepeatAndExhaustion(t *testing.T) {
	sc := &Scenario{
		Name: "steps",
		Steps: []Step{
			{Repeat: 2, Response: StepResponse{Message: &MessageSpec{Content: []BlockSpec{{Type: "text", Text: "a"}}}}},
			{Response: StepResponse{Message: &MessageSpec{Content: []BlockSpec{{Type: "text", Text: "b"}}}}},
		},
	}
	wantText := func(idx int, want string) {
		step, exhausted := stepForIndex(sc, idx)
		if exhausted {
			t.Fatalf("idx %d unexpectedly exhausted", idx)
		}
		if got := step.Response.Message.Content[0].Text; got != want {
			t.Fatalf("idx %d → %q, want %q", idx, got, want)
		}
	}
	wantText(0, "a")
	wantText(1, "a")
	wantText(2, "b")
	if _, exhausted := stepForIndex(sc, 3); !exhausted {
		t.Fatal("idx 3 should be exhausted")
	}

	// repeat -1 never exhausts.
	sc.Steps[1].Repeat = -1
	if _, exhausted := stepForIndex(sc, 99); exhausted {
		t.Fatal("repeat -1 must never exhaust")
	}
}

func TestOnExhaustedModes(t *testing.T) {
	base := func(mode string) *Scenario {
		return &Scenario{
			Name:        "exhaust-" + strings.ReplaceAll(mode, "_", "-"),
			OnExhausted: mode,
			Steps: []Step{{Response: StepResponse{Message: &MessageSpec{
				Content: []BlockSpec{{Type: "text", Text: "only step"}},
			}}}},
		}
	}
	run := func(sc *Scenario) *httptest.ResponseRecorder {
		sim := newTestSim(t, true)
		data, _ := json.Marshal(sc)
		if _, err := sim.store.WriteScenarioFile(sc.Name, data); err != nil {
			t.Fatal(err)
		}
		gate := sim.Gate(failingNext(t))
		hdrs := map[string]string{
			"x-api-key":             "sim-scenario:" + sc.Name,
			"X-Gateway-Sim-Session": "s",
		}
		body := messagesBodyWithTools("claude-opus-4-8", "x", false)
		// Consume the single step, then hit the exhausted path.
		w := httptest.NewRecorder()
		gate.ServeHTTP(w, simRequest("POST", "/v1/messages", body, hdrs))
		w = httptest.NewRecorder()
		gate.ServeHTTP(w, simRequest("POST", "/v1/messages", body, hdrs))
		return w
	}

	w := run(base("end_turn"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "exhausted") {
		t.Fatalf("end_turn: %d %s", w.Code, w.Body.String())
	}

	w = run(base("last"))
	if w.Code != 200 || !strings.Contains(w.Body.String(), "only step") {
		t.Fatalf("last: %d %s", w.Code, w.Body.String())
	}

	w = run(base("error"))
	if w.Code != 500 {
		t.Fatalf("error: %d %s", w.Code, w.Body.String())
	}
}

func TestDriftWarningsStillServe(t *testing.T) {
	sim := newTestSim(t, true)
	idx0 := 5 // deliberately wrong
	sc := &Scenario{
		Name: "drifty",
		Steps: []Step{{
			Match: &StepMatch{
				RequestIndex:        &idx0,
				LastMessageContains: "never-present",
				ModelRegex:          "^claude-nonexistent$",
			},
			Repeat: -1,
			Response: StepResponse{Message: &MessageSpec{
				Content: []BlockSpec{{Type: "text", Text: "served anyway"}},
			}},
		}},
	}
	data, _ := json.Marshal(sc)
	if _, err := sim.store.WriteScenarioFile("drifty", data); err != nil {
		t.Fatal(err)
	}

	gate := sim.Gate(failingNext(t))
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, simRequest("POST", "/v1/messages", messagesBodyWithTools("claude-opus-4-8", "hello", false),
		map[string]string{"x-api-key": "sim-scenario:drifty", "X-Gateway-Sim-Session": "d"}))

	if w.Code != 200 || !strings.Contains(w.Body.String(), "served anyway") {
		t.Fatalf("drift must not block serving: %d %s", w.Code, w.Body.String())
	}
	infos := sim.snapshotSessions()
	if len(infos) != 1 || len(infos[0].Warnings) != 3 {
		t.Fatalf("want 3 drift warnings, got %+v", infos)
	}
}

func TestSubstitutions(t *testing.T) {
	req := &anthropicRequest{
		Model: "claude-opus-4-8",
		Messages: []anthropicMsg{
			{Role: "user", Content: json.RawMessage(`"the user prompt"`)},
		},
		Tools: []anthropicTool{
			{Name: "Bash"},
			{Name: "mcp__vibecast__stop_broadcast"},
		},
	}

	if got := substitute("say: $lastUserText on $model at $requestIndex", req, 3); got != "say: the user prompt on claude-opus-4-8 at 3" {
		t.Fatalf("substitute → %q", got)
	}
	if got := resolveToolName("$toolMatching:stop_broadcast", req); got != "mcp__vibecast__stop_broadcast" {
		t.Fatalf("toolMatching hit → %q", got)
	}
	if got := resolveToolName("$toolMatching:no_such_tool", req); got != "no_such_tool" {
		t.Fatalf("toolMatching fallback → %q", got)
	}
	if got := resolveToolName("LiteralTool", req); got != "LiteralTool" {
		t.Fatalf("literal name → %q", got)
	}
}

func TestMessageTextExtraction(t *testing.T) {
	// string content
	m := anthropicMsg{Role: "user", Content: json.RawMessage(`"plain"`)}
	if m.text() != "plain" {
		t.Fatalf("string content: %q", m.text())
	}
	// blocks with text + tool_result (string and block forms)
	m = anthropicMsg{Role: "user", Content: json.RawMessage(
		`[{"type":"text","text":"a "},{"type":"tool_result","tool_use_id":"t1","content":"b "},` +
			`{"type":"tool_result","tool_use_id":"t2","content":[{"type":"text","text":"c"}]}]`)}
	if m.text() != "a b c" {
		t.Fatalf("block content: %q", m.text())
	}
}
