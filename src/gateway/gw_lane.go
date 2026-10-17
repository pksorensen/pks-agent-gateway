package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The subscription lane (ADR 0001): gateway-keyed traffic to registered APIs
// (/apis/{api}/…) and MCP servers (/mcp/{server}). It is the only lane that
// rewrites credentials: the presented key is stripped and the API's
// Credential is injected. It never reaches the passthrough UPSTREAM.

// TokenVerifier validates an OAuth bearer JWT for MCP (PRD FR-6a) and returns
// its subject and audiences. The gateway is a protected resource only.
type TokenVerifier func(ctx context.Context, raw string) (sub string, aud []string, err error)

type Lane struct {
	root  *GatewayRoot
	hub   *connectorHub
	creds *CredentialResolver
	now   func() time.Time

	// MCP OAuth (optional). issuer empty = key-only.
	oauthIssuer   string
	oauthAudience string // extra accepted audience (Keycloak mapper value)
	verify        TokenVerifier
	publicBaseURL string // e.g. https://gw.example.com; empty = derive from request

	// entra is the entra-jwt caller verifier for /apis/ (ADR 0006); nil = off.
	entra *entraJWTVerifier
	// limits enforces per-principal caps; defaultMaxBody applies to
	// operations without maxBodyBytes (-1 there = no cap).
	limits         *usageLimiter
	defaultMaxBody int64
	// transport reaches http(s) upstreams; nil = http.DefaultTransport.
	transport http.RoundTripper
}

func NewLane(root *GatewayRoot, creds *CredentialResolver) *Lane {
	return &Lane{root: root, hub: newConnectorHub(), creds: creds, now: time.Now,
		limits: newUsageLimiter(), defaultMaxBody: defaultMaxBodyBytes}
}

// grant is the outcome of a successful authorization.
type grant struct {
	st        *GatewayStore
	sub       *Subscription
	principal *Principal
	bundle    *Bundle
}

type denial struct {
	status int
	msg    string
}

func (d *denial) Error() string { return d.msg }

// authorizeKey checks a gwk_ key against a target the bundle must contain.
// The key is only ever checked against the owner named in the URL.
func (l *Lane) authorizeKey(st *GatewayStore, key string, covers func(*Bundle) bool) (*grant, *denial) {
	subID, ok := parseSubscriptionKey(key)
	if !ok {
		return nil, &denial{http.StatusUnauthorized, "invalid gateway key"}
	}
	sub, err := st.GetSubscription(subID)
	if err != nil || !keyMatches(sub, key) {
		return nil, &denial{http.StatusUnauthorized, "invalid gateway key"}
	}
	return l.checkGrant(st, sub, covers)
}

func (l *Lane) checkGrant(st *GatewayStore, sub *Subscription, covers func(*Bundle) bool) (*grant, *denial) {
	if state := sub.EffectiveState(l.now()); state != SubActive {
		return nil, &denial{http.StatusForbidden, "subscription is " + string(state)}
	}
	p, err := st.GetPrincipal(sub.Principal)
	if err != nil || p.Disabled {
		return nil, &denial{http.StatusForbidden, "principal is disabled"}
	}
	b, err := st.GetBundle(sub.Bundle)
	if err != nil || !covers(b) {
		return nil, &denial{http.StatusForbidden, "subscription does not cover this resource"}
	}
	return &grant{st: st, sub: sub, principal: p, bundle: b}, nil
}

// authorizeOAuth maps a verified JWT subject to the principal's active
// subscription on a bundle covering the target.
func (l *Lane) authorizeOAuth(ctx context.Context, st *GatewayStore, raw, resource string, covers func(*Bundle) bool) (*grant, *denial) {
	sub, aud, err := l.verify(ctx, raw)
	if err != nil {
		return nil, &denial{http.StatusUnauthorized, "invalid access token"}
	}
	if !contains(aud, resource) && (l.oauthAudience == "" || !contains(aud, l.oauthAudience)) {
		return nil, &denial{http.StatusUnauthorized, "access token audience does not match this resource"}
	}
	p, err := st.PrincipalBySub(sub)
	if err != nil {
		return nil, &denial{http.StatusForbidden, "no principal for this identity"}
	}
	return l.grantForPrincipal(st, p, covers)
}

// grantForPrincipal finds an active subscription of p whose bundle covers the
// target. Used by token-authenticated callers, who present no key.
func (l *Lane) grantForPrincipal(st *GatewayStore, p *Principal, covers func(*Bundle) bool) (*grant, *denial) {
	subs, err := listKind[Subscription](st, kindSubscription)
	if err != nil {
		return nil, &denial{http.StatusInternalServerError, "store error"}
	}
	var last *denial
	for i := range subs {
		if subs[i].Principal != p.ID {
			continue
		}
		g, d := l.checkGrant(st, &subs[i], covers)
		if d == nil {
			return g, nil
		}
		last = d
	}
	if last == nil {
		last = &denial{http.StatusForbidden, "no subscription covers this resource"}
	}
	return nil, last
}

