package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Subscription-lane model (PRD 0001, ADR 0003). Every object is one JSON file:
//
//	{dataDir}/owners/{owner}/gateway/apis/{id}.json
//	{dataDir}/owners/{owner}/gateway/mcp/{id}.json
//	{dataDir}/owners/{owner}/gateway/bundles/{id}.json
//	{dataDir}/owners/{owner}/gateway/principals/{id}.json
//	{dataDir}/owners/{owner}/gateway/subscriptions/{id}.json
//	{dataDir}/owners/{owner}/gateway/credentials/{id}.json
//	{dataDir}/owners/{owner}/gateway/usage/{YYYY-MM-DD}.jsonl
//
// The whole set is small (tens to hundreds of objects), so reads are served
// from disk on demand; writes go through a temp file + rename.

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

func validID(id string) bool { return idPattern.MatchString(id) }

type ApiKind string

const (
	ApiKindHTTP ApiKind = "http"
	ApiKindLLM  ApiKind = "llm"
)

// Api is an upstream published through the gateway at /apis/{id}/….
type Api struct {
	ID          string      `json:"id"`
	Name        string      `json:"name,omitempty"`
	Kind        ApiKind     `json:"kind"`
	Upstream    string      `json:"upstream"`
	Credential  string      `json:"credential,omitempty"` // Credential id; empty = none
	Operations  []Operation `json:"operations,omitempty"`
	CreatedAt   time.Time   `json:"createdAt"`
	Description string      `json:"description,omitempty"`
}

// Operation is one callable route of an Api — the unit an MCP tool maps to.
// Path is a template relative to the upstream, e.g. "/speech/{deployment}/transcribe".
type Operation struct {
	ID          string          `json:"id"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Summary     string          `json:"summary,omitempty"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"` // JSON Schema of the tool arguments
	// BodyContentType of the upstream request body when called as a tool
	// (default application/json).
	BodyContentType string `json:"bodyContentType,omitempty"`
}

type McpSource string

const (
	McpSourceAPI    McpSource = "api"
	McpSourceRemote McpSource = "remote"
)

// McpServer is served at /mcp/{id}.
type McpServer struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	Source      McpSource `json:"source"`
	// source=api
	Api   string    `json:"api,omitempty"`
	Tools []McpTool `json:"tools,omitempty"`
	// source=remote
	RemoteURL  string    `json:"remoteUrl,omitempty"`
	Credential string    `json:"credential,omitempty"`
	AllowTools []string  `json:"allowTools,omitempty"` // empty = all
	CreatedAt  time.Time `json:"createdAt"`
}

// McpTool maps a tool name onto an Api operation.
type McpTool struct {
	Name        string `json:"name"`
	Operation   string `json:"operation"`
	Description string `json:"description,omitempty"` // overrides the operation's
}

type Bundle struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	Description string    `json:"description,omitempty"`
	Hidden      bool      `json:"hidden,omitempty"`
	Apis        []string  `json:"apis"`
	McpServers  []string  `json:"mcpServers"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (b *Bundle) hasApi(id string) bool       { return contains(b.Apis, id) }
func (b *Bundle) hasMcpServer(id string) bool { return contains(b.McpServers, id) }

type PrincipalKind string

const (
	PrincipalHuman   PrincipalKind = "human"
	PrincipalService PrincipalKind = "service"
)

type Principal struct {
	ID        string        `json:"id"`
	Kind      PrincipalKind `json:"kind"`
	Sub       string        `json:"sub,omitempty"` // OIDC subject (humans; optional for services)
	Email     string        `json:"email,omitempty"`
	Name      string        `json:"name,omitempty"`
	Disabled  bool          `json:"disabled,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
}

type SubState string

const (
	SubActive    SubState = "active"
	SubSuspended SubState = "suspended"
	SubCancelled SubState = "cancelled"
	SubExpired   SubState = "expired" // derived from ExpiresAt, never stored
)

