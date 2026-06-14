package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

const maxOtelBodyBytes = 10 * 1024 * 1024 // 10 MB

// otelHandler handles OTLP HTTP JSON ingestion for logs, traces, and metrics.
// No authentication is required — clients are expected to be internal agents.
type otelHandler struct {
	store *Store
}

func newOtelHandler(store *Store) *otelHandler {
	return &otelHandler{store: store}
}

func (h *otelHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var signal string
	switch r.URL.Path {
	case "/otel/v1/logs":
		signal = "logs"
	case "/otel/v1/traces":
		signal = "traces"
	case "/otel/v1/metrics":
		signal = "metrics"
	default:
		http.NotFound(w, r)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxOtelBodyBytes))
	if err != nil {
		http.Error(w, "gateway: read body: "+err.Error(), http.StatusBadRequest)
		return
	}

	raw := json.RawMessage(body)

	project := extractResourceAttr(raw, signal, "project")
	user := extractResourceAttr(raw, signal, "user")

	if project == "" {
		log.Printf("otel/%s: no 'project' resource attribute — routing to _unrouted", signal)
		project = "_unrouted"
	}

	wrapped := map[string]interface{}{
		"savedAt": time.Now().UnixMilli(),
		"signal":  signal,
		"project": project,
		"user":    user,
		"data":    raw,
	}
	wrappedJSON, err := json.Marshal(wrapped)
	if err != nil {
		http.Error(w, "gateway: marshal wrapper: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := h.store.AppendOtelEvent(project, json.RawMessage(wrappedJSON)); err != nil {
		log.Printf("otel/%s: append event for project %q: %v", signal, project, err)
		http.Error(w, "gateway: store event: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, `{"partialSuccess":{}}`)
}

// extractResourceAttr walks the top-level resource array for the given signal
// type and returns the stringValue for the first attribute whose key matches.
//
// OTLP JSON shapes:
//
//	logs    → resourceLogs[].resource.attributes[]
//	traces  → resourceSpans[].resource.attributes[]
//	metrics → resourceMetrics[].resource.attributes[]
func extractResourceAttr(body json.RawMessage, signal string, key string) string {
	var topKey string
	switch signal {
	case "logs":
		topKey = "resourceLogs"
	case "traces":
		topKey = "resourceSpans"
	case "metrics":
		topKey = "resourceMetrics"
	default:
		return ""
	}

	// Decode only the fields we need to avoid copying the full payload.
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(body, &envelope); err != nil {
		return ""
	}
	resourcesRaw, ok := envelope[topKey]
	if !ok {
		return ""
	}

	var resources []struct {
		Resource struct {
			Attributes []struct {
				Key   string `json:"key"`
				Value struct {
					StringValue string `json:"stringValue"`
				} `json:"value"`
			} `json:"attributes"`
		} `json:"resource"`
	}
	if err := json.Unmarshal(resourcesRaw, &resources); err != nil {
		return ""
	}

	for _, res := range resources {
		for _, attr := range res.Resource.Attributes {
			if attr.Key == key {
				return attr.Value.StringValue
			}
		}
	}
	return ""
}
