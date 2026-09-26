package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/coder/websocket"
)

const realtimeSpeechCapability = "speech.realtime.transcribe"

type speechConfig struct {
	RuntimeURL   string
	ClientToken  string
	RuntimeToken string
}

func speechConfigFromEnv() speechConfig {
	return speechConfig{
		RuntimeURL:   strings.TrimRight(os.Getenv("SPEECH_RUNTIME_URL"), "/"),
		ClientToken:  os.Getenv("SPEECH_GATEWAY_TOKEN"),
		RuntimeToken: os.Getenv("SPEECH_RUNTIME_TOKEN"),
	}
}

func (c speechConfig) enabled() bool { return c.RuntimeURL != "" }

func newSpeechCapabilitiesHandler(config speechConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"capabilities": []map[string]any{{
				"id":        realtimeSpeechCapability,
				"enabled":   config.enabled(),
				"transport": "websocket",
				"audio": map[string]any{
					"sampleRate": 24000,
					"channels":   1,
					"encoding":   "pcm16",
				},
			}},
		})
	}
}

func newSpeechRealtimeHandler(config speechConfig) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !config.enabled() {
			http.Error(w, "speech.realtime.transcribe is not configured", http.StatusServiceUnavailable)
			return
		}
		if config.ClientToken != "" && r.Header.Get("Authorization") != "Bearer "+config.ClientToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		client, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			OriginPatterns: []string{"*"},
		})
		if err != nil {
			return
		}
		defer client.CloseNow()

		runtimeURL, err := speechRuntimeWebSocketURL(config.RuntimeURL)
		if err != nil {
			_ = client.Close(websocket.StatusInternalError, err.Error())
			return
		}
		headers := http.Header{}
		if config.RuntimeToken != "" {
			headers.Set("Authorization", "Bearer "+config.RuntimeToken)
		}
		runtime, _, err := websocket.Dial(r.Context(), runtimeURL, &websocket.DialOptions{HTTPHeader: headers})
		if err != nil {
			log.Printf("speech runtime dial failed: %v", err)
			_ = client.Close(websocket.StatusTryAgainLater, "speech runtime unavailable")
			return
		}
		defer runtime.CloseNow()

		ctx, cancel := context.WithCancel(r.Context())
		defer cancel()
		errors := make(chan error, 2)
		go func() { errors <- copyWebSocket(ctx, runtime, client) }()
		go func() { errors <- copyWebSocket(ctx, client, runtime) }()
		err = <-errors
		cancel()
		if err != nil && !isNormalWebSocketClose(err) {
			log.Printf("speech websocket bridge ended: %v", err)
		}
	}
}

func speechRuntimeWebSocketURL(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("invalid SPEECH_RUNTIME_URL %q", base)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("unsupported speech runtime URL scheme %q", u.Scheme)
	}
	u.Path = strings.TrimRight(u.Path, "/") + "/v1/speech/realtime"
	return u.String(), nil
}

func copyWebSocket(ctx context.Context, destination, source *websocket.Conn) error {
	for {
		messageType, payload, err := source.Read(ctx)
		if err != nil {
			return err
		}
		if err := destination.Write(ctx, messageType, payload); err != nil {
			return err
		}
	}
}

func isNormalWebSocketClose(err error) bool {
	status := websocket.CloseStatus(err)
	return status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
}