type Subscription struct {
	ID            string     `json:"id"`
	Bundle        string     `json:"bundle"`
	Principal     string     `json:"principal"`
	State         SubState   `json:"state"`
	PrimaryHash   string     `json:"primaryHash"`
	SecondaryHash string     `json:"secondaryHash"`
	ExpiresAt     *time.Time `json:"expiresAt,omitempty"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
}

// EffectiveState folds expiry into the stored state.
func (s *Subscription) EffectiveState(now time.Time) SubState {
	if s.State == SubActive && s.ExpiresAt != nil && !now.Before(*s.ExpiresAt) {
		return SubExpired
	}
	return s.State
}

// UsageRecord is one data-plane call, attributed by the authenticated subscription.
type UsageRecord struct {
	Time         time.Time `json:"time"`
	Subscription string    `json:"subscription"`
	Principal    string    `json:"principal"`
	Bundle       string    `json:"bundle"`
	Api          string    `json:"api,omitempty"`
	McpServer    string    `json:"mcpServer,omitempty"`
	Tool         string    `json:"tool,omitempty"`
	Operation    string    `json:"operation,omitempty"`
	Method       string    `json:"method,omitempty"`
	Status       int       `json:"status"`
	LatencyMs    int64     `json:"latencyMs"`
	BytesOut     int64     `json:"bytesOut"`
}

var errNotFound = errors.New("not found")

// GatewayStore persists the subscription-lane model.
// One per owner; obtain it through GatewayRoot.Owner, which validates the name.
type GatewayStore struct {
	owner string
	base  string
	mu    sync.Mutex // serialises writes + usage appends
}

func (g *GatewayStore) path(kind, id string) string {
	return filepath.Join(g.base, kind, id+".json")
}

func (g *GatewayStore) get(kind, id string, out any) error {
	if !validID(id) {
		return errNotFound
	}
	data, err := os.ReadFile(g.path(kind, id))
	if os.IsNotExist(err) {
		return errNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func (g *GatewayStore) put(kind, id string, v any) error {
	if !validID(id) {
		return fmt.Errorf("invalid id %q (lowercase letters, digits, '-'; max 63)", id)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	dir := filepath.Join(g.base, kind)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.path(kind, id) + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, g.path(kind, id))
}

func (g *GatewayStore) exists(kind, id string) bool {
	if !validID(id) {
		return false
	}
	_, err := os.Stat(g.path(kind, id))
	return err == nil
}

func (g *GatewayStore) remove(kind, id string) error {
	if !validID(id) {
		return errNotFound
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	err := os.Remove(g.path(kind, id))
	if os.IsNotExist(err) {
		return errNotFound
	}
	return err
}

func listKind[T any](g *GatewayStore, kind string) ([]T, error) {
	entries, err := os.ReadDir(filepath.Join(g.base, kind))
	if os.IsNotExist(err) {
		return []T{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		n := e.Name()
		if !e.IsDir() && filepath.Ext(n) == ".json" {
			names = append(names, n[:len(n)-5])
		}
	}
	sort.Strings(names)
	out := make([]T, 0, len(names))
	for _, id := range names {
		var v T
		if err := g.get(kind, id, &v); err == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

const (
	kindApi          = "apis"
	kindMcp          = "mcp"
	kindBundle       = "bundles"
	kindPrincipal    = "principals"
	kindSubscription = "subscriptions"
	kindCredential   = "credentials"
	kindConnector    = "connectors"
)

func (g *GatewayStore) GetApi(id string) (*Api, error) {
	var v Api
	return &v, g.get(kindApi, id, &v)
}
func (g *GatewayStore) GetMcpServer(id string) (*McpServer, error) {
	var v McpServer
	return &v, g.get(kindMcp, id, &v)
}
func (g *GatewayStore) GetBundle(id string) (*Bundle, error) {
	var v Bundle
	return &v, g.get(kindBundle, id, &v)
}
func (g *GatewayStore) GetPrincipal(id string) (*Principal, error) {
	var v Principal
	return &v, g.get(kindPrincipal, id, &v)
}
func (g *GatewayStore) GetSubscription(id string) (*Subscription, error) {
	var v Subscription
	return &v, g.get(kindSubscription, id, &v)
}
func (g *GatewayStore) GetCredential(id string) (*Credential, error) {
	var v Credential
	return &v, g.get(kindCredential, id, &v)
}

// PrincipalBySub finds the principal bound to an OIDC subject.
func (g *GatewayStore) PrincipalBySub(sub string) (*Principal, error) {
	if sub == "" {
		return nil, errNotFound
	}
	ps, err := listKind[Principal](g, kindPrincipal)
	if err != nil {
		return nil, err
	}
	for i := range ps {
		if ps[i].Sub == sub {
			return &ps[i], nil
		}
	}
	return nil, errNotFound
}

// AppendUsage appends one record to usage/{today}.jsonl.
func (g *GatewayStore) AppendUsage(rec UsageRecord) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	dir := filepath.Join(g.base, "usage")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, rec.Time.UTC().Format("2006-01-02")+".jsonl"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = f.Write(append(line, '\n'))
	return err
}

// ReadUsage returns records in [from, to] (dates YYYY-MM-DD, inclusive).
func (g *GatewayStore) ReadUsage(from, to string) ([]UsageRecord, error) {
	dir := filepath.Join(g.base, "usage")
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []UsageRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []UsageRecord{}
	for _, e := range entries {
		n := e.Name()
		if len(n) != len("2006-01-02.jsonl") {
			continue
		}
		d := n[:10]
		if (from != "" && d < from) || (to != "" && d > to) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return nil, err
		}
		for _, line := range splitLines(data) {
			var r UsageRecord
			if len(line) > 0 && json.Unmarshal(line, &r) == nil {
				out = append(out, r)
			}
		}
	}
	return out, nil
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func without(xs []string, x string) []string {
	out := make([]string, 0, len(xs))
	for _, v := range xs {
		if v != x {
			out = append(out, v)
		}
	}
	return out
}
