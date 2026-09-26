package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// MCP endpoint (PRD F6/F7, FR-4..6a): POST /mcp/{server}, Streamable HTTP,
// stateless JSON responses. source=api servers turn Api operations into
// tools; source=remote servers are proxied with the credential injected.

const mcpProtocolVersion = "2025-06-18"

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any, rerr *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	resp := map[string]any{"jsonrpc": "2.0", "id": id}
	if rerr != nil {
		resp["error"] = rerr
	} else {
		resp["result"] = result
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// resourceURL is the canonical URL of an MCP server (the OAuth audience).
func (l *Lane) resourceURL(r *http.Request, owner, server string) string {
	base := l.publicBaseURL
	if base == "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		host := r.Header.Get("X-Forwarded-Host")
		if host == "" {
			host = r.Host
		}
		base = scheme + "://" + host
	}
	return strings.TrimRight(base, "/") + "/mcp/" + owner + "/" + server
}

// ServeProtectedResourceMetadata answers RFC 9728 metadata for an MCP server.
func (l *Lane) ServeProtectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	server := r.PathValue("server")
	st, err := l.root.Owner(r.PathValue("owner"))
	if err != nil || l.oauthIssuer == "" || !st.exists(kindMcp, server) {
		jsonError(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"resource":                 l.resourceURL(r, st.owner, server),
		"authorization_servers":    []string{l.oauthIssuer},
		"bearer_methods_supported": []string{"header"},
	})
}

