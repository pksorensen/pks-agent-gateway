package main

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
)

func TestRuntimeFlags(t *testing.T) {
	envOf := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	if _, err := runtimeFlagsFromEnv(envOf("GATEWAY_MODE", "prod")); err == nil || !strings.Contains(err.Error(), "OIDC_ISSUER") {
		t.Fatalf("prod without OIDC_ISSUER: %v", err)
	}
	f, err := runtimeFlagsFromEnv(envOf("GATEWAY_MODE", "prod", "OIDC_ISSUER", "https://kc/realms/x", "GATEWAY_PASSTHROUGH", "off"))
	if err != nil || !f.prod || f.passthrough || f.sim || f.realtimeSpeech {
		t.Fatalf("prod flags: %+v %v", f, err)
	}
	if !strings.Contains(f.summary(), "passthrough=off") || !strings.Contains(f.summary(), "management-auth=OIDC") {
		t.Fatalf("summary: %s", f.summary())
	}
	f, err = runtimeFlagsFromEnv(envOf())
	if err != nil || !f.passthrough || f.sim || f.realtimeSpeech || f.prod {
		t.Fatalf("dev defaults: %+v %v", f, err)
	}
	for _, bad := range [][]string{{"GATEWAY_MODE", "production"}, {"GATEWAY_PASSTHROUGH", "false"}} {
		if _, err := runtimeFlagsFromEnv(envOf(bad...)); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
	f, _ = runtimeFlagsFromEnv(envOf("GATEWAY_SIM_ENABLED", "1", "GATEWAY_REALTIME_SPEECH_ENABLED", "1"))
	if !f.sim || !f.realtimeSpeech {
		t.Fatalf("explicit enables: %+v", f)
	}
}

func TestRealtimeSpeechOffUnlessEnabled(t *testing.T) {
	cfg := speechConfig{RuntimeURL: "http://runtime"} // URL alone is not enough
	w := httptest.NewRecorder()
	newSpeechRealtimeHandler(cfg).ServeHTTP(w, httptest.NewRequest("GET", "/v1/speech/realtime", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("realtime without enable flag: %d", w.Code)
	}
	w = httptest.NewRecorder()
	newSpeechCapabilitiesHandler(cfg).ServeHTTP(w, httptest.NewRequest("GET", "/v1/capabilities", nil))
	if !strings.Contains(w.Body.String(), `"enabled":false`) {
		t.Fatalf("capabilities: %s", w.Body.String())
	}
}

func TestPassthroughOffAnswersLocally(t *testing.T) {
	sim := newSimulator(NewStore(t.TempDir(), "default"), false)
	h := gatewayKeyGuard(sim.Gate(passthroughDisabled()))
	for _, tc := range []struct {
		key  string
		want int
	}{{"sk-ant-real", 404}, {"sim-echo", 403}, {"gwk_x_y", 401}} {
		r := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{}`))
		r.Header.Set("x-api-key", tc.key)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("%s: %d want %d", tc.key, w.Code, tc.want)
		}
	}
}

// provisionSpeech sets up a metered speech API like the production one.
func provisionSpeech(g *gw, upstream string, op map[string]any, limits map[string]any) string {
	g.must(201, "POST", "/api/v1/owners/test/credentials", map[string]any{
		"id": "speech-key", "source": "sealed", "value": canary, "inject": map[string]any{"header": "Ocp-Apim-Subscription-Key"}})
	g.must(201, "POST", "/api/v1/owners/test/apis", map[string]any{"id": "speech", "upstream": upstream, "credential": "speech-key",
		"operations": []map[string]any{op, {"id": "other", "method": "POST", "path": "/other", "maxBodyBytes": 1000}}})
	g.must(201, "POST", "/api/v1/owners/test/bundles", map[string]any{"id": "speech", "apis": []string{"speech"}})
	g.must(201, "POST", "/api/v1/owners/test/principals", map[string]any{"id": "site", "kind": "service", "limits": limits})
	var issued issuedKeys
	_ = json.Unmarshal([]byte(g.must(201, "POST", "/api/v1/owners/test/subscriptions", map[string]any{"bundle": "speech", "principal": "site"})), &issued)
	return issued.PrimaryKey
}

var transcribeOp = map[string]any{"id": "transcribe", "method": "POST", "path": "/speechtotext/transcriptions:transcribe", "meter": "audio"}

func TestAudioQuotaAndRateLimit(t *testing.T) {
	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)
	key := provisionSpeech(g, up.URL, transcribeOp, map[string]any{"audioSecondsPerDay": 10, "requestsPerMinute": 5})
	path := "/apis/test/speech/speechtotext/transcriptions:transcribe?api-version=2025-10-15"
	sixSeconds := strings.Repeat("a", 6*32000)

	if c, out := g.do("POST", path, sixSeconds, g.key(key)); c != 200 {
		t.Fatalf("first 6 s: %d %s", c, out)
	}
	hits := up.count()
	c, out := g.do("POST", path, sixSeconds, g.key(key))
	if c != 429 || !strings.Contains(out, "audio seconds") || up.count() != hits {
		t.Fatalf("over daily audio: %d %s (upstream hits %d→%d)", c, out, hits, up.count())
	}

	// A restart forgets memory, not the day's usage.
	g.lane.limits = newUsageLimiter()
	if c, _ := g.do("POST", path, sixSeconds, g.key(key)); c != 429 {
		t.Fatalf("after restart: %d", c)
	}
	if c, _ := g.do("POST", path, strings.Repeat("a", 4*32000), g.key(key)); c != 200 {
		t.Fatalf("exactly at quota: %d", c)
	}

	usage := g.must(200, "GET", "/api/v1/owners/test/usage", nil)
	var u struct {
		Total usageTotals `json:"total"`
	}
	_ = json.Unmarshal([]byte(usage), &u)
	if u.Total.AudioSeconds != 10 || u.Total.BytesIn != 10*32000 {
		t.Fatalf("usage totals: %+v", u.Total)
	}
	g.assertNoCanary(usage)

	// Requests/min: from a fresh window, 5 pass and the 6th is a 429 with
	// Retry-After.
	g.lane.limits = newUsageLimiter()
	for i := 0; i < 5; i++ {
		if c, _ := g.do("POST", "/apis/test/speech/other", "{}", g.key(key)); c != 200 {
			t.Fatalf("request %d: %d", i, c)
		}
	}
	req, _ := http.NewRequest("POST", g.srv.URL+"/apis/test/speech/other", strings.NewReader("{}"))
	req.Header.Set("Api-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 429 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("rpm: %d retry-after=%q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}

	// Limits are managed via the API; {} clears them.
	g.must(200, "PUT", "/api/v1/owners/test/principals/site/limits", map[string]any{})
	if c, _ := g.do("POST", "/apis/test/speech/other", "{}", g.key(key)); c != 200 {
		t.Fatalf("after clearing limits: %d", c)
	}
	if c, _ := g.do("PUT", "/api/v1/owners/test/principals/site/limits", map[string]any{"requestsPerMinute": -1}, nil); c != 400 {
		t.Fatalf("negative limit: %d", c)
	}
}

func TestBodyCaps(t *testing.T) {
	up := newFakeUpstream(t)
	g := newTestGateway(t, nil)
	key := provisionSpeech(g, up.URL, transcribeOp, nil)

	// Declared Content-Length over the op cap: 413, nothing forwarded.
	if c, _ := g.do("POST", "/apis/test/speech/other", strings.Repeat("x", 2000), g.key(key)); c != 413 || up.count() != 0 {
		t.Fatalf("over cap: %d, upstream %d", c, up.count())
	}
	// Chunked (no Content-Length) body over the cap is cut at the cap.
	chunked := func(path string, n int) int {
		pr, pw := io.Pipe()
		go func() { pw.Write(bytes.Repeat([]byte("x"), n)); pw.Close() }()
		req, _ := http.NewRequest("POST", g.srv.URL+path, pr)
		req.ContentLength = -1
		req.Header.Set("Api-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := chunked("/apis/test/speech/other", 4000); c != 413 {
		t.Fatalf("chunked over cap: %d", c)
	}
	if c := chunked("/apis/test/speech/other", 500); c != 200 {
		t.Fatalf("chunked under cap: %d", c)
	}
	// A metered op cannot be estimated without Content-Length.
	if c := chunked("/apis/test/speech/speechtotext/transcriptions:transcribe", 100); c != 411 {
		t.Fatalf("chunked metered: %d", c)
	}
	// Default cap (6 MiB) applies to ops that set none.
	if c, _ := g.do("POST", "/apis/test/speech/speechtotext/transcriptions:transcribe", strings.Repeat("x", 6<<20+1), g.key(key)); c != 413 {
		t.Fatalf("default cap: %d", c)
	}
	if c, out := g.do("PUT", "/api/v1/owners/test/apis/speech/operations/bad", map[string]any{"id": "bad", "method": "POST", "path": "/x", "meter": "tokens"}, nil); c != 400 {
		t.Fatalf("unknown meter: %d %s", c, out)
	}
}

// TestSpeechTranscribeColonPath: Azure's fast-transcription operation has a
// colon in its last segment. It must reach the upstream unescaped, with
// api-version and the multipart body untouched.
func TestSpeechTranscribeColonPath(t *testing.T) {
	type seen struct {
		requestURI, path, query, contentType, key string
		body                                      []byte
	}
	var mu sync.Mutex
	var got []seen
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, seen{r.RequestURI, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type"), r.Header.Get("Ocp-Apim-Subscription-Key"), b})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"combinedPhrases":[{"text":"hej"}]}`)
	}))
	defer up.Close()
	g := newTestGateway(t, nil)
	key := provisionSpeech(g, up.URL, transcribeOp, nil)

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="audio"; filename="a.wav"`)
	h.Set("Content-Type", "audio/wav")
	part, _ := mw.CreatePart(h)
	part.Write(append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), bytes.Repeat([]byte{0, 1}, 3200)...))
	def, _ := mw.CreateFormField("definition")
	def.Write([]byte(`{"locales":["da"],"enhancedMode":{"enabled":true,"model":"mai-transcribe-1.5","phraseList":["Gjellervand"]}}`))
	mw.Close()
	ct := mw.FormDataContentType()

	for _, p := range []string{
		"/apis/test/speech/speechtotext/transcriptions:transcribe?api-version=2025-10-15",
		"/apis/test/speech/speechtotext/transcriptions%3Atranscribe?api-version=2025-10-15",
	} {
		req, _ := http.NewRequest("POST", g.srv.URL+p, bytes.NewReader(body.Bytes()))
		req.Header.Set("Content-Type", ct)
		req.Header.Set("Api-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		out, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(string(out), "combinedPhrases") {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, out)
		}
		mu.Lock()
		s := got[len(got)-1]
		mu.Unlock()
		if s.requestURI != "/speechtotext/transcriptions:transcribe?api-version=2025-10-15" {
			t.Fatalf("%s: upstream request-URI %q", p, s.requestURI)
		}
		if s.contentType != ct || !bytes.Equal(s.body, body.Bytes()) {
			t.Fatalf("%s: multipart altered (ct %q, %d vs %d bytes)", p, s.contentType, len(s.body), body.Len())
		}
		if s.key != canary {
			t.Fatalf("%s: credential not injected", p)
		}
	}
	// A different verb or path on the same API is still a local 404.
	if c, _ := g.do("GET", "/apis/test/speech/speechtotext/transcriptions:transcribe", nil, g.key(key)); c != 404 {
		t.Fatalf("GET on transcribe: %d", c)
	}
	if c, _ := g.do("POST", "/apis/test/speech/speechtotext/transcriptions:other", "{}", g.key(key)); c != 404 {
		t.Fatalf("other colon verb: %d", c)
	}
}
