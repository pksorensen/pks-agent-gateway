package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestSpeechCapabilitiesReflectRuntimeConfiguration(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v1/capabilities", nil)
	w := httptest.NewRecorder()
	newSpeechCapabilitiesHandler(speechConfig{RuntimeURL: "http://runtime"}).ServeHTTP(w, r)

	var result struct {
		Capabilities []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Capabilities) != 1 || result.Capabilities[0].ID != realtimeSpeechCapability || !result.Capabilities[0].Enabled {
		t.Fatalf("unexpected capabilities: %+v", result.Capabilities)
	}
}

func TestSpeechRealtimeBridgesTextAndBinary(t *testing.T) {
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer runtime-secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		messageType, payload, err := ws.Read(r.Context())
		if err != nil {
			return
		}
		_ = ws.Write(r.Context(), messageType, payload)
	}))
	defer runtime.Close()

	gateway := httptest.NewServer(newSpeechRealtimeHandler(speechConfig{
		RuntimeURL:   runtime.URL,
		ClientToken:  "meeting-secret",
		RuntimeToken: "runtime-secret",
	}))
	defer gateway.Close()

	url := "ws" + strings.TrimPrefix(gateway.URL, "http")
	headers := http.Header{"Authorization": []string{"Bearer meeting-secret"}}
	client, _, err := websocket.Dial(context.Background(), url, &websocket.DialOptions{HTTPHeader: headers})
	if err != nil {
		t.Fatal(err)
	}
	defer client.CloseNow()

	want := []byte{1, 2, 3, 4}
	if err := client.Write(context.Background(), websocket.MessageBinary, want); err != nil {
		t.Fatal(err)
	}
	typ, got, err := client.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if typ != websocket.MessageBinary || string(got) != string(want) {
		t.Fatalf("unexpected echo type=%v payload=%v", typ, got)
	}
}
