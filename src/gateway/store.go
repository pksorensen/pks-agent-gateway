package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Project holds project metadata persisted to project.json.
type Project struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
}

// Member holds membership info persisted to members.json.
type Member struct {
	Sub   string `json:"sub"`
	Email string `json:"email"`
	Role  string `json:"role"` // "ProjectAdmin" or "ProjectUser"
}

// Store is a file-based storage layer rooted at dataDir/owners/owner/.
//
// Layout:
//
//	{dataDir}/owners/{owner}/projects/{projectId}/project.json
//	{dataDir}/owners/{owner}/projects/{projectId}/members.json
//	{dataDir}/owners/{owner}/projects/{projectId}/otel/{YYYY-MM-DD}.jsonl
type Store struct {
	base string // dataDir/owners/owner/projects

	mu    sync.Mutex
	locks map[string]*sync.Mutex // per-file append mutex
}

// NewStore creates a Store. dataDir defaults to "./data" if empty.
func NewStore(dataDir, owner string) *Store {
	if dataDir == "" {
		dataDir = "./data"
	}
	return &Store{
		base:  filepath.Join(dataDir, "owners", owner, "projects"),
		locks: make(map[string]*sync.Mutex),
	}
}

func (s *Store) projectDir(id string) string {
	return filepath.Join(s.base, id)
}

// ListProjects returns all projects found under the owner's projects directory.
func (s *Store) ListProjects() ([]Project, error) {
	entries, err := os.ReadDir(s.base)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}

	var projects []Project
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p, err := s.GetProject(e.Name())
		if err != nil {
			continue // skip corrupt entries
		}
		projects = append(projects, *p)
	}
	return projects, nil
}

// GetProject reads project.json for the given project ID.
func (s *Store) GetProject(id string) (*Project, error) {
	path := filepath.Join(s.projectDir(id), "project.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, fmt.Errorf("project %q not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("read project %q: %w", id, err)
	}
	var p Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse project %q: %w", id, err)
	}
	return &p, nil
}

// CreateProject writes project.json and initialises an empty members.json.
func (s *Store) CreateProject(p Project) error {
	dir := s.projectDir(p.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create project dir: %w", err)
	}

	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), data, 0o644); err != nil {
		return fmt.Errorf("write project.json: %w", err)
	}

	// Initialise empty members list.
	emptyMembers, _ := json.Marshal([]Member{})
	if err := os.WriteFile(filepath.Join(dir, "members.json"), emptyMembers, 0o644); err != nil {
		return fmt.Errorf("write members.json: %w", err)
	}
	return nil
}

// ListMembers reads members.json for the given project.
func (s *Store) ListMembers(projectID string) ([]Member, error) {
	path := filepath.Join(s.projectDir(projectID), "members.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read members: %w", err)
	}
	var members []Member
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, fmt.Errorf("parse members: %w", err)
	}
	return members, nil
}

// fileMu returns (and caches) a per-file mutex for safe concurrent appends.
func (s *Store) fileMu(path string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, ok := s.locks[path]; ok {
		return m
	}
	m := &sync.Mutex{}
	s.locks[path] = m
	return m
}

// AppendOtelEvent appends a wrapped JSONL record to otel/{today}.jsonl.
func (s *Store) AppendOtelEvent(projectID string, event json.RawMessage) error {
	otelDir := filepath.Join(s.projectDir(projectID), "otel")
	if err := os.MkdirAll(otelDir, 0o755); err != nil {
		return fmt.Errorf("create otel dir: %w", err)
	}

	date := time.Now().UTC().Format("2006-01-02")
	path := filepath.Join(otelDir, date+".jsonl")

	mu := s.fileMu(path)
	mu.Lock()
	defer mu.Unlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open otel log: %w", err)
	}
	defer f.Close()

	line, err := json.Marshal(event)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	_, err = f.Write(line)
	return err
}

// ReadOtelEvents reads and parses every line of the JSONL file for the given date.
// date must be in YYYY-MM-DD format.
func (s *Store) ReadOtelEvents(projectID string, date string) ([]json.RawMessage, error) {
	path := filepath.Join(s.projectDir(projectID), "otel", date+".jsonl")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read otel events: %w", err)
	}

	var events []json.RawMessage
	for _, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		// Each line is a JSON-encoded json.RawMessage (i.e. a quoted/escaped JSON
		// string). Unmarshal it back to the original raw bytes.
		var raw json.RawMessage
		if err := json.Unmarshal(line, &raw); err != nil {
			// If the line itself is already valid JSON (not double-encoded), use it
			// directly.
			raw = json.RawMessage(line)
		}
		events = append(events, raw)
	}
	return events, nil
}

// splitLines splits bytes on newlines without allocating a string copy.
func splitLines(data []byte) [][]byte {
	var lines [][]byte
	start := 0
	for i, b := range data {
		if b == '\n' {
			lines = append(lines, data[start:i])
			start = i + 1
		}
	}
	if start < len(data) {
		lines = append(lines, data[start:])
	}
	return lines
}

// OtelFileDates returns sorted list of date strings for which JSONL files exist.
func (s *Store) OtelFileDates(projectID string) ([]string, error) {
	otelDir := filepath.Join(s.projectDir(projectID), "otel")
	entries, err := os.ReadDir(otelDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list otel dir: %w", err)
	}
	var dates []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if len(name) == len("2006-01-02.jsonl") && name[len(name)-6:] == ".jsonl" {
			dates = append(dates, name[:len(name)-6])
		}
	}
	return dates, nil
}
