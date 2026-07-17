package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
)

// Test-bench management API — scenario CRUD, cassette inspection, live sim
// sessions.
//
// This handler owns the whole /api/testbench/ subtree via its own inner
// ServeMux (main.go registers it once, wrapped in the auth middleware).
// Deliberately NOT routed through api.go's manual prefix/suffix switch: the
// subtree pattern outranks the "/" proxy catch-all, and unmatched paths
// inside the subtree get the inner mux's local 404 — so a typo'd testbench
// URL can never fall through to the upstream proxy carrying a Bearer token.
type testbenchHandler struct {
	store *Store
	sim   *simulator
	mux   *http.ServeMux
}

func newTestbenchHandler(store *Store, sim *simulator) *testbenchHandler {
	t := &testbenchHandler{store: store, sim: sim, mux: http.NewServeMux()}

	t.mux.HandleFunc("GET /api/testbench/scenarios", t.listScenarios)
	t.mux.HandleFunc("GET /api/testbench/scenarios/{name}", t.getScenario)
	t.mux.HandleFunc("PUT /api/testbench/scenarios/{name}", t.putScenario)
	t.mux.HandleFunc("DELETE /api/testbench/scenarios/{name}", t.deleteScenario)

	t.mux.HandleFunc("GET /api/testbench/cassettes", t.listCassettes)
	t.mux.HandleFunc("GET /api/testbench/cassettes/{name}", t.getCassette)
	t.mux.HandleFunc("DELETE /api/testbench/cassettes/{name}", t.deleteCassette)

	t.mux.HandleFunc("GET /api/testbench/sessions", t.listSessions)
	t.mux.HandleFunc("DELETE /api/testbench/sessions", t.resetSessions)
	t.mux.HandleFunc("DELETE /api/testbench/sessions/{key}", t.deleteSession)

	return t
}

func (t *testbenchHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !t.sim.enabled {
		jsonError(w, "gateway simulator is disabled (set GATEWAY_SIM_ENABLED=1)", http.StatusForbidden)
		return
	}
	t.mux.ServeHTTP(w, r)
}

// --- Scenarios ---

type scenarioListItem struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Steps       int    `json:"steps"`
	Builtin     bool   `json:"builtin"`
	Overridden  bool   `json:"overridden"` // builtin shadowed by a stored file
}

