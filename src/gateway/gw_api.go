package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Management plane for the subscription lane: /api/v1/… (PRD FR-1..22,
// ADR 0005). Own inner mux (like /api/testbench/) so an unknown path is a
// local 404 and can never fall through to the passthrough proxy.
//
// Everything an operator manages lives under /api/v1/owners/{owner}/… and
// needs owner-admin (the owner's listed subs, or realm GatewayAdmin). The
// owner's catalog is readable by any authenticated caller; /api/v1/me/… is
// subscriber self-service across owners.

type gatewayAPI struct {
	root   *GatewayRoot
	hub    *connectorHub // nil = presence not reported
	creds  *CredentialResolver
	sealer *Sealer
	mux    *http.ServeMux
	now    func() time.Time
}

type ownerStoreKey struct{}

// ownerStore is the owner store resolved (and authorised) by withOwner.
func ownerStore(r *http.Request) *GatewayStore {
	return r.Context().Value(ownerStoreKey{}).(*GatewayStore)
}

// withOwner resolves {owner} (validated by GatewayRoot.Owner), requires it to
// be claimed, and — when admin — requires the caller to administer it.
func (a *gatewayAPI) withOwner(admin bool, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st, err := a.root.Owner(r.PathValue("owner"))
		if err == nil {
			if _, err = st.Record(); err == nil {
				// Re-resolve: a claim racing the first lookup is now cached,
				// so every writer shares the owner's one write mutex.
				st, err = a.root.Owner(st.owner)
			}
		}
		if err != nil {
			jsonError(w, "owner not found", http.StatusNotFound)
			return
		}
		if admin && !isOwnerAdmin(claimsFromCtx(r.Context()), st) {
			jsonError(w, "not an admin of this owner", http.StatusForbidden)
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), ownerStoreKey{}, st)))
	}
}

func newGatewayAPI(root *GatewayRoot, creds *CredentialResolver, sealer *Sealer) *gatewayAPI {
	a := &gatewayAPI{root: root, creds: creds, sealer: sealer, mux: http.NewServeMux(), now: time.Now}
	const o = "/api/v1/owners/{owner}"
	admin := func(method, path string, h http.HandlerFunc) {
		a.mux.HandleFunc(method+" "+o+path, a.withOwner(true, h))
	}

	a.mux.HandleFunc("GET /api/v1/owners", a.listOwners)
	a.mux.HandleFunc("POST /api/v1/owners", a.claimOwner)
	admin("GET", "", a.getOwner)
	admin("PUT", "/admins/{sub}", a.ownerAdmin(true))
	admin("DELETE", "/admins/{sub}", a.ownerAdmin(false))

	admin("GET", "/apis", a.listApis)
	admin("POST", "/apis", a.createApi)
	admin("GET", "/apis/{id}", a.getApi)
	admin("PATCH", "/apis/{id}", a.patchApi)
	admin("DELETE", "/apis/{id}", a.deleteApi)
	admin("PUT", "/apis/{id}/operations/{op}", a.putOperation)
	admin("DELETE", "/apis/{id}/operations/{op}", a.deleteOperation)
	admin("POST", "/apis/{id}/openapi", a.importOpenAPI)

	admin("GET", "/mcp-servers", a.listMcp)
	admin("POST", "/mcp-servers", a.createMcp)
	admin("GET", "/mcp-servers/{id}", a.getMcp)
	admin("DELETE", "/mcp-servers/{id}", a.deleteMcp)

	admin("GET", "/bundles", a.listBundles)
	admin("POST", "/bundles", a.createBundle)
	admin("GET", "/bundles/{id}", a.getBundle)
	admin("DELETE", "/bundles/{id}", a.deleteBundle)
	admin("PUT", "/bundles/{id}/apis/{item}", a.bundleItem(true, false))
	admin("DELETE", "/bundles/{id}/apis/{item}", a.bundleItem(false, false))
	admin("PUT", "/bundles/{id}/mcp-servers/{item}", a.bundleItem(true, true))
	admin("DELETE", "/bundles/{id}/mcp-servers/{item}", a.bundleItem(false, true))
	admin("POST", "/bundles/{id}/visibility", a.bundleVisibility)

	admin("GET", "/principals", a.listPrincipals)
	admin("POST", "/principals", a.createPrincipal)
	admin("GET", "/principals/{id}", a.getPrincipal)
	admin("POST", "/principals/{id}/disable", a.setPrincipalDisabled(true))
	admin("POST", "/principals/{id}/enable", a.setPrincipalDisabled(false))

	admin("GET", "/subscriptions", a.listSubscriptions)
	admin("POST", "/subscriptions", a.createSubscription)
	admin("GET", "/subscriptions/{id}", a.getSubscription)
	admin("POST", "/subscriptions/{id}/suspend", a.transition("suspend"))
	admin("POST", "/subscriptions/{id}/reactivate", a.transition("reactivate"))
	admin("POST", "/subscriptions/{id}/cancel", a.transition("cancel"))
	admin("POST", "/subscriptions/{id}/regenerate", a.regenerate(false))

	admin("GET", "/credentials", a.listCredentials)
	admin("POST", "/credentials", a.createCredential)
	admin("GET", "/credentials/{id}", a.getCredential)
	admin("DELETE", "/credentials/{id}", a.deleteCredential)
	admin("POST", "/credentials/{id}/rotate", a.rotateCredential)

	admin("GET", "/connectors", a.listConnectors)
	admin("POST", "/connectors", a.createConnector)
	admin("GET", "/connectors/{id}", a.getConnector)
	admin("DELETE", "/connectors/{id}", a.deleteConnector)
	admin("POST", "/connectors/{id}/disable", a.setConnectorDisabled(true))
	admin("POST", "/connectors/{id}/enable", a.setConnectorDisabled(false))
	admin("POST", "/connectors/{id}/regenerate", a.regenerateConnector)

	admin("GET", "/usage", a.usage)

	a.mux.HandleFunc("GET "+o+"/catalog", a.withOwner(false, a.catalog))
	a.mux.HandleFunc("GET /api/v1/me/subscriptions", a.mySubscriptions)
	a.mux.HandleFunc("POST /api/v1/me/subscriptions/{owner}/{id}/regenerate", a.withOwner(false, a.regenerate(true)))
	return a
}

