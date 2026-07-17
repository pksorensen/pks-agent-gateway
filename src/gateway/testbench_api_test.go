package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newTestbenchServer builds the same mux wiring main.go uses (dev-mode auth,
// testbench subtree + gate with failing proxy) so routing precedence is
// exercised for real.
func newTestbenchServer(t *testing.T, enabled bool) (*httptest.Server, *simulator) {
	t.Helper()
	sim := newTestSim(t, enabled)
	auth := DevModeMiddleware()
	mux := http.NewServeMux()
	mux.Handle("/api/testbench/", auth.Require(RoleGatewayAdmin)(newTestbenchHandler(sim.store, sim)))
	mux.Handle("/", sim.Gate(failingNext(t)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, sim
}

func doJSON(t *testing.T, method, url, body string) (*http.Response, string) {
	t.Helper()
	var reader *strings.Reader = strings.NewReader(body)
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	return resp, sb.String()
}

func TestTestbenchScenarioCRUD(t *testing.T) {
	srv, _ := newTestbenchServer(t, true)

	scenario := `{
		"steps": [
			{"response": {"message": {"content": [{"type": "text", "text": "scripted"}]}}}
		]
	}`

	// PUT (create) — name comes from the URL.
	resp, body := doJSON(t, "PUT", srv.URL+"/api/testbench/scenarios/my-test", scenario)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}

	// PUT again (update) → 200.
	resp, _ = doJSON(t, "PUT", srv.URL+"/api/testbench/scenarios/my-test", scenario)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update: %d", resp.StatusCode)
	}

	// GET single.
	resp, body = doJSON(t, "GET", srv.URL+"/api/testbench/scenarios/my-test", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "scripted") {
		t.Fatalf("get: %d %s", resp.StatusCode, body)
	}

	// GET list includes both stored file and builtins.
	resp, body = doJSON(t, "GET", srv.URL+"/api/testbench/scenarios", "")
	if resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, "my-test") || !strings.Contains(body, "tool-loop-then-stop") {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}

	// The stored scenario actually serves.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(messagesBody("claude-opus-4-8", "x", false)))
	req.Header.Set("x-api-key", "sim-scenario:my-test")
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if res2.StatusCode != http.StatusOK {
		t.Fatalf("stored scenario did not serve: %d", res2.StatusCode)
	}

	// DELETE.
	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/testbench/scenarios/my-test", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", srv.URL+"/api/testbench/scenarios/my-test", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: %d", resp.StatusCode)
	}
}

func TestTestbenchScenarioValidation(t *testing.T) {
	srv, _ := newTestbenchServer(t, true)

	// Invalid: no steps.
	resp, body := doJSON(t, "PUT", srv.URL+"/api/testbench/scenarios/bad", `{"steps": []}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "at least one step") {
		t.Fatalf("validation: %d %s", resp.StatusCode, body)
	}

	// Name mismatch between body and URL.
	resp, body = doJSON(t, "PUT", srv.URL+"/api/testbench/scenarios/name-a",
		`{"name":"name-b","steps":[{"response":{"message":{"content":[{"type":"text","text":"x"}]}}}]}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "does not match") {
		t.Fatalf("name mismatch: %d %s", resp.StatusCode, body)
	}
}

func TestTestbenchBuiltinDelete409(t *testing.T) {
	srv, _ := newTestbenchServer(t, true)
	resp, body := doJSON(t, "DELETE", srv.URL+"/api/testbench/scenarios/tool-loop-then-stop", "")
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("builtin delete: %d %s", resp.StatusCode, body)
	}

	// But a file override of a builtin IS deletable (falls back to builtin).
	override := `{"steps":[{"response":{"message":{"content":[{"type":"text","text":"override"}]}}}]}`
	resp, _ = doJSON(t, "PUT", srv.URL+"/api/testbench/scenarios/echo", override)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("override create: %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/testbench/scenarios/echo", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("override delete: %d", resp.StatusCode)
	}
	resp, body = doJSON(t, "GET", srv.URL+"/api/testbench/scenarios/echo", "")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "override") {
		t.Fatalf("builtin should be back after override delete: %d %s", resp.StatusCode, body)
	}
}

