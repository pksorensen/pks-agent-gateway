package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/pksorensen/pks-agent-tunnel/src/protocol"
)

// Runner-hosted upstreams (ADR 0004). A Connector is an owner-scoped entity
// whose token lets an unmodified `agent-tunnel host` register at
// /connect/v1/control. An Api with upstream runner://<connector>/<slot>[/base]
// is then served over a yamux stream on that session instead of a TCP dial.

const (
	gatewayContextHeader = "X-Gateway-Context"
	runnerScheme         = "runner"
)

type Connector struct {
	ID            string    `json:"id"`
	Name          string    `json:"name,omitempty"`
	Disabled      bool      `json:"disabled,omitempty"`
	PrimaryHash   string    `json:"primaryHash"`
	SecondaryHash string    `json:"secondaryHash"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

func (g *GatewayStore) GetConnector(id string) (*Connector, error) {
	var v Connector
	return &v, g.get(kindConnector, id, &v)
}

// runnerTarget is a parsed runner://<connector>/<slot>[/base/path] upstream.
type runnerTarget struct {
	connector, slot, basePath string
}

func parseRunnerUpstream(s string) (runnerTarget, bool) {
	u, err := url.Parse(s)
	if err != nil || u.Scheme != runnerScheme || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return runnerTarget{}, false
	}
	slot, base, _ := strings.Cut(strings.TrimPrefix(u.Path, "/"), "/")
	if !validID(u.Host) || protocol.ValidateName(slot) != nil {
		return runnerTarget{}, false
	}
	if base != "" {
		base = "/" + base
	}
	return runnerTarget{connector: u.Host, slot: slot, basePath: base}, true
}

// --- Session registry ---

type connSession struct {
	sess   *yamux.Session
	token  string // "primary" | "secondary": which token slot authenticated it
	slots  map[string]bool
	remote string
	since  time.Time
}

type connectorPresence struct {
	Connected bool       `json:"connected"`
	Sessions  int        `json:"sessions"`
	Slots     []string   `json:"slots,omitempty"`
	LastSeen  *time.Time `json:"lastSeen,omitempty"`
}

type connectorHub struct {
	mu       sync.Mutex
	sessions map[string][]*connSession // "owner/connector"
	next     map[string]int
	lastSeen map[string]time.Time
	now      func() time.Time
}

func newConnectorHub() *connectorHub {
	return &connectorHub{sessions: map[string][]*connSession{}, next: map[string]int{},
		lastSeen: map[string]time.Time{}, now: time.Now}
}

func hubKey(owner, id string) string { return owner + "/" + id }

func (h *connectorHub) add(key string, s *connSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sessions[key] = append(h.sessions[key], s)
	h.lastSeen[key] = h.now().UTC()
}

func (h *connectorHub) remove(key string, s *connSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	xs := h.sessions[key]
	for i := range xs {
		if xs[i] == s {
			xs = append(xs[:i], xs[i+1:]...)
			break
		}
	}
	if len(xs) == 0 {
		delete(h.sessions, key)
	} else {
		h.sessions[key] = xs
	}
	h.lastSeen[key] = h.now().UTC()
}

func (h *connectorHub) touch(key string) {
	h.mu.Lock()
	h.lastSeen[key] = h.now().UTC()
	h.mu.Unlock()
}

// pick returns the next live session serving slot, round-robin.
func (h *connectorHub) pick(key, slot string) *connSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	xs := h.sessions[key]
	for i := 0; i < len(xs); i++ {
		n := h.next[key] % len(xs)
		h.next[key] = n + 1
		if s := xs[n]; s.slots[slot] && !s.sess.IsClosed() {
			return s
		}
	}
	return nil
}

// closeAll drops the live sessions of a connector: all of them (token "",
// on disable/delete) or only those authenticated by one token slot
// (regenerate), so the other slot keeps serving during a rotation.
func (h *connectorHub) closeAll(key, token string) {
	h.mu.Lock()
	xs := append([]*connSession(nil), h.sessions[key]...)
	h.mu.Unlock()
	for _, s := range xs {
		if token == "" || s.token == token {
			_ = s.sess.Close()
		}
	}
}

func (h *connectorHub) presence(key string) connectorPresence {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := connectorPresence{Sessions: len(h.sessions[key])}
	p.Connected = p.Sessions > 0
	seen := map[string]bool{}
	for _, s := range h.sessions[key] {
		for slot := range s.slots {
			if !seen[slot] {
				seen[slot] = true
				p.Slots = append(p.Slots, slot)
			}
		}
	}
	if t, ok := h.lastSeen[key]; ok {
		p.LastSeen = &t
	}
	return p
}

var errConnectorOffline = errors.New("connector offline")

// transport returns a RoundTripper that carries one request over a fresh
// yamux stream to the connector's slot (same shape as the tunnel server's
// router: StreamMeta line, then raw HTTP; no keep-alive).
func (h *connectorHub) transport(owner string, t runnerTarget, remote string) (http.RoundTripper, error) {
	s := h.pick(hubKey(owner, t.connector), t.slot)
	if s == nil {
		return nil, errConnectorOffline
	}
	return &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			stream, err := s.sess.OpenStream()
			if err != nil {
				return nil, err
			}
			meta, _ := json.Marshal(protocol.StreamMeta{Slot: t.slot, Remote: remote})
			if _, err := stream.Write(append(meta, '\n')); err != nil {
				stream.Close()
				return nil, err
			}
			return stream, nil
		},
		DisableKeepAlives:     true,
		MaxIdleConnsPerHost:   -1,
		ResponseHeaderTimeout: 5 * time.Minute,
	}, nil
}

// --- Control endpoint: GET /connect/v1/control ---

type connectorControl struct {
	root *GatewayRoot
	hub  *connectorHub
}

func (c *connectorControl) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer ws.Close(websocket.StatusNormalClosure, "")
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	sess, err := yamux.Server(websocket.NetConn(ctx, ws, websocket.MessageBinary), nil)
	if err != nil {
		return
	}
	defer sess.Close()
	ctrl, err := sess.AcceptStream()
	if err != nil {
		return
	}
	defer ctrl.Close()

	br := bufio.NewReader(ctrl)
	_ = ctrl.SetReadDeadline(time.Now().Add(15 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	_ = ctrl.SetReadDeadline(time.Time{})
	var reg protocol.RegisterFrame
	if err := json.Unmarshal(line, &reg); err != nil || reg.Type != protocol.FrameRegister {
		writeControlError(ctrl, "bad_frame", "first frame must be type=register", "")
		return
	}
	// The frame's owner only locates the store; the token decides. Its tunnel
	// name is ignored — the session binds to the connector the token names.
	st, conn, slot, ok := c.authenticate(reg.Owner, reg.Token)
	if !ok {
		writeControlError(ctrl, "unauthorized", "invalid connector token", "")
		return
	}
	s := &connSession{sess: sess, token: slot, slots: map[string]bool{}, remote: r.RemoteAddr, since: time.Now().UTC()}
	acks := make([]protocol.SlotAck, 0, len(reg.Slots))
	for _, sl := range reg.Slots {
		if err := protocol.ValidateName(sl.Name); err != nil {
			writeControlError(ctrl, "bad_name", err.Error(), sl.Name)
			return
		}
		if sl.Kind != protocol.SlotKindHTTP {
			writeControlError(ctrl, "unsupported", "only http slots are served by the gateway", sl.Name)
			return
		}
		s.slots[sl.Name] = true
		// No public URL: a slot is reachable only through an API in a Bundle.
		acks = append(acks, protocol.SlotAck{Name: sl.Name, Kind: sl.Kind})
	}
	if err := writeControlFrame(ctrl, protocol.RegisterAckFrame{Type: protocol.FrameRegisterAck, Tunnel: conn.ID, Slots: acks}); err != nil {
		return
	}
	key := hubKey(st.owner, conn.ID)
	c.hub.add(key, s)
	defer c.hub.remove(key, s)
	log.Printf("connector: owner=%s connector=%s registered %d slot(s) from %s", st.owner, conn.ID, len(acks), r.RemoteAddr)

	for {
		line, err := br.ReadBytes('\n')
		if err != nil {
			log.Printf("connector: owner=%s connector=%s disconnected", st.owner, conn.ID)
			return
		}
		if t, err := protocol.DecodeEnvelope(line); err == nil && t == protocol.FramePing {
			var p protocol.PingFrame
			_ = json.Unmarshal(line, &p)
			c.hub.touch(key)
			if err := writeControlFrame(ctrl, protocol.PongFrame{Type: protocol.FramePong, Nonce: p.Nonce}); err != nil {
				return
			}
		}
	}
}

// authenticate returns the connector the token names and which token slot matched.
func (c *connectorControl) authenticate(owner, token string) (*GatewayStore, *Connector, string, bool) {
	id, ok := parseKey(connectorKeyPrefix, token)
	if !ok {
		return nil, nil, "", false
	}
	st, err := c.root.Owner(owner)
	if err != nil {
		return nil, nil, "", false
	}
	conn, err := st.GetConnector(id)
	if err != nil || conn.Disabled {
		return nil, nil, "", false
	}
	switch {
	case hashMatches(token, conn.PrimaryHash):
		return st, conn, "primary", true
	case hashMatches(token, conn.SecondaryHash):
		return st, conn, "secondary", true
	}
	return nil, nil, "", false
}

func writeControlFrame(w net.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_ = w.SetWriteDeadline(time.Now().Add(10 * time.Second))
	defer w.SetWriteDeadline(time.Time{})
	_, err = w.Write(append(b, '\n'))
	return err
}

func writeControlError(w net.Conn, code, msg, slot string) {
	_ = writeControlFrame(w, protocol.ErrorFrame{Type: protocol.FrameError, Code: code, Message: msg, Slot: slot})
}

// gatewayContext is the X-Gateway-Context value sent to runner upstreams only.
func gatewayContext(rec UsageRecord) string {
	return fmt.Sprintf("sub=%s; principal=%s; bundle=%s; op=%s", rec.Subscription, rec.Principal, rec.Bundle, rec.Operation)
}

// upstreamTarget resolves an Api's upstream to the URL requests are built
// against. For runner:// it is a placeholder origin (the dial goes over the
// connector's yamux session) plus the base path, and rt carries the request.
func (l *Lane) upstreamTarget(st *GatewayStore, api *Api, remote string) (target *url.URL, rt http.RoundTripper, err error) {
	if t, ok := parseRunnerUpstream(api.Upstream); ok {
		if l.hub == nil {
			return nil, nil, errConnectorOffline
		}
		rt, err := l.hub.transport(st.owner, t, remote)
		if err != nil {
			return nil, nil, err
		}
		return &url.URL{Scheme: "http", Host: "localhost", Path: t.basePath}, rt, nil
	}
	target, err = url.Parse(api.Upstream)
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		return nil, nil, fmt.Errorf("api upstream misconfigured")
	}
	return target, nil, nil
}

// --- Management: /api/v1/owners/{owner}/connectors ---

type connectorView struct {
	ID        string            `json:"id"`
	Name      string            `json:"name,omitempty"`
	Disabled  bool              `json:"disabled"`
	Presence  connectorPresence `json:"presence"`
	CreatedAt time.Time         `json:"createdAt"`
	UpdatedAt time.Time         `json:"updatedAt"`
}

type issuedConnectorTokens struct {
	Connector      connectorView `json:"connector"`
	PrimaryToken   string        `json:"primaryToken,omitempty"`
	SecondaryToken string        `json:"secondaryToken,omitempty"`
	Note           string        `json:"note"`
}

func (a *gatewayAPI) connectorView(st *GatewayStore, c *Connector) connectorView {
	v := connectorView{ID: c.ID, Name: c.Name, Disabled: c.Disabled, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt}
	if a.hub != nil {
		v.Presence = a.hub.presence(hubKey(st.owner, c.ID))
	}
	return v
}

func (a *gatewayAPI) listConnectors(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	xs, err := listKind[Connector](st, kindConnector)
	if err != nil {
		storeErr(w, "connector", err)
		return
	}
	out := make([]connectorView, 0, len(xs))
	for i := range xs {
		out = append(out, a.connectorView(st, &xs[i]))
	}
	writeJSONStatus(w, http.StatusOK, out)
}

func (a *gatewayAPI) createConnector(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	var in struct {
		ID   string `json:"id"`
		Name string `json:"name,omitempty"`
	}
	if err := decodeBody(r, &in); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !validID(in.ID) {
		jsonError(w, "invalid id", http.StatusBadRequest)
		return
	}
	if st.exists(kindConnector, in.ID) {
		jsonError(w, "connector already exists", http.StatusConflict)
		return
	}
	now := a.now().UTC()
	pk, ph := newKey(connectorKeyPrefix, in.ID)
	sk, sh := newKey(connectorKeyPrefix, in.ID)
	c := &Connector{ID: in.ID, Name: in.Name, PrimaryHash: ph, SecondaryHash: sh, CreatedAt: now, UpdatedAt: now}
	if err := st.put(kindConnector, c.ID, c); err != nil {
		storeErr(w, "connector", err)
		return
	}
	writeJSONStatus(w, http.StatusCreated, issuedConnectorTokens{Connector: a.connectorView(st, c),
		PrimaryToken: pk, SecondaryToken: sk, Note: keysShownOnce})
}

func (a *gatewayAPI) getConnector(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	c, err := st.GetConnector(r.PathValue("id"))
	if err != nil {
		storeErr(w, "connector", err)
		return
	}
	writeJSONStatus(w, http.StatusOK, a.connectorView(st, c))
}

func (a *gatewayAPI) deleteConnector(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	id := r.PathValue("id")
	apis, _ := listKind[Api](st, kindApi)
	for _, api := range apis {
		if t, ok := parseRunnerUpstream(api.Upstream); ok && t.connector == id {
			jsonError(w, fmt.Sprintf("connector is used by api %q", api.ID), http.StatusConflict)
			return
		}
	}
	if err := st.remove(kindConnector, id); err != nil {
		storeErr(w, "connector", err)
		return
	}
	a.dropSessions(st, id, "")
	w.WriteHeader(http.StatusNoContent)
}

func (a *gatewayAPI) setConnectorDisabled(disabled bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		st := ownerStore(r)
		c, err := st.GetConnector(r.PathValue("id"))
		if err != nil {
			storeErr(w, "connector", err)
			return
		}
		c.Disabled, c.UpdatedAt = disabled, a.now().UTC()
		if err := st.put(kindConnector, c.ID, c); err != nil {
			storeErr(w, "connector", err)
			return
		}
		if disabled {
			a.dropSessions(st, c.ID, "")
		}
		writeJSONStatus(w, http.StatusOK, a.connectorView(st, c))
	}
}

func (a *gatewayAPI) regenerateConnector(w http.ResponseWriter, r *http.Request) {
	st := ownerStore(r)
	c, err := st.GetConnector(r.PathValue("id"))
	if err != nil {
		storeErr(w, "connector", err)
		return
	}
	var p struct {
		Slot string `json:"slot"`
	}
	if err := decodeBody(r, &p); err != nil {
		jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	tok, hash := newKey(connectorKeyPrefix, c.ID)
	out := issuedConnectorTokens{Note: keysShownOnce}
	switch p.Slot {
	case "primary":
		c.PrimaryHash, out.PrimaryToken = hash, tok
	case "secondary":
		c.SecondaryHash, out.SecondaryToken = hash, tok
	default:
		jsonError(w, `slot must be "primary" or "secondary"`, http.StatusBadRequest)
		return
	}
	c.UpdatedAt = a.now().UTC()
	if err := st.put(kindConnector, c.ID, c); err != nil {
		storeErr(w, "connector", err)
		return
	}
	// Only sessions authenticated with the replaced token must re-register.
	a.dropSessions(st, c.ID, p.Slot)
	out.Connector = a.connectorView(st, c)
	writeJSONStatus(w, http.StatusOK, out)
}

func (a *gatewayAPI) dropSessions(st *GatewayStore, id, token string) {
	if a.hub != nil {
		a.hub.closeAll(hubKey(st.owner, id), token)
	}
}