func (a *gatewayAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.mux.ServeHTTP(w, r) }

// --- Owners ---

func (a *gatewayAPI) listOwners(w http.ResponseWriter, r *http.Request) {
	c := claimsFromCtx(r.Context())
	out := []OwnerRecord{}
	for _, st := range a.root.Owners() {
		if isOwnerAdmin(c, st) {
			if rec, err := st.Record(); err == nil {
				out = append(out, *rec)
			}
		}
	}
	writeJSONStatus(w, http.StatusOK, out)
}

func (a *gatewayAPI) claimOwner(w http.ResponseWriter, r *http.Request) {
	c := claimsFromCtx(r.Context())
	if c == nil || c.Sub == "" {
		jsonError(w, "an authenticated subject is required", http.StatusUnauthorized)
		return
	}
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeBody(r, &in); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !validID(in.ID) {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	rec, err := a.root.Claim(in.ID, c.Sub, a.now().UTC())
	if errors.Is(err, errOwnerTaken) {
		jsonError(w, "owner already claimed", http.StatusConflict)
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONStatus(w, http.StatusCreated, rec)
}

func (a *gatewayAPI) getOwner(w http.ResponseWriter, r *http.Request) {
	rec, err := ownerStore(r).Record()
	if err != nil {
		storeErr(w, "owner", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, rec)
}

func (a *gatewayAPI) ownerAdmin(add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := ownerStore(r)
		rec, err := st.Record()
		if err != nil {
			storeErr(w, "owner", err)
			return
		}
		sub := r.PathValue("sub")
		if add {
			if !contains(rec.Admins, sub) {
				rec.Admins = append(rec.Admins, sub)
			}
		} else {
			rec.Admins = without(rec.Admins, sub)
			if len(rec.Admins) == 0 {
				jsonError(w, "an owner must keep at least one admin", http.StatusConflict)
				return
			}
		}
		rec.UpdatedAt = a.now().UTC()
		if err := st.SaveRecord(rec); err != nil {
			storeErr(w, "owner", err)
			return
		}
		writeJSONStatus(w, http.StatusOK, rec)
	}
}

func writeJSONStatus(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func decodeBody(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, 8<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// storeErr maps store errors onto HTTP.
func storeErr(w http.ResponseWriter, what string, err error) {
	if errors.Is(err, errNotFound) {
		jsonError(w, what+" not found", http.StatusNotFound)
		return
	}
	jsonError(w, err.Error(), http.StatusInternalServerError)
}

func listHandler[T any](kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := ownerStore(r)
		xs, err := listKind[T](st, kind)
		if err != nil {
			storeErr(w, kind, err)
			return
		}
		writeJSONStatus(w, http.StatusOK, xs)
	}
}

// --- APIs ---

func (a *gatewayAPI) listApis(w http.ResponseWriter, r *http.Request) {
	listHandler[Api](kindApi)(w, r)
}

func (a *gatewayAPI) validateApi(st *GatewayStore, api *Api) error {
	if api.Kind == "" {
		api.Kind = ApiKindHTTP
	}
	if api.Kind != ApiKindHTTP && api.Kind != ApiKindLLM {
		return fmt.Errorf("kind must be http or llm")
	}
	if strings.HasPrefix(api.Upstream, runnerScheme+"://") {
		t, ok := parseRunnerUpstream(api.Upstream)
		if !ok {
			return fmt.Errorf("upstream must be runner://<connector>/<slot>[/base/path]")
		}
		if api.Credential != "" {
			// ADR 0004: the credential lives with the runner, never here.
			return fmt.Errorf("a runner:// upstream must not carry a credential")
		}
		if !st.exists(kindConnector, t.connector) {
			return fmt.Errorf("connector %q not found", t.connector)
		}
	} else if !strings.HasPrefix(api.Upstream, "http://") && !strings.HasPrefix(api.Upstream, "https://") {
		return fmt.Errorf("upstream must be an http(s) or runner:// URL")
	}
	if err := a.checkInjectable(st, api.Credential); err != nil {
		return err
	}
	for i := range api.Operations {
		if err := validateOperation(&api.Operations[i]); err != nil {
			return err
		}
	}
	return nil
}

// checkInjectable refuses a credential that has nowhere to go: without an
// inject target the upstream call would silently leave unauthenticated.
func (a *gatewayAPI) checkInjectable(st *GatewayStore, id string) error {
	if id == "" {
		return nil
	}
	c, err := st.GetCredential(id)
	if err != nil {
		return fmt.Errorf("credential %q not found", id)
	}
	if !c.Inject.valid() {
		return fmt.Errorf("credential %q has no inject target (header, query or bearer)", id)
	}
	return nil
}

var opIDPattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.-]{0,63}$`)

func validateOperation(op *Operation) error {
	if !opIDPattern.MatchString(op.ID) {
		return fmt.Errorf("operation id %q invalid", op.ID)
	}
	op.Method = strings.ToUpper(op.Method)
	switch op.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD":
	default:
		return fmt.Errorf("operation %s: unsupported method %q", op.ID, op.Method)
	}
	if !strings.HasPrefix(op.Path, "/") {
		return fmt.Errorf("operation %s: path must start with /", op.ID)
	}
	if len(op.InputSchema) > 0 && !json.Valid(op.InputSchema) {
		return fmt.Errorf("operation %s: inputSchema is not valid JSON", op.ID)
	}
	return nil
}

func (a *gatewayAPI) createApi(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	var api Api
	if err := decodeBody(r, &api); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !validID(api.ID) {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if st.exists(kindApi, api.ID) {
		jsonError(w, "api already exists", http.StatusConflict)
		return
	}
	if err := a.validateApi(st, &api); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	api.CreatedAt = a.now().UTC()
	if err := st.put(kindApi, api.ID, &api); err != nil {
		storeErr(w, "api", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, api)
}

func (a *gatewayAPI) getApi(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	api, err := st.GetApi(r.PathValue("id"))
	if err != nil {
		storeErr(w, "api", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, api)
}

func (a *gatewayAPI) patchApi(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	api, err := st.GetApi(r.PathValue("id"))
	if err != nil {
		storeErr(w, "api", err)
		return
	}
	var p struct {
		Name        *string `json:"name"`
		Description *string `json:"description"`
		Upstream    *string `json:"upstream"`
		Credential  *string `json:"credential"`
	}
	if err := decodeBody(r, &p); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if p.Name != nil {
		api.Name = *p.Name
	}
	if p.Description != nil {
		api.Description = *p.Description
	}
	if p.Upstream != nil {
		api.Upstream = *p.Upstream
	}
	if p.Credential != nil {
		api.Credential = *p.Credential
	}
	if err := a.validateApi(st, api); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := st.put(kindApi, api.ID, api); err != nil {
		storeErr(w, "api", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, api)
}

func (a *gatewayAPI) deleteApi(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	id := r.PathValue("id")
	if refs := a.refsTo(st, id, false); len(refs) > 0 {
		jsonError(w, "api is still referenced by: "+strings.Join(refs, ", "), http.StatusConflict)
		return
	}
	if err := st.remove(kindApi, id); err != nil {
		storeErr(w, "api", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// refsTo lists bundles (and for APIs, MCP servers) referencing an item.
func (a *gatewayAPI) refsTo(st *GatewayStore, id string, mcp bool) []string {
	var refs []string
	bundles, _ := listKind[Bundle](st, kindBundle)
	for _, b := range bundles {
		if (!mcp && b.hasApi(id)) || (mcp && b.hasMcpServer(id)) {
			refs = append(refs, "bundle/"+b.ID)
		}
	}
	if !mcp {
		servers, _ := listKind[McpServer](st, kindMcp)
		for _, s := range servers {
			if s.Api == id {
				refs = append(refs, "mcp-server/"+s.ID)
			}
		}
	}
	return refs
}

func (a *gatewayAPI) putOperation(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	api, err := st.GetApi(r.PathValue("id"))
	if err != nil {
		storeErr(w, "api", err)
		return
	}
	var op Operation
	if err := decodeBody(r, &op); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	op.ID = r.PathValue("op")
	if err := validateOperation(&op); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	replaced := false
	for i := range api.Operations {
		if api.Operations[i].ID == op.ID {
			api.Operations[i], replaced = op, true
		}
	}
	if !replaced {
		api.Operations = append(api.Operations, op)
	}
	if err := st.put(kindApi, api.ID, api); err != nil {
		storeErr(w, "api", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, api)
}

func (a *gatewayAPI) deleteOperation(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	api, err := st.GetApi(r.PathValue("id"))
	if err != nil {
		storeErr(w, "api", err)
		return
	}
	opID := r.PathValue("op")
	n := len(api.Operations)
	ops := api.Operations[:0]
	for _, op := range api.Operations {
		if op.ID != opID {
			ops = append(ops, op)
		}
	}
	if len(ops) == n {
		jsonError(w, "operation not found", http.StatusNotFound)
		return
	}
	api.Operations = ops
	if err := st.put(kindApi, api.ID, api); err != nil {
		storeErr(w, "api", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, api)
}

// importOpenAPI replaces an API's operations from an OpenAPI 3.x JSON document.
func (a *gatewayAPI) importOpenAPI(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	api, err := st.GetApi(r.PathValue("id"))
	if err != nil {
		storeErr(w, "api", err)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		jsonError(w, "read body", http.StatusBadRequest)
		return
	}
	ops, err := operationsFromOpenAPI(body)
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	api.Operations = ops
	if err := st.put(kindApi, api.ID, api); err != nil {
		storeErr(w, "api", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, api)
}

// --- MCP servers ---

func (a *gatewayAPI) listMcp(w http.ResponseWriter, r *http.Request) {
	listHandler[McpServer](kindMcp)(w, r)
}

func (a *gatewayAPI) createMcp(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	var in struct {
		McpServer
		// Operations is a shortcut: expose these operation ids as tools
		// named after the operation.
		Operations []string `json:"operations,omitempty"`
	}
	if err := decodeBody(r, &in); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	s := in.McpServer
	if !validID(s.ID) {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if st.exists(kindMcp, s.ID) {
		jsonError(w, "mcp server already exists", http.StatusConflict)
		return
	}
	switch s.Source {
	case McpSourceAPI, "":
		s.Source = McpSourceAPI
		api, err := st.GetApi(s.Api)
		if err != nil {
			jsonError(w, fmt.Sprintf("api %q not found", s.Api), http.StatusBadRequest)
			return
		}
		if len(in.Operations) == 1 && in.Operations[0] == "*" {
			in.Operations = nil
			for _, op := range api.Operations {
				in.Operations = append(in.Operations, op.ID)
			}
		}
		for _, id := range in.Operations {
			s.Tools = append(s.Tools, McpTool{Name: id, Operation: id})
		}
		if len(s.Tools) == 0 {
			jsonError(w, "an api-sourced server needs at least one tool", http.StatusBadRequest)
			return
		}
		seen := map[string]bool{}
		for _, t := range s.Tools {
			if findOperation(api, t.Operation) == nil {
				jsonError(w, fmt.Sprintf("operation %q not found on api %q", t.Operation, api.ID), http.StatusBadRequest)
				return
			}
			if seen[t.Name] {
				jsonError(w, "duplicate tool name "+t.Name, http.StatusBadRequest)
				return
			}
			seen[t.Name] = true
		}
	case McpSourceRemote:
		if !strings.HasPrefix(s.RemoteURL, "http://") && !strings.HasPrefix(s.RemoteURL, "https://") {
			jsonError(w, "remoteUrl must be an http(s) URL (Streamable HTTP)", http.StatusBadRequest)
			return
		}
		if err := a.checkInjectable(st, s.Credential); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
	default:
		jsonError(w, "source must be api or remote", http.StatusBadRequest)
		return
	}
	s.CreatedAt = a.now().UTC()
	if err := st.put(kindMcp, s.ID, &s); err != nil {
		storeErr(w, "mcp server", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, s)
}

func (a *gatewayAPI) getMcp(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	s, err := st.GetMcpServer(r.PathValue("id"))
	if err != nil {
		storeErr(w, "mcp server", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, s)
}

func (a *gatewayAPI) deleteMcp(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	id := r.PathValue("id")
	if refs := a.refsTo(st, id, true); len(refs) > 0 {
		jsonError(w, "mcp server is still referenced by: "+strings.Join(refs, ", "), http.StatusConflict)
		return
	}
	if err := st.remove(kindMcp, id); err != nil {
		storeErr(w, "mcp server", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Bundles ---

func (a *gatewayAPI) listBundles(w http.ResponseWriter, r *http.Request) {
	listHandler[Bundle](kindBundle)(w, r)
}

func (a *gatewayAPI) createBundle(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	var b Bundle
	if err := decodeBody(r, &b); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !validID(b.ID) {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if st.exists(kindBundle, b.ID) {
		jsonError(w, "bundle already exists", http.StatusConflict)
		return
	}
	for _, id := range b.Apis {
		if !st.exists(kindApi, id) {
			jsonError(w, fmt.Sprintf("api %q not found", id), http.StatusBadRequest)
			return
		}
	}
	for _, id := range b.McpServers {
		if !st.exists(kindMcp, id) {
			jsonError(w, fmt.Sprintf("mcp server %q not found", id), http.StatusBadRequest)
			return
		}
	}
	if b.Apis == nil {
		b.Apis = []string{}
	}
	if b.McpServers == nil {
		b.McpServers = []string{}
	}
	b.CreatedAt = a.now().UTC()
	if err := st.put(kindBundle, b.ID, &b); err != nil {
		storeErr(w, "bundle", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, b)
}

func (a *gatewayAPI) getBundle(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	b, err := st.GetBundle(r.PathValue("id"))
	if err != nil {
		storeErr(w, "bundle", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, b)
}

func (a *gatewayAPI) deleteBundle(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	id := r.PathValue("id")
	subs, _ := listKind[Subscription](st, kindSubscription)
	for _, s := range subs {
		if s.Bundle == id && s.State != SubCancelled {
			jsonError(w, "bundle has live subscriptions (cancel them first): "+s.ID, http.StatusConflict)
			return
		}
	}
	if err := st.remove(kindBundle, id); err != nil {
		storeErr(w, "bundle", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *gatewayAPI) bundleItem(add, mcp bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := ownerStore(r)
		b, err := st.GetBundle(r.PathValue("id"))
		if err != nil {
			storeErr(w, "bundle", err)
			return
		}
		item := r.PathValue("item")
		kind, list := kindApi, &b.Apis
		if mcp {
			kind, list = kindMcp, &b.McpServers
		}
		if add {
			if !st.exists(kind, item) {
				jsonError(w, item+" not found", http.StatusNotFound)
				return
			}
			if !contains(*list, item) {
				*list = append(*list, item)
			}
		} else {
			*list = without(*list, item)
		}
		if err := st.put(kindBundle, b.ID, b); err != nil {
			storeErr(w, "bundle", err)
			return
		}
		writeJSONStatus(w, http.StatusOK, b)
	}
}

func (a *gatewayAPI) bundleVisibility(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	b, err := st.GetBundle(r.PathValue("id"))
	if err != nil {
		storeErr(w, "bundle", err)
		return
	}
	var p struct {
		Hidden bool `json:"hidden"`
	}
	if err := decodeBody(r, &p); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	b.Hidden = p.Hidden
	if err := st.put(kindBundle, b.ID, b); err != nil {
		storeErr(w, "bundle", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, b)
}

// --- Principals ---

func (a *gatewayAPI) listPrincipals(w http.ResponseWriter, r *http.Request) {
	listHandler[Principal](kindPrincipal)(w, r)
}

func (a *gatewayAPI) createPrincipal(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	var p Principal
	if err := decodeBody(r, &p); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !validID(p.ID) {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if p.Kind == "" {
		p.Kind = PrincipalHuman
	}
	if p.Kind != PrincipalHuman && p.Kind != PrincipalService {
		jsonError(w, "kind must be human or service", http.StatusBadRequest)
		return
	}
	if st.exists(kindPrincipal, p.ID) {
		jsonError(w, "principal already exists", http.StatusConflict)
		return
	}
	if p.Sub != "" {
		if other, err := st.PrincipalBySub(p.Sub); err == nil {
			jsonError(w, "sub already bound to principal "+other.ID, http.StatusConflict)
			return
		}
	}
	p.Disabled = false
	p.CreatedAt = a.now().UTC()
	if err := st.put(kindPrincipal, p.ID, &p); err != nil {
		storeErr(w, "principal", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, p)
}

func (a *gatewayAPI) getPrincipal(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	p, err := st.GetPrincipal(r.PathValue("id"))
	if err != nil {
		storeErr(w, "principal", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, p)
}

func (a *gatewayAPI) setPrincipalDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := ownerStore(r)
		p, err := st.GetPrincipal(r.PathValue("id"))
		if err != nil {
			storeErr(w, "principal", err)
			return
		}
		p.Disabled = disabled
		if err := st.put(kindPrincipal, p.ID, p); err != nil {
			storeErr(w, "principal", err)
			return
		}
		writeJSONStatus(w, http.StatusOK, p)
	}
}

// --- Subscriptions ---

// subscriptionView never exposes key hashes.
type subscriptionView struct {
	ID        string     `json:"id"`
	Bundle    string     `json:"bundle"`
	Principal string     `json:"principal"`
	State     SubState   `json:"state"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	UpdatedAt time.Time  `json:"updatedAt"`
}

func (a *gatewayAPI) view(s *Subscription) subscriptionView {
	return subscriptionView{ID: s.ID, Bundle: s.Bundle, Principal: s.Principal,
		State: s.EffectiveState(a.now()), ExpiresAt: s.ExpiresAt, CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

type issuedKeys struct {
	Subscription subscriptionView `json:"subscription"`
	PrimaryKey   string           `json:"primaryKey,omitempty"`
	SecondaryKey string           `json:"secondaryKey,omitempty"`
	Note         string           `json:"note"`
}

const keysShownOnce = "Keys are shown once and stored only as hashes. Save them now."

func (a *gatewayAPI) listSubscriptions(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	subs, err := listKind[Subscription](st, kindSubscription)
	if err != nil {
		storeErr(w, "subscriptions", err)
		return
	}
	q := r.URL.Query()
	out := []subscriptionView{}
	for i := range subs {
		if p := q.Get("principal"); p != "" && subs[i].Principal != p {
			continue
		}
		if b := q.Get("bundle"); b != "" && subs[i].Bundle != b {
			continue
		}
		out = append(out, a.view(&subs[i]))
	}
	writeJSONStatus(w, http.StatusOK, out)
}

func (a *gatewayAPI) createSubscription(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	var in struct {
		ID        string     `json:"id,omitempty"`
		Bundle    string     `json:"bundle"`
		Principal string     `json:"principal"`
		ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	}
	if err := decodeBody(r, &in); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !st.exists(kindBundle, in.Bundle) {
		jsonError(w, fmt.Sprintf("bundle %q not found", in.Bundle), http.StatusBadRequest)
		return
	}
	p, err := st.GetPrincipal(in.Principal)
	if err != nil {
		jsonError(w, fmt.Sprintf("principal %q not found", in.Principal), http.StatusBadRequest)
		return
	}
	if p.Disabled {
		jsonError(w, "principal is disabled", http.StatusConflict)
		return
	}
	if in.ID == "" {
		in.ID = p.ID + "-" + in.Bundle
	}
	if !validID(in.ID) {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if st.exists(kindSubscription, in.ID) {
		jsonError(w, "subscription already exists", http.StatusConflict)
		return
	}
	now := a.now().UTC()
	pk, ph := newSubscriptionKey(in.ID)
	sk, sh := newSubscriptionKey(in.ID)
	s := &Subscription{ID: in.ID, Bundle: in.Bundle, Principal: p.ID, State: SubActive,
		PrimaryHash: ph, SecondaryHash: sh, ExpiresAt: in.ExpiresAt, CreatedAt: now, UpdatedAt: now}
	if err := st.put(kindSubscription, s.ID, s); err != nil {
		storeErr(w, "subscription", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, issuedKeys{Subscription: a.view(s), PrimaryKey: pk, SecondaryKey: sk, Note: keysShownOnce})
}

func (a *gatewayAPI) getSubscription(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	s, err := st.GetSubscription(r.PathValue("id"))
	if err != nil {
		storeErr(w, "subscription", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, a.view(s))
}

func (a *gatewayAPI) transition(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := ownerStore(r)
		s, err := st.GetSubscription(r.PathValue("id"))
		if err != nil {
			storeErr(w, "subscription", err)
			return
		}
		state := s.EffectiveState(a.now())
		var next SubState
		switch {
		case action == "suspend" && state == SubActive:
			next = SubSuspended
		case action == "reactivate" && state == SubSuspended:
			next = SubActive
		case action == "cancel" && (state == SubActive || state == SubSuspended):
			next = SubCancelled
		default:
			jsonError(w, fmt.Sprintf("cannot %s a subscription that is %s", action, state), http.StatusConflict)
			return
		}
		s.State, s.UpdatedAt = next, a.now().UTC()
		if err := st.put(kindSubscription, s.ID, s); err != nil {
			storeErr(w, "subscription", err)
			return
		}
		writeJSONStatus(w, http.StatusOK, a.view(s))
	}
}

// regenerate replaces one key slot; self=true restricts to the caller's own.
func (a *gatewayAPI) regenerate(self bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := ownerStore(r)
		s, err := st.GetSubscription(r.PathValue("id"))
		if err != nil {
			storeErr(w, "subscription", err)
			return
		}
		if self && !a.ownsSubscription(r, st, s) {
			jsonError(w, "subscription not found", http.StatusNotFound)
			return
		}
		var p struct {
			Slot string `json:"slot"`
		}
		if err := decodeBody(r, &p); err != nil {
			jsonError(w, err.Error(), http.StatusBadRequest)
			return
		}
		state := s.EffectiveState(a.now())
		if state != SubActive && state != SubSuspended {
			jsonError(w, "cannot regenerate keys of a subscription that is "+string(state), http.StatusConflict)
			return
		}
		key, hash := newSubscriptionKey(s.ID)
		out := issuedKeys{Note: keysShownOnce}
		switch p.Slot {
		case "primary":
			s.PrimaryHash, out.PrimaryKey = hash, key
		case "secondary":
			s.SecondaryHash, out.SecondaryKey = hash, key
		default:
			jsonError(w, `slot must be "primary" or "secondary"`, http.StatusBadRequest)
			return
		}
		s.UpdatedAt = a.now().UTC()
		if err := st.put(kindSubscription, s.ID, s); err != nil {
			storeErr(w, "subscription", err)
			return
		}
		out.Subscription = a.view(s)
		writeJSONStatus(w, http.StatusOK, out)
	}
}

func callerPrincipal(r *http.Request, st *GatewayStore) *Principal {
	c := claimsFromCtx(r.Context())
	if c == nil {
		return nil
	}
	p, err := st.PrincipalBySub(c.Sub)
	if err != nil {
		return nil
	}
	return p
}

func (a *gatewayAPI) ownsSubscription(r *http.Request, st *GatewayStore, s *Subscription) bool {
	p := callerPrincipal(r, st)
	return p != nil && s.Principal == p.ID
}

type mySubscription struct {
	Owner string `json:"owner"`
	subscriptionView
}

// mySubscriptions spans every owner where the caller's sub is a principal.
func (a *gatewayAPI) mySubscriptions(w http.ResponseWriter, r *http.Request) {
	out := []mySubscription{}
	for _, st := range a.root.Owners() {
		p := callerPrincipal(r, st)
		if p == nil {
			continue
		}
		subs, _ := listKind[Subscription](st, kindSubscription)
		for i := range subs {
			if subs[i].Principal == p.ID {
				out = append(out, mySubscription{Owner: st.owner, subscriptionView: a.view(&subs[i])})
			}
		}
	}
	writeJSONStatus(w, http.StatusOK, out)
}

// --- Credentials ---

func (a *gatewayAPI) listCredentials(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	cs, err := listKind[Credential](st, kindCredential)
	if err != nil {
		storeErr(w, "credentials", err)
		return
	}
	out := make([]credentialView, 0, len(cs))
	for i := range cs {
		out = append(out, cs[i].view())
	}
	writeJSONStatus(w, http.StatusOK, out)
}

type credentialInput struct {
	ID           string               `json:"id"`
	Source       CredentialSourceKind `json:"source"`
	Inject       Injection            `json:"inject"`
	Var          string               `json:"var,omitempty"`
	Value        string               `json:"value,omitempty"` // sealed only; write-only
	Item         string               `json:"item,omitempty"`
	Tenant       string               `json:"tenant,omitempty"`
	ClientID     string               `json:"clientId,omitempty"`
	ClientSecret string               `json:"clientSecret,omitempty"`
	Scope        string               `json:"scope,omitempty"`
}

func (a *gatewayAPI) createCredential(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	var in credentialInput
	if err := decodeBody(r, &in); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !validID(in.ID) {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if st.exists(kindCredential, in.ID) {
		jsonError(w, "credential already exists", http.StatusConflict)
		return
	}
	now := a.now().UTC()
	c := &Credential{ID: in.ID, Source: in.Source, Inject: in.Inject, CreatedAt: now, UpdatedAt: now}
	// An entra client secret is referenced, never injected — it needs no inject spec.
	if !in.Inject.valid() && !(in.Inject == Injection{}) {
		jsonError(w, "inject must set exactly one of header, query, bearer", http.StatusBadRequest)
		return
	}
	switch in.Source {
	case CredEnv:
		if in.Var == "" {
			jsonError(w, "env credential needs var", http.StatusBadRequest)
			return
		}
		c.Var = in.Var
	case CredSealed:
		if a.sealer == nil {
			jsonError(w, "GATEWAY_SEAL_KEY is not set on the gateway; sealed credentials are unavailable", http.StatusConflict)
			return
		}
		if in.Value == "" {
			jsonError(w, "sealed credential needs value", http.StatusBadRequest)
			return
		}
		sealed, err := a.sealer.Seal(in.Value, c.ID)
		if err != nil {
			jsonError(w, "seal failed", http.StatusInternalServerError)
			return
		}
		c.Sealed = sealed
	case CredEntra:
		if in.Tenant == "" || in.ClientID == "" || in.ClientSecret == "" || in.Scope == "" {
			jsonError(w, "entra credential needs tenant, clientId, clientSecret (credential id) and scope", http.StatusBadRequest)
			return
		}
		if !st.exists(kindCredential, in.ClientSecret) {
			jsonError(w, fmt.Sprintf("clientSecret credential %q not found", in.ClientSecret), http.StatusBadRequest)
			return
		}
		if in.Inject == (Injection{}) {
			c.Inject = Injection{Bearer: true}
		}
		c.Tenant, c.ClientID, c.ClientSecret, c.Scope = in.Tenant, in.ClientID, in.ClientSecret, in.Scope
	case CredVault:
		if in.Item == "" {
			jsonError(w, "vault credential needs item (<owner>/<item>)", http.StatusBadRequest)
			return
		}
		c.Item = in.Item
	default:
		jsonError(w, "source must be env, sealed, entra or vault", http.StatusBadRequest)
		return
	}
	if in.Value != "" && in.Source != CredSealed {
		jsonError(w, "value is only accepted for sealed credentials", http.StatusBadRequest)
		return
	}
	if err := st.put(kindCredential, c.ID, c); err != nil {
		storeErr(w, "credential", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, c.view())
}

func (a *gatewayAPI) getCredential(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	c, err := st.GetCredential(r.PathValue("id"))
	if err != nil {
		storeErr(w, "credential", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, c.view())
}

func (a *gatewayAPI) deleteCredential(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	id := r.PathValue("id")
	var refs []string
	apis, _ := listKind[Api](st, kindApi)
	for _, x := range apis {
		if x.Credential == id {
			refs = append(refs, "api/"+x.ID)
		}
	}
	servers, _ := listKind[McpServer](st, kindMcp)
	for _, x := range servers {
		if x.Credential == id {
			refs = append(refs, "mcp-server/"+x.ID)
		}
	}
	creds, _ := listKind[Credential](st, kindCredential)
	for _, x := range creds {
		if x.ClientSecret == id {
			refs = append(refs, "credential/"+x.ID)
		}
	}
	if len(refs) > 0 {
		jsonError(w, "credential is still referenced by: "+strings.Join(refs, ", "), http.StatusConflict)
		return
	}
	if err := st.remove(kindCredential, id); err != nil {
		storeErr(w, "credential", err)
		return
	}
	a.creds.Forget(st, id)
	w.WriteHeader(http.StatusNoContent)
}

// rotateCredential replaces a sealed value (FR-15); other sources rotate at
// their origin, and this just drops any cached token.
func (a *gatewayAPI) rotateCredential(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	c, err := st.GetCredential(r.PathValue("id"))
	if err != nil {
		storeErr(w, "credential", err)
		return
	}
	var p struct {
		Value string `json:"value,omitempty"`
	}
	if err := decodeBody(r, &p); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if c.Source == CredSealed {
		if a.sealer == nil || p.Value == "" {
			jsonError(w, "sealed rotation needs value and GATEWAY_SEAL_KEY", http.StatusBadRequest)
			return
		}
		sealed, err := a.sealer.Seal(p.Value, c.ID)
		if err != nil {
			jsonError(w, "seal failed", http.StatusInternalServerError)
			return
		}
		c.Sealed = sealed
	} else if p.Value != "" {
		jsonError(w, "value is only accepted for sealed credentials", http.StatusBadRequest)
		return
	}
	c.UpdatedAt = a.now().UTC()
	if err := st.put(kindCredential, c.ID, c); err != nil {
		storeErr(w, "credential", err)
		return
	}
	a.creds.Forget(st, c.ID)
	writeJSONStatus(w, http.StatusOK, c.view())
}

// --- Usage ---

type usageTotals struct {
	Calls     int   `json:"calls"`
	Errors    int   `json:"errors"`
	BytesOut  int64 `json:"bytesOut"`
	LatencyMs int64 `json:"latencyMsTotal"`
}

func (a *gatewayAPI) usage(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	q := r.URL.Query()
	recs, err := st.ReadUsage(q.Get("from"), q.Get("to"))
	if err != nil {
		storeErr(w, "usage", err)
		return
	}
	filtered := []UsageRecord{}
	bySub := map[string]*usageTotals{}
	total := &usageTotals{}
	for _, rec := range recs {
		if (q.Get("subscription") != "" && rec.Subscription != q.Get("subscription")) ||
			(q.Get("bundle") != "" && rec.Bundle != q.Get("bundle")) ||
			(q.Get("principal") != "" && rec.Principal != q.Get("principal")) ||
			(q.Get("api") != "" && rec.Api != q.Get("api")) {
			continue
		}
		filtered = append(filtered, rec)
		t := bySub[rec.Subscription]
		if t == nil {
			t = &usageTotals{}
			bySub[rec.Subscription] = t
		}
		for _, x := range []*usageTotals{t, total} {
			x.Calls++
			if rec.Status >= 400 {
				x.Errors++
			}
			x.BytesOut += rec.BytesOut
			x.LatencyMs += rec.LatencyMs
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Time.Before(filtered[j].Time) })
	out := map[string]any{"total": total, "bySubscription": bySub}
	if q.Get("records") == "1" {
		out["records"] = filtered
	}
	writeJSONStatus(w, http.StatusOK, out)
}

// --- Catalog ---

type catalogBundle struct {
	ID          string       `json:"id"`
	Name        string       `json:"name,omitempty"`
	Description string       `json:"description,omitempty"`
	Hidden      bool         `json:"hidden,omitempty"`
	Apis        []catalogApi `json:"apis"`
	McpServers  []catalogMcp `json:"mcpServers"`
}

type catalogApi struct {
	ID         string   `json:"id"`
	Name       string   `json:"name,omitempty"`
	Kind       ApiKind  `json:"kind"`
	Path       string   `json:"path"`
	Operations []string `json:"operations"`
}

type catalogMcp struct {
	ID    string   `json:"id"`
	Name  string   `json:"name,omitempty"`
	Path  string   `json:"path"`
	Tools []string `json:"tools,omitempty"`
}

func (a *gatewayAPI) catalog(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	admin := isOwnerAdmin(claimsFromCtx(r.Context()), st)
	bundles, err := listKind[Bundle](st, kindBundle)
	if err != nil {
		storeErr(w, "catalog", err)
		return
	}
	out := []catalogBundle{}
	for i := range bundles {
		if bundles[i].Hidden && !admin {
			continue
		}
		out = append(out, catalogEntry(st, &bundles[i]))
	}
	writeJSONStatus(w, http.StatusOK, out)
}

// catalogEntry describes one bundle with the owner-qualified paths to call.
func catalogEntry(st *GatewayStore, b *Bundle) catalogBundle {
	cb := catalogBundle{ID: b.ID, Name: b.Name, Description: b.Description, Hidden: b.Hidden,
		Apis: []catalogApi{}, McpServers: []catalogMcp{}}
	for _, id := range b.Apis {
		if api, err := st.GetApi(id); err == nil {
			ca := catalogApi{ID: api.ID, Name: api.Name, Kind: api.Kind, Path: "/apis/" + st.owner + "/" + api.ID, Operations: []string{}}
			for _, op := range api.Operations {
				ca.Operations = append(ca.Operations, op.Method+" "+op.Path)
			}
			cb.Apis = append(cb.Apis, ca)
		}
	}
	for _, id := range b.McpServers {
		if s, err := st.GetMcpServer(id); err == nil {
			cm := catalogMcp{ID: s.ID, Name: s.Name, Path: "/mcp/" + st.owner + "/" + s.ID}
			for _, t := range s.Tools {
				cm.Tools = append(cm.Tools, t.Name)
			}
			cb.McpServers = append(cb.McpServers, cm)
		}
	}
	return cb
}
