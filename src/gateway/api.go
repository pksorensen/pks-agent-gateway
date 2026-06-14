package main

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// apiHandler implements REST endpoints for project management.
//
// Routes (registered in main.go, auth middleware applied by caller):
//
//	GET  /api/projects
//	POST /api/projects
//	GET  /api/projects/{id}
//	GET  /api/projects/{id}/stats
type apiHandler struct {
	store *Store
}

func newAPIHandler(store *Store) *apiHandler {
	return &apiHandler{store: store}
}

func (h *apiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case r.Method == http.MethodGet && path == "/api/projects":
		h.listProjects(w, r)
	case r.Method == http.MethodPost && path == "/api/projects":
		h.createProject(w, r)
	case r.Method == http.MethodGet && strings.HasSuffix(path, "/stats"):
		// GET /api/projects/{id}/stats
		id := extractPathSegment(path, "/api/projects/", "/stats")
		h.projectStats(w, r, id)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/projects/"):
		// GET /api/projects/{id}
		id := strings.TrimPrefix(path, "/api/projects/")
		h.getProject(w, r, id)
	default:
		http.NotFound(w, r)
	}
}

// extractPathSegment trims prefix and suffix from path to yield the middle
// segment, e.g. "/api/projects/foo/stats" → "foo".
func extractPathSegment(path, prefix, suffix string) string {
	s := strings.TrimPrefix(path, prefix)
	s = strings.TrimSuffix(s, suffix)
	return s
}

