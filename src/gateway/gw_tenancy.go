package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Tenancy (ADR 0005): every gateway entity lives under an owner, and an owner
// is claimed by the first caller, whose OIDC sub becomes its first admin.
// The owner name arrives in URLs, so it is untrusted: GatewayRoot.Owner is the
// only way to a GatewayStore and it validates the name before any path is
// built.

type GatewayRoot struct {
	dataDir string
	mu      sync.Mutex
	owners  map[string]*GatewayStore // shared per owner so its write mutex is too
}

func NewGatewayRoot(dataDir string) *GatewayRoot {
	if dataDir == "" {
		dataDir = "./data"
	}
	return &GatewayRoot{dataDir: dataDir, owners: map[string]*GatewayStore{}}
}

// Owner returns the store for owner o (whether or not it has been claimed).
// Only claimed owners are cached: the data plane calls this before any key
// check, so caching unclaimed ids would let anonymous traffic grow the map.
// An unclaimed store is never written to (every writer sits behind withOwner,
// which requires a record).
func (g *GatewayRoot) Owner(o string) (*GatewayStore, error) {
	if !validID(o) {
		return nil, errNotFound
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if st, ok := g.owners[o]; ok {
		return st, nil
	}
	st := &GatewayStore{owner: o, base: filepath.Join(g.dataDir, "owners", o, "gateway")}
	if _, err := os.Stat(st.ownerFile()); err == nil {
		g.owners[o] = st
	}
	return st, nil
}

// OwnerRecord is owners/{owner}/gateway/owner.json.
type OwnerRecord struct {
	ID        string    `json:"id"`
	Admins    []string  `json:"admins"` // OIDC subs
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

func (g *GatewayStore) ownerFile() string { return filepath.Join(g.base, "owner.json") }

// Record returns the owner record, or errNotFound when unclaimed.
func (g *GatewayStore) Record() (*OwnerRecord, error) {
	data, err := os.ReadFile(g.ownerFile())
	if os.IsNotExist(err) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	var rec OwnerRecord
	return &rec, json.Unmarshal(data, &rec)
}

func (g *GatewayStore) SaveRecord(rec *OwnerRecord) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := os.MkdirAll(g.base, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	tmp := g.ownerFile() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, g.ownerFile())
}

// Claim creates the owner with sub as its first admin; fails if it exists.
func (g *GatewayRoot) Claim(o, sub string, now time.Time) (*OwnerRecord, error) {
	st, err := g.Owner(o)
	if err != nil {
		return nil, fmt.Errorf("invalid owner id")
	}
	g.mu.Lock() // serialise claims across owners: first caller wins
	defer g.mu.Unlock()
	if _, err := os.Stat(st.ownerFile()); err == nil {
		return nil, errOwnerTaken
	}
	rec := &OwnerRecord{ID: o, Admins: []string{sub}, CreatedAt: now, UpdatedAt: now}
	if err := st.SaveRecord(rec); err != nil {
		return nil, err
	}
	g.owners[o] = st
	return rec, nil
}

var errOwnerTaken = fmt.Errorf("owner already claimed")

// Owners lists claimed owners.
func (g *GatewayRoot) Owners() []*GatewayStore {
	entries, _ := os.ReadDir(filepath.Join(g.dataDir, "owners"))
	var names []string
	for _, e := range entries {
		if e.IsDir() && validID(e.Name()) {
			if _, err := os.Stat(filepath.Join(g.dataDir, "owners", e.Name(), "gateway", "owner.json")); err == nil {
				names = append(names, e.Name())
			}
		}
	}
	sort.Strings(names)
	out := make([]*GatewayStore, 0, len(names))
	for _, n := range names {
		st, _ := g.Owner(n)
		out = append(out, st)
	}
	return out
}

// isOwnerAdmin: realm GatewayAdmin, or the caller's sub is listed.
func isOwnerAdmin(c *UserClaims, st *GatewayStore) bool {
	if c == nil {
		return false
	}
	if c.IsAdmin() {
		return true
	}
	rec, err := st.Record()
	return err == nil && c.Sub != "" && contains(rec.Admins, c.Sub)
}