func (l *Lane) unauthorizedMCP(w http.ResponseWriter, r *http.Request, owner, server, msg string) {
	if l.oauthIssuer != "" {
		meta := strings.Replace(l.resourceURL(r, owner, server), "/mcp/", "/.well-known/oauth-protected-resource/mcp/", 1)
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata=%q`, meta))
	}
	jsonError(w, msg, http.StatusUnauthorized)
}

func (l *Lane) ServeMCP(w http.ResponseWriter, r *http.Request) {
	start := l.now()
	serverID := r.PathValue("server")
	st, err := l.root.Owner(r.PathValue("owner"))
	if err != nil {
		jsonError(w, "no such mcp server", http.StatusNotFound)
		return
	}
	srv, err := st.GetMcpServer(serverID)
	if err != nil {
		jsonError(w, "no such mcp server", http.StatusNotFound)
		return
	}
	covers := func(b *Bundle) bool { return b.hasMcpServer(srv.ID) }

	var g *grant
	var d *denial
	if key, ok := presentedGatewayKey(r); ok {
		g, d = l.authorizeKey(st, key, covers)
	} else if tok, ok := bearerToken(r); ok && l.oauthIssuer != "" {
		g, d = l.authorizeOAuth(r.Context(), st, tok, l.resourceURL(r, st.owner, srv.ID), covers)
	} else {
		l.unauthorizedMCP(w, r, st.owner, srv.ID, "missing gateway key or access token")
		return
	}
	if d != nil {
		if d.status == http.StatusUnauthorized {
			l.unauthorizedMCP(w, r, st.owner, srv.ID, d.msg)
			return
		}
		jsonError(w, d.msg, d.status)
		return
	}

	if r.Method != http.MethodPost {
		// Stateless server: no server-initiated SSE stream, no sessions.
		w.Header().Set("Allow", "POST")
		jsonError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		jsonError(w, "read body", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil || req.Method == "" {
		writeRPC(w, nil, nil, &rpcError{-32700, "parse error (batches are not supported)"})
		return
	}

	rec := UsageRecord{Subscription: g.sub.ID, Principal: g.principal.ID, Bundle: g.bundle.ID,
		McpServer: srv.ID, Method: req.Method}

	if srv.Source == McpSourceRemote {
		l.proxyRemoteMCP(w, r, st, srv, &req, body, rec, start)
		return
	}

	switch {
	case strings.HasPrefix(req.Method, "notifications/"):
		w.WriteHeader(http.StatusAccepted)
	case req.Method == "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := p.ProtocolVersion
		if v == "" {
			v = mcpProtocolVersion
		}
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": v,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": srv.ID, "version": "1"},
			"instructions":    srv.Description,
		}, nil)
	case req.Method == "ping":
		writeRPC(w, req.ID, map[string]any{}, nil)
	case req.Method == "tools/list":
		tools, err := l.apiTools(st, srv)
		if err != nil {
			writeRPC(w, req.ID, nil, &rpcError{-32603, err.Error()})
			return
		}
		writeRPC(w, req.ID, map[string]any{"tools": tools}, nil)
	case req.Method == "tools/call":
		l.callAPITool(w, r, st, srv, &req, rec, start)
	default:
		writeRPC(w, req.ID, nil, &rpcError{-32601, "method not found: " + req.Method})
	}
}

type toolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

func (l *Lane) apiTools(st *GatewayStore, srv *McpServer) ([]toolDef, error) {
	api, err := st.GetApi(srv.Api)
	if err != nil {
		return nil, fmt.Errorf("api %q not found", srv.Api)
	}
	out := []toolDef{}
	for _, t := range srv.Tools {
		op := findOperation(api, t.Operation)
		if op == nil {
			continue
		}
		desc := t.Description
		if desc == "" {
			desc = op.Description
		}
		if desc == "" {
			desc = op.Summary
		}
		out = append(out, toolDef{Name: t.Name, Description: desc, InputSchema: toolSchema(op)})
	}
	return out, nil
}

func findOperation(api *Api, id string) *Operation {
	for i := range api.Operations {
		if api.Operations[i].ID == id {
			return &api.Operations[i]
		}
	}
	return nil
}

// toolSchema is the operation's declared schema, or one derived from its path
// parameters plus an optional free-form body.
func toolSchema(op *Operation) json.RawMessage {
	if len(op.InputSchema) > 0 {
		return op.InputSchema
	}
	props := map[string]any{}
	var required []string
	for _, seg := range splitPath(op.Path) {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			name := seg[1 : len(seg)-1]
			props[name] = map[string]any{"type": "string"}
			required = append(required, name)
		}
	}
	if m := strings.ToUpper(op.Method); m != http.MethodGet && m != http.MethodDelete {
		props["body"] = map[string]any{"type": "object", "description": "JSON request body"}
	}
	schema := map[string]any{"type": "object", "properties": props}
	if len(required) > 0 {
		schema["required"] = required
	}
	b, _ := json.Marshal(schema)
	return b
}

func (l *Lane) callAPITool(w http.ResponseWriter, r *http.Request, st *GatewayStore, srv *McpServer, req *rpcRequest, rec UsageRecord, start time.Time) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil {
		writeRPC(w, req.ID, nil, &rpcError{-32602, "invalid params"})
		return
	}
	var tool *McpTool
	for i := range srv.Tools {
		if srv.Tools[i].Name == p.Name {
			tool = &srv.Tools[i]
		}
	}
	api, err := st.GetApi(srv.Api)
	if tool == nil || err != nil {
		writeRPC(w, req.ID, nil, &rpcError{-32602, "unknown tool: " + p.Name})
		return
	}
	op := findOperation(api, tool.Operation)
	if op == nil {
		writeRPC(w, req.ID, nil, &rpcError{-32602, "unknown tool: " + p.Name})
		return
	}
	rec.Api, rec.Tool, rec.Operation = api.ID, tool.Name, op.ID

	target, rt, err := l.upstreamTarget(st, api, r.RemoteAddr)
	if errors.Is(err, errConnectorOffline) {
		l.record(st, rec, http.StatusServiceUnavailable, start, 0)
		writeRPC(w, req.ID, toolResult("connector offline", true), nil)
		return
	}
	if err != nil {
		writeRPC(w, req.ID, toolResult("api upstream misconfigured", true), nil)
		return
	}
	out, err := buildToolRequest(r, target, op, p.Arguments)
	if err != nil {
		writeRPC(w, req.ID, nil, &rpcError{-32602, err.Error()})
		return
	}
	cred, inject, err := l.credentialFor(r.Context(), st, api.Credential)
	if err != nil {
		log.Printf("lane: mcp=%s tool=%s: %v", srv.ID, tool.Name, err)
		l.record(st, rec, http.StatusBadGateway, start, 0)
		writeRPC(w, req.ID, toolResult("upstream credential unavailable", true), nil)
		return
	}
	if cred != "" {
		inject.apply(out, cred)
	}
	client := l.creds.client
	if rt != nil {
		out.Header.Set(gatewayContextHeader, gatewayContext(rec))
		client = &http.Client{Transport: rt, Timeout: client.Timeout}
	}
	resp, err := client.Do(out)
	if err != nil {
		log.Printf("lane: mcp=%s tool=%s: upstream request failed", srv.ID, tool.Name)
		l.record(st, rec, http.StatusBadGateway, start, 0)
		writeRPC(w, req.ID, toolResult("upstream request failed", true), nil)
		return
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	l.record(st, rec, resp.StatusCode, start, int64(len(data)))
	text := string(data)
	if resp.StatusCode >= 400 {
		text = fmt.Sprintf("upstream returned %s\n%s", resp.Status, text)
	}
	writeRPC(w, req.ID, toolResult(text, resp.StatusCode >= 400), nil)
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// buildToolRequest maps tool arguments onto the operation: path parameters
// fill the template; for GET/DELETE the rest become query parameters,
// otherwise "body" (or the remaining arguments) becomes the JSON body.
func buildToolRequest(r *http.Request, target *url.URL, op *Operation, args map[string]any) (*http.Request, error) {
	rest := map[string]any{}
	for k, v := range args {
		rest[k] = v
	}
	var segs []string
	for _, seg := range splitPath(op.Path) {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			name := seg[1 : len(seg)-1]
			v, ok := rest[name]
			if !ok {
				return nil, fmt.Errorf("missing argument %q", name)
			}
			delete(rest, name)
			segs = append(segs, url.PathEscape(fmt.Sprint(v)))
			continue
		}
		segs = append(segs, seg)
	}
	u := *target
	u.Path = joinURLPath(target.Path, "/"+strings.Join(segs, "/"))
	u.RawPath = ""

	method := strings.ToUpper(op.Method)
	var body io.Reader
	toQuery := func(m map[string]any) {
		q := u.Query()
		for k, v := range m {
			q.Set(k, fmt.Sprint(v))
		}
		u.RawQuery = q.Encode()
	}
	if method == http.MethodGet || method == http.MethodDelete {
		toQuery(rest)
	} else {
		// An explicit "body" argument is the body and the rest are query
		// parameters (the OpenAPI-import shape); otherwise all remaining
		// arguments form the JSON body.
		var payload any = rest
		if b, ok := rest["body"]; ok {
			payload = b
			delete(rest, "body")
			toQuery(rest)
		}
		if s, ok := payload.(string); ok {
			body = strings.NewReader(s)
		} else {
			b, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}
			body = bytes.NewReader(b)
		}
	}
	out, err := http.NewRequestWithContext(r.Context(), method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		ct := op.BodyContentType
		if ct == "" {
			ct = "application/json"
		}
		out.Header.Set("Content-Type", ct)
	}
	out.Header.Set("Accept", "application/json")
	return out, nil
}

// proxyRemoteMCP forwards to a remote Streamable-HTTP MCP server with the
// credential injected, enforcing the tool allow-list on tools/call.
func (l *Lane) proxyRemoteMCP(w http.ResponseWriter, r *http.Request, st *GatewayStore, srv *McpServer, req *rpcRequest, body []byte, rec UsageRecord, start time.Time) {
	if req.Method == "tools/call" {
		var p struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(req.Params, &p)
		rec.Tool = p.Name
		if len(srv.AllowTools) > 0 && !contains(srv.AllowTools, p.Name) {
			writeRPC(w, req.ID, nil, &rpcError{-32602, "tool not allowed: " + p.Name})
			return
		}
	}
	target, err := url.Parse(srv.RemoteURL)
	if err != nil {
		jsonError(w, "mcp remote misconfigured", http.StatusBadGateway)
		return
	}
	cred, inject, err := l.credentialFor(r.Context(), st, srv.Credential)
	if err != nil {
		log.Printf("lane: mcp=%s: %v", srv.ID, err)
		jsonError(w, "upstream credential unavailable", http.StatusBadGateway)
		l.record(st, rec, http.StatusBadGateway, start, 0)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	proxy := &httputil.ReverseProxy{
		FlushInterval: -1,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = target.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = target.Host
			stripPresentedCredentials(pr.Out.Header)
			pr.Out.Header.Del("Cookie")
			if cred != "" {
				inject.apply(pr.Out, cred)
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			log.Printf("lane: mcp=%s: remote request failed", srv.ID)
			jsonError(w, "upstream request failed", http.StatusBadGateway)
		},
	}
	proxy.ServeHTTP(sw, r)
	l.record(st, rec, sw.status, start, sw.bytes)
}
