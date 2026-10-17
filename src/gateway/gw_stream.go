package main

import (
	"bufio"
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// WebSocket operations on the subscription lane (ADR 0007). The session is
// the stdlib ReverseProxy's own upgrade path: the upgrade request is
// rewritten exactly like any other operation (credential injected, caller
// credentials stripped, query kept), the upstream's 101 is relayed, and the
// two connections are spliced byte for byte. Frames are never parsed or
// logged. Admission happens before a token is minted or the upstream dialed;
// the session is bounded by cancelling the request context, which makes
// ReverseProxy close the upstream connection and then the caller's.

func isWebSocketUpgrade(r *http.Request) bool {
	return r.Method == http.MethodGet &&
		strings.EqualFold(r.Header.Get("Upgrade"), "websocket") &&
		headerHasToken(r.Header, "Connection", "upgrade")
}

func headerHasToken(h http.Header, name, token string) bool {
	for _, v := range h.Values(name) {
		for _, t := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(t), token) {
				return true
			}
		}
	}
	return false
}

func (l *Lane) serveStream(w http.ResponseWriter, r *http.Request, st *GatewayStore, g *grant, api *Api, op *Operation, rest string, rec UsageRecord, start time.Time) {
	if !isWebSocketUpgrade(r) {
		w.Header().Set("Upgrade", "websocket")
		w.Header().Set("Connection", "Upgrade")
		jsonError(w, "websocket operation: send a WebSocket upgrade request", http.StatusUpgradeRequired)
		l.record(st, rec, http.StatusUpgradeRequired, start, 0)
		return
	}
	metered := op.Meter == meterAudio
	s, d, retryAfter := l.limits.admitStream(st, g.principal, l.now(), metered)
	if d != nil {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
		jsonError(w, d.msg, d.status)
		l.record(st, rec, d.status, start, 0)
		return
	}
	charged := int64(0)
	defer func() { s.release(charged) }()

	maxSession := op.MaxSessionSeconds
	if maxSession <= 0 {
		maxSession = defaultMaxSessionSeconds
	}
	if s.audioLeft > 0 && s.audioLeft < maxSession {
		maxSession = s.audioLeft // a session cannot outrun today's quota
	}

	cred, inject, err := l.credentialFor(r.Context(), st, api.Credential)
	if err != nil {
		log.Printf("lane: owner=%s api=%s op=%s: %v", st.owner, api.ID, op.ID, err)
		jsonError(w, "upstream credential unavailable", http.StatusBadGateway)
		l.record(st, rec, http.StatusBadGateway, start, 0)
		return
	}
	target, rt, err := l.upstreamTarget(st, api, r.RemoteAddr)
	if errors.Is(err, errConnectorOffline) {
		jsonError(w, "connector offline", http.StatusServiceUnavailable)
		l.record(st, rec, http.StatusServiceUnavailable, start, 0)
		return
	}
	if err != nil {
		jsonError(w, "api upstream misconfigured", http.StatusBadGateway)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), time.Duration(maxSession)*time.Second)
	defer cancel()
	hw := &hijackCounter{statusWriter: &statusWriter{ResponseWriter: w, status: http.StatusOK}, now: l.now}
	l.proxy(api, op, rest, r.URL.RawQuery, target, rt, cred, inject, rec).ServeHTTP(hw, r.WithContext(ctx))

	var bytesOut int64
	if hw.conn != nil {
		dur := l.now().Sub(hw.opened)
		rec.DurationMs = dur.Milliseconds()
		rec.BytesIn, bytesOut = hw.conn.in.Load(), hw.conn.out.Load()
		if metered {
			charged = int64((dur + time.Second - 1) / time.Second) // a started second is a second
			if s.audioLeft > 0 && charged > s.audioLeft {
				charged = s.audioLeft // cut at the quota: the deadline's overshoot is not audio
			}
			rec.AudioSeconds = charged
		}
	} else {
		bytesOut = hw.bytes
	}
	l.record(st, rec, hw.status, start, bytesOut)
}

// hijackCounter lets ReverseProxy hijack the caller connection for a 101 and
// counts the bytes that then flow over it.
type hijackCounter struct {
	*statusWriter
	conn   *countingConn
	opened time.Time
	now    func() time.Time
}

func (h *hijackCounter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	c, brw, err := http.NewResponseController(h.statusWriter.ResponseWriter).Hijack()
	if err != nil {
		return nil, nil, err
	}
	h.status = http.StatusSwitchingProtocols
	h.opened = h.now()
	h.conn = &countingConn{Conn: c}
	return h.conn, brw, nil
}