// --- Key-authenticated catalog: GET /apis/{owner}/_catalog (ADR 0005) ---

// ServeCatalog tells a key holder what their key grants, without an
// operator login: the subscription and its bundle with callable paths.
func (l *Lane) ServeCatalog(w http.ResponseWriter, r *http.Request) {
	st, err := l.root.Owner(r.PathValue("owner"))
	if err != nil {
		jsonError(w, "invalid gateway key", http.StatusUnauthorized)
		return
	}
	key, ok := presentedGatewayKey(r)
	if !ok {
		jsonError(w, "missing gateway key", http.StatusUnauthorized)
		return
	}
	g, d := l.authorizeKey(st, key, func(*Bundle) bool { return true })
	if d != nil {
		jsonError(w, d.msg, d.status)
		return
	}
	writeJSONStatus(w, http.StatusOK, map[string]any{
		"owner":        st.owner,
		"subscription": g.sub.ID,
		"principal":    g.principal.ID,
		"bundle":       catalogEntry(st, g.bundle),
	})
}

// --- HTTP APIs: /apis/{owner}/{api}/{rest...} ---

func (l *Lane) ServeAPI(w http.ResponseWriter, r *http.Request) {
	start := l.now()
	st, err := l.root.Owner(r.PathValue("owner"))
	if err != nil {
		jsonError(w, "no such api", http.StatusNotFound)
		return
	}
	apiID := r.PathValue("api")
	rest := "/" + r.PathValue("rest")

	// A gwk_ key, or (when configured) an Entra app-only access token.
	var authorize func(covers func(*Bundle) bool) (*grant, *denial)
	if key, ok := presentedGatewayKey(r); ok {
		authorize = func(covers func(*Bundle) bool) (*grant, *denial) { return l.authorizeKey(st, key, covers) }
	} else if tok, ok := bearerEntraToken(r); ok && l.entra != nil {
		authorize = func(covers func(*Bundle) bool) (*grant, *denial) {
			return l.authorizeEntra(r.Context(), st, tok, covers)
		}
	} else {
		jsonError(w, "missing gateway key", http.StatusUnauthorized)
		return
	}
	api, err := st.GetApi(apiID)
	if err != nil {
		// Same answer as a bad credential: don't reveal which API ids exist.
		if _, d := authorize(func(*Bundle) bool { return false }); d != nil && d.status == http.StatusUnauthorized {
			jsonError(w, d.msg, d.status)
			return
		}
		jsonError(w, "no such api", http.StatusNotFound)
		return
	}
	g, d := authorize(func(b *Bundle) bool { return b.hasApi(api.ID) })
	if d != nil {
		jsonError(w, d.msg, d.status)
		return
	}
	op := matchOperation(api.Operations, r.Method, rest)
	if op == nil {
		// FR-3: undeclared routes are answered locally, never forwarded.
		jsonError(w, "no such operation", http.StatusNotFound)
		return
	}
	rec := UsageRecord{Subscription: g.sub.ID, Principal: g.principal.ID, Bundle: g.bundle.ID,
		Api: api.ID, Operation: op.ID, Method: r.Method}
	if op.WebSocket {
		l.serveStream(w, r, st, g, api, op, rest, rec, start)
		return
	}

	// Body cap and metering are decided from Content-Length before anything
	// is read, and before an upstream token is minted for a doomed request.
	maxBody := op.MaxBodyBytes
	if maxBody == 0 {
		maxBody = l.defaultMaxBody
	}
	if maxBody > 0 && r.ContentLength > maxBody {
		jsonError(w, fmt.Sprintf("request body exceeds %d bytes", maxBody), http.StatusRequestEntityTooLarge)
		l.record(st, rec, http.StatusRequestEntityTooLarge, start, 0)
		return
	}
	if op.Meter == meterAudio {
		if r.ContentLength < 0 {
			jsonError(w, "metered operation needs Content-Length", http.StatusLengthRequired)
			l.record(st, rec, http.StatusLengthRequired, start, 0)
			return
		}
		rec.AudioSeconds = estimateAudioSeconds(r.ContentLength, op.AudioBytesPerSecond)
	}
	if d, retryAfter := l.limits.admit(st, g.principal, l.now(), rec.AudioSeconds); d != nil {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		jsonError(w, d.msg, d.status)
		rec.AudioSeconds = 0 // not charged
		l.record(st, rec, d.status, start, 0)
		return
	}
	if maxBody > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, maxBody) // a lying chunked body still stops at the cap
	}
	body := &countingBody{ReadCloser: r.Body}
	r.Body = body

	cred, inject, err := l.credentialFor(r.Context(), st, api.Credential)
	if err != nil {
		log.Printf("lane: owner=%s api=%s op=%s: %v", st.owner, api.ID, op.ID, err)
		jsonError(w, "upstream credential unavailable", http.StatusBadGateway)
		l.record(st, rec, http.StatusBadGateway, start, 0)
		return
	}
	target, rt, err := l.upstreamTarget(st, api, r.RemoteAddr)
	if errors.Is(err, errConnectorOffline) {
		// ADR 0004: answered locally, never a passthrough.
		jsonError(w, "connector offline", http.StatusServiceUnavailable)
		l.record(st, rec, http.StatusServiceUnavailable, start, 0)
		return
	}
	if err != nil {
		jsonError(w, "api upstream misconfigured", http.StatusBadGateway)
		return
	}

	sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
	proxy := l.proxy(api, op, rest, r.URL.RawQuery, target, rt, cred, inject, rec)
	proxy.ServeHTTP(sw, r)
	rec.BytesIn = body.n
	l.record(st, rec, sw.status, start, sw.bytes)
}