// listProjects — GET /api/projects
// GatewayAdmin sees all projects; others see only projects where their sub
// appears in members.json.
func (h *apiHandler) listProjects(w http.ResponseWriter, r *http.Request) {
	claims := claimsFromCtx(r.Context())

	all, err := h.store.ListProjects()
	if err != nil {
		jsonError(w, "list projects failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if claims != nil && claims.IsAdmin() {
		jsonOK(w, all)
		return
	}

	// Filter to projects where the user is a member.
	var visible []Project
	for _, p := range all {
		members, _ := h.store.ListMembers(p.ID)
		for _, m := range members {
			if claims != nil && m.Sub == claims.Sub {
				visible = append(visible, p)
				break
			}
		}
	}
	jsonOK(w, visible)
}

// createProject — POST /api/projects (GatewayAdmin only)
func (h *apiHandler) createProject(w http.ResponseWriter, r *http.Request) {
	// Role already enforced by auth middleware; double-check defensively.
	claims := claimsFromCtx(r.Context())
	if claims != nil && !claims.IsAdmin() {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}

	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		jsonError(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(body.Name) == "" {
		jsonError(w, "name is required", http.StatusBadRequest)
		return
	}

	p := Project{
		ID:          slugify(body.Name),
		Name:        body.Name,
		Description: body.Description,
		CreatedAt:   time.Now().UTC(),
	}
	if err := h.store.CreateProject(p); err != nil {
		jsonError(w, "create project failed: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(p)
}

// getProject — GET /api/projects/{id}
func (h *apiHandler) getProject(w http.ResponseWriter, r *http.Request, id string) {
	claims := claimsFromCtx(r.Context())

	p, err := h.store.GetProject(id)
	if err != nil {
		if os.IsNotExist(err) || strings.Contains(err.Error(), "not found") {
			jsonError(w, "project not found", http.StatusNotFound)
		} else {
			jsonError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	if !canAccessProject(h.store, claims, id) {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}

	jsonOK(w, p)
}

// canAccessProject returns true if the user is a GatewayAdmin or a member of
// the project.
func canAccessProject(store *Store, claims *UserClaims, projectID string) bool {
	if claims == nil {
		return false
	}
	if claims.IsAdmin() {
		return true
	}
	members, _ := store.ListMembers(projectID)
	for _, m := range members {
		if m.Sub == claims.Sub {
			return true
		}
	}
	return false
}

// --- Stats ---

type modelStats struct {
	Requests int     `json:"requests"`
	CostUsd  float64 `json:"costUsd"`
}

type userStats struct {
	Requests int     `json:"requests"`
	CostUsd  float64 `json:"costUsd"`
}

type statsResponse struct {
	Project           string                `json:"project"`
	TotalCostUsd      float64               `json:"totalCostUsd"`
	TotalInputTokens  int64                 `json:"totalInputTokens"`
	TotalOutputTokens int64                 `json:"totalOutputTokens"`
	ByModel           map[string]modelStats `json:"byModel"`
	ByUser            map[string]userStats  `json:"byUser"`
	Days              []string              `json:"days"`
}

// projectStats — GET /api/projects/{id}/stats
func (h *apiHandler) projectStats(w http.ResponseWriter, r *http.Request, id string) {
	if !canAccessProject(h.store, claimsFromCtx(r.Context()), id) {
		jsonError(w, "forbidden", http.StatusForbidden)
		return
	}

	dates, err := h.store.OtelFileDates(id)
	if err != nil {
		jsonError(w, "list otel files: "+err.Error(), http.StatusInternalServerError)
		return
	}

	stats := statsResponse{
		Project: id,
		ByModel: make(map[string]modelStats),
		ByUser:  make(map[string]userStats),
		Days:    dates,
	}

	for _, date := range dates {
		events, err := h.store.ReadOtelEvents(id, date)
		if err != nil {
			continue
		}
		for _, ev := range events {
			aggregateEvent(&stats, ev)
		}
	}

	jsonOK(w, stats)
}

// aggregateEvent updates stats from a single wrapped JSONL event.
func aggregateEvent(stats *statsResponse, ev json.RawMessage) {
	// Wrapper shape: {"savedAt":..., "signal":"logs", "user":"alice@co", "data":{...}}
	var wrapper struct {
		Signal string          `json:"signal"`
		User   string          `json:"user"` // resource-level user tag set by gateway-cli env
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(ev, &wrapper); err != nil {
		return
	}
	if wrapper.Signal != "logs" {
		return
	}

	// Walk resourceLogs[].scopeLogs[].logRecords[].attributes[]
	var envelope struct {
		ResourceLogs []struct {
			ScopeLogs []struct {
				LogRecords []struct {
					Attributes []otlpAttr `json:"attributes"`
				} `json:"logRecords"`
			} `json:"scopeLogs"`
		} `json:"resourceLogs"`
	}
	if err := json.Unmarshal(wrapper.Data, &envelope); err != nil {
		return
	}

	for _, rl := range envelope.ResourceLogs {
		for _, sl := range rl.ScopeLogs {
			for _, lr := range sl.LogRecords {
				attrs := attrMap(lr.Attributes)

				inputTokens := attrInt(attrs, "input_tokens")
				outputTokens := attrInt(attrs, "output_tokens")
				costUsd := attrFloat(attrs, "cost_usd")
				model := attrString(attrs, "model")
				// user comes from the resource-level wrapper; fall back to log-record attribute
				userEmail := wrapper.User
				if userEmail == "" {
					userEmail = attrString(attrs, "user_email")
				}

				stats.TotalInputTokens += inputTokens
				stats.TotalOutputTokens += outputTokens
				stats.TotalCostUsd += costUsd

				if model != "" {
					ms := stats.ByModel[model]
					ms.Requests++
					ms.CostUsd += costUsd
					stats.ByModel[model] = ms
				}
				if userEmail != "" {
					us := stats.ByUser[userEmail]
					us.Requests++
					us.CostUsd += costUsd
					stats.ByUser[userEmail] = us
				}
			}
		}
	}
}

// otlpAttr represents a single OTLP attribute key/value pair.
// OTLP JSON encodes int64 values as strings ("intValue":"1500") per protobuf
// JSON mapping rules, so we use otlpValue with a custom unmarshaler.
type otlpAttr struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue string
	IntValue    int64
	DoubleValue float64
}

func (v *otlpValue) UnmarshalJSON(b []byte) error {
	var raw struct {
		StringValue string          `json:"stringValue"`
		IntValue    json.RawMessage `json:"intValue"`    // may be string or number
		DoubleValue json.RawMessage `json:"doubleValue"` // may be string or number
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	v.StringValue = raw.StringValue
	if len(raw.IntValue) > 0 {
		// Protobuf JSON maps int64 → JSON string; accept both forms.
		var asNum int64
		if err := json.Unmarshal(raw.IntValue, &asNum); err == nil {
			v.IntValue = asNum
		} else {
			var asStr string
			if err2 := json.Unmarshal(raw.IntValue, &asStr); err2 == nil {
				v.IntValue, _ = strconv.ParseInt(asStr, 10, 64)
			}
		}
	}
	if len(raw.DoubleValue) > 0 {
		var asNum float64
		if err := json.Unmarshal(raw.DoubleValue, &asNum); err == nil {
			v.DoubleValue = asNum
		} else {
			var asStr string
			if err2 := json.Unmarshal(raw.DoubleValue, &asStr); err2 == nil {
				v.DoubleValue, _ = strconv.ParseFloat(asStr, 64)
			}
		}
	}
	return nil
}

func attrMap(attrs []otlpAttr) map[string]otlpAttr {
	m := make(map[string]otlpAttr, len(attrs))
	for _, a := range attrs {
		m[a.Key] = a
	}
	return m
}

func attrString(m map[string]otlpAttr, key string) string {
	if a, ok := m[key]; ok {
		return a.Value.StringValue
	}
	return ""
}

func attrInt(m map[string]otlpAttr, key string) int64 {
	if a, ok := m[key]; ok {
		return a.Value.IntValue
	}
	return 0
}

func attrFloat(m map[string]otlpAttr, key string) float64 {
	if a, ok := m[key]; ok {
		return a.Value.DoubleValue
	}
	return 0
}

// --- helpers ---

var nonAlphanum = regexp.MustCompile(`[^a-z0-9-]+`)

// slugify converts a project name into a URL-safe identifier.
func slugify(name string) string {
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, " ", "-")
	s = nonAlphanum.ReplaceAllString(s, "")
	s = strings.Trim(s, "-")
	return s
}

func jsonOK(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