func TestTestbenchCassetteEndpoints(t *testing.T) {
	srv, sim := newTestbenchServer(t, true)

	reqBody, _ := json.Marshal(map[string]any{
		"model": "claude-opus-4-8", "messages": []map[string]any{{"role": "user", "content": "x"}},
	})
	if err := sim.store.AppendCassetteEntry("tape-1", &cassetteEntry{
		Method: "POST", Path: "/v1/messages", Status: 200,
		RequestBody: reqBody, Fingerprint: fingerprintOf(reqBody),
		ResponseBodyB64: base64.StdEncoding.EncodeToString([]byte(`{"id":"m"}`)),
	}); err != nil {
		t.Fatal(err)
	}

	resp, body := doJSON(t, "GET", srv.URL+"/api/testbench/cassettes", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "tape-1") {
		t.Fatalf("list cassettes: %d %s", resp.StatusCode, body)
	}

	// Default detail view is metadata-only (no bodies).
	resp, body = doJSON(t, "GET", srv.URL+"/api/testbench/cassettes/tape-1", "")
	if resp.StatusCode != http.StatusOK || strings.Contains(body, "responseBodyB64") {
		t.Fatalf("cassette meta view: %d %s", resp.StatusCode, body)
	}
	// ?full=1 includes bodies.
	resp, body = doJSON(t, "GET", srv.URL+"/api/testbench/cassettes/tape-1?full=1", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "responseBodyB64") {
		t.Fatalf("cassette full view: %d %s", resp.StatusCode, body)
	}

	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/testbench/cassettes/tape-1", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete cassette: %d", resp.StatusCode)
	}
	resp, _ = doJSON(t, "GET", srv.URL+"/api/testbench/cassettes/tape-1", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("get after delete: %d", resp.StatusCode)
	}
}

func TestTestbenchSessions(t *testing.T) {
	srv, sim := newTestbenchServer(t, true)

	// Generate a session by serving one echo request.
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages",
		strings.NewReader(messagesBody("claude-opus-4-8", "x", false)))
	req.Header.Set("x-api-key", "sim-echo")
	req.Header.Set("X-Gateway-Sim-Session", "visible-session")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	resp, body := doJSON(t, "GET", srv.URL+"/api/testbench/sessions", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "visible-session") {
		t.Fatalf("sessions list: %d %s", resp.StatusCode, body)
	}

	resp, body = doJSON(t, "DELETE", srv.URL+"/api/testbench/sessions/visible-session", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"removed":1`) {
		t.Fatalf("delete session: %d %s", resp.StatusCode, body)
	}

	if infos := sim.snapshotSessions(); len(infos) != 0 {
		t.Fatalf("session not removed: %+v", infos)
	}

	resp, _ = doJSON(t, "DELETE", srv.URL+"/api/testbench/sessions", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reset all: %d", resp.StatusCode)
	}
}

func TestTestbenchDisabledReturns403(t *testing.T) {
	srv, _ := newTestbenchServer(t, false)
	paths := []struct{ method, path string }{
		{"GET", "/api/testbench/scenarios"},
		{"PUT", "/api/testbench/scenarios/x"},
		{"GET", "/api/testbench/cassettes"},
		{"GET", "/api/testbench/sessions"},
		{"DELETE", "/api/testbench/sessions"},
	}
	for _, p := range paths {
		resp, _ := doJSON(t, p.method, srv.URL+p.path, "{}")
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s %s: %d, want 403", p.method, p.path, resp.StatusCode)
		}
	}
}

func TestTestbenchUnmatchedPathIsLocal404(t *testing.T) {
	// The subtree's inner mux must answer unknown testbench paths itself —
	// never the proxy catch-all (which would leak the request upstream).
	srv, _ := newTestbenchServer(t, true)
	resp, _ := doJSON(t, "GET", srv.URL+"/api/testbench/typo-endpoint", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unmatched testbench path: %d, want local 404", resp.StatusCode)
	}
}

func TestTestbenchNameTraversalSanitized(t *testing.T) {
	srv, sim := newTestbenchServer(t, true)
	scenario := `{"steps":[{"response":{"message":{"content":[{"type":"text","text":"x"}]}}}]}`
	resp, _ := doJSON(t, "PUT", srv.URL+"/api/testbench/scenarios/"+
		strings.ReplaceAll("..%2F..%2Fevil-name", "%2F", "%2F"), scenario)
	// Whatever the router does with the encoded slashes, no file may escape
	// the scenarios dir; the slugified name must be served from there.
	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		names, _ := sim.store.ListScenarioFiles()
		for _, n := range names {
			if strings.Contains(n, "..") || strings.Contains(n, "/") {
				t.Fatalf("unsanitized scenario file name: %q", n)
			}
		}
	}
}