func (t *testbenchHandler) listScenarios(w http.ResponseWriter, r *http.Request) {
	fileNames, err := t.store.ListScenarioFiles()
	if err != nil {
		jsonError(w, "list scenarios: "+err.Error(), http.StatusInternalServerError)
		return
	}
	files := map[string]bool{}
	for _, n := range fileNames {
		files[n] = true
	}

	var items []scenarioListItem
	for _, name := range fileNames {
		sc, err := t.sim.loadScenario(name)
		if err != nil {
			items = append(items, scenarioListItem{Name: name, Description: "INVALID: " + err.Error()})
			continue
		}
		_, isBuiltin := t.sim.builtins[name]
		items = append(items, scenarioListItem{
			Name: name, Description: sc.Description, Steps: len(sc.Steps),
			Builtin: isBuiltin, Overridden: isBuiltin,
		})
	}
	for name, sc := range t.sim.builtins {
		if files[name] {
			continue
		}
		items = append(items, scenarioListItem{
			Name: name, Description: sc.Description, Steps: len(sc.Steps), Builtin: true,
		})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	jsonOK(w, items)
}

func (t *testbenchHandler) getScenario(w http.ResponseWriter, r *http.Request) {
	name := slugify(r.PathValue("name"))
	sc, err := t.sim.loadScenario(name)
	if err != nil {
		if os.IsNotExist(err) {
			jsonError(w, fmt.Sprintf("scenario %q not found", name), http.StatusNotFound)
		} else {
			jsonError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	jsonOK(w, sc)
}

func (t *testbenchHandler) putScenario(w http.ResponseWriter, r *http.Request) {
	name := slugify(r.PathValue("name"))
	if name == "" {
		jsonError(w, "scenario name must be a non-empty slug", http.StatusBadRequest)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxSimBodyBytes))
	if err != nil {
		jsonError(w, "read body: "+err.Error(), http.StatusBadRequest)
		return
	}
	var sc Scenario
	if err := json.Unmarshal(body, &sc); err != nil {
		jsonError(w, "invalid scenario JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if sc.Name == "" {
		sc.Name = name
	}
	if sc.Name != name {
		jsonError(w, fmt.Sprintf("scenario name %q does not match URL name %q", sc.Name, name), http.StatusBadRequest)
		return
	}
	if err := sc.Validate(); err != nil {
		jsonError(w, "invalid scenario: "+err.Error(), http.StatusBadRequest)
		return
	}

	normalized, err := json.MarshalIndent(sc, "", "  ")
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	existed, err := t.store.WriteScenarioFile(name, normalized)
	if err != nil {
		jsonError(w, "write scenario: "+err.Error(), http.StatusInternalServerError)
		return
	}
	status := http.StatusCreated
	if existed {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(sc)
}

func (t *testbenchHandler) deleteScenario(w http.ResponseWriter, r *http.Request) {
	name := slugify(r.PathValue("name"))
	err := t.store.DeleteScenarioFile(name)
	if os.IsNotExist(err) {
		if _, isBuiltin := t.sim.builtins[name]; isBuiltin {
			jsonError(w, fmt.Sprintf("cannot delete built-in scenario %q (no stored override exists)", name), http.StatusConflict)
			return
		}
		jsonError(w, fmt.Sprintf("scenario %q not found", name), http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "delete scenario: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"deleted": name})
}

// --- Cassettes ---

func (t *testbenchHandler) listCassettes(w http.ResponseWriter, r *http.Request) {
	infos, err := t.store.ListCassettes()
	if err != nil {
		jsonError(w, "list cassettes: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if infos == nil {
		infos = []CassetteInfo{}
	}
	jsonOK(w, infos)
}

// cassetteEntryMeta is the body-free projection returned by default;
// ?full=1 returns complete entries (bodies included).
type cassetteEntryMeta struct {
	Seq         int          `json:"seq"`
	RecordedAt  string       `json:"recordedAt"`
	Method      string       `json:"method"`
	Path        string       `json:"path"`
	Status      int          `json:"status"`
	SSE         bool         `json:"sse"`
	Fingerprint *fingerprint `json:"fingerprint,omitempty"`
}

func (t *testbenchHandler) getCassette(w http.ResponseWriter, r *http.Request) {
	name := slugify(r.PathValue("name"))
	entries, err := t.store.ReadCassette(name)
	if err != nil {
		if os.IsNotExist(err) {
			jsonError(w, fmt.Sprintf("cassette %q not found", name), http.StatusNotFound)
		} else {
			jsonError(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if r.URL.Query().Get("full") == "1" {
		jsonOK(w, entries)
		return
	}
	metas := make([]cassetteEntryMeta, 0, len(entries))
	for _, e := range entries {
		metas = append(metas, cassetteEntryMeta{
			Seq: e.Seq, RecordedAt: e.RecordedAt.Format("2006-01-02T15:04:05Z07:00"),
			Method: e.Method, Path: e.Path, Status: e.Status, SSE: e.SSE,
			Fingerprint: e.Fingerprint,
		})
	}
	jsonOK(w, metas)
}

func (t *testbenchHandler) deleteCassette(w http.ResponseWriter, r *http.Request) {
	name := slugify(r.PathValue("name"))
	err := t.store.DeleteCassette(name)
	if os.IsNotExist(err) {
		jsonError(w, fmt.Sprintf("cassette %q not found", name), http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, "delete cassette: "+err.Error(), http.StatusInternalServerError)
		return
	}
	jsonOK(w, map[string]string{"deleted": name})
}

// --- Sessions ---

func (t *testbenchHandler) listSessions(w http.ResponseWriter, r *http.Request) {
	jsonOK(w, t.sim.snapshotSessions())
}

func (t *testbenchHandler) resetSessions(w http.ResponseWriter, r *http.Request) {
	removed := t.sim.resetSessions("")
	jsonOK(w, map[string]int{"removed": removed})
}

func (t *testbenchHandler) deleteSession(w http.ResponseWriter, r *http.Request) {
	removed := t.sim.resetSessions(r.PathValue("key"))
	if removed == 0 {
		jsonError(w, "no session with that key", http.StatusNotFound)
		return
	}
	jsonOK(w, map[string]int{"removed": removed})
}