// proxy forwards one declared operation: the API's credential is injected,
// presented credentials and cookies are stripped, the query is kept. A
// protocol upgrade passes only for a WebSocket operation; ReverseProxy
// handles the 101 and splices the two connections.
func (l *Lane) proxy(api *Api, op *Operation, rest, rawQuery string, target *url.URL, rt http.RoundTripper, cred string, inject Injection, rec UsageRecord) *httputil.ReverseProxy {
	runner := rt != nil
	if rt == nil {
		rt = l.transport // nil = http.DefaultTransport
	}
	return &httputil.ReverseProxy{
		FlushInterval: -1, // streaming — same invariant as the passthrough proxy
		Transport:     rt, // nil = default; a yamux stream for runner:// upstreams
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = joinURLPath(target.Path, rest)
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = rawQuery
			pr.Out.Host = target.Host
			stripPresentedCredentials(pr.Out.Header)
			pr.Out.Header.Del("Cookie")
			if !op.WebSocket {
				// ReverseProxy re-adds these for any upgrade; only a declared
				// WebSocket operation may switch protocols.
				pr.Out.Header.Del("Upgrade")
				pr.Out.Header.Del("Connection")
			}
			if cred != "" {
				inject.apply(pr.Out, cred)
			}
			if runner {
				pr.Out.Header.Set(gatewayContextHeader, gatewayContext(rec))
			}
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				jsonError(w, fmt.Sprintf("request body exceeds %d bytes", tooBig.Limit), http.StatusRequestEntityTooLarge)
				return
			}
			// Never echo err: with query injection it contains the URL + secret.
			log.Printf("lane: api=%s op=%s: upstream request failed", api.ID, op.ID)
			jsonError(w, "upstream request failed", http.StatusBadGateway)
		},
	}
}

func (l *Lane) credentialFor(ctx context.Context, st *GatewayStore, id string) (string, Injection, error) {
	if id == "" {
		return "", Injection{}, nil
	}
	c, err := st.GetCredential(id)
	if err != nil {
		return "", Injection{}, errors.Join(errCredential, err)
	}
	v, err := l.creds.Resolve(ctx, st, id)
	return v, c.Inject, err
}

func (l *Lane) record(st *GatewayStore, rec UsageRecord, status int, start time.Time, bytes int64) {
	rec.Time = start.UTC()
	rec.Status = status
	rec.LatencyMs = l.now().Sub(start).Milliseconds()
	rec.BytesOut = bytes
	if err := st.AppendUsage(rec); err != nil {
		log.Printf("lane: usage append failed: %v", err)
	}
}

// matchOperation finds the declared operation for method + path. Template
// segments in braces match any single non-empty segment.
func matchOperation(ops []Operation, method, path string) *Operation {
	for i := range ops {
		if strings.EqualFold(ops[i].Method, method) {
			if _, ok := matchPath(ops[i].Path, path); ok {
				return &ops[i]
			}
		}
	}
	return nil
}

func matchPath(tmpl, path string) (map[string]string, bool) {
	ts := splitPath(tmpl)
	ps := splitPath(path)
	if len(ts) != len(ps) {
		return nil, false
	}
	params := map[string]string{}
	for i := range ts {
		if strings.HasPrefix(ts[i], "{") && strings.HasSuffix(ts[i], "}") {
			if ps[i] == "" {
				return nil, false
			}
			params[ts[i][1:len(ts[i])-1]] = ps[i]
			continue
		}
		if ts[i] != ps[i] {
			return nil, false
		}
	}
	return params, true
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func joinURLPath(base, rest string) string {
	if base == "" || base == "/" {
		return rest
	}
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(rest, "/")
}

// statusWriter records status + bytes and keeps streaming working: Flush and
// Unwrap let ReverseProxy's ResponseController reach the real writer.
type statusWriter struct {
	http.ResponseWriter
	status      int
	bytes       int64
	wroteHeader bool
}

func (s *statusWriter) WriteHeader(code int) {
	if !s.wroteHeader {
		s.status = code
		s.wroteHeader = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	s.wroteHeader = true
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
