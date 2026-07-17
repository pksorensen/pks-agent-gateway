package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Test-bench storage: sim scenarios and record/replay cassettes.
//
// Layout (sibling of the projects/ tree, same owner root):
//
//	{dataDir}/owners/{owner}/testbench/scenarios/{name}.json
//	{dataDir}/owners/{owner}/testbench/cassettes/{name}.jsonl

func (s *Store) scenariosDir() string {
	return filepath.Join(s.testbenchBase, "scenarios")
}

func (s *Store) cassettesDir() string {
	return filepath.Join(s.testbenchBase, "cassettes")
}

func (s *Store) scenarioPath(name string) string {
	return filepath.Join(s.scenariosDir(), slugify(name)+".json")
}

func (s *Store) cassettePath(name string) string {
	return filepath.Join(s.cassettesDir(), slugify(name)+".jsonl")
}

// ListScenarioFiles returns the names (without extension) of all stored
// scenario files, sorted by directory order.
func (s *Store) ListScenarioFiles() ([]string, error) {
	entries, err := os.ReadDir(s.scenariosDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list scenarios: %w", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		names = append(names, strings.TrimSuffix(e.Name(), ".json"))
	}
	return names, nil
}

// ReadScenarioFile returns the raw bytes of a stored scenario, or
// os.ErrNotExist if no file exists for the name.
func (s *Store) ReadScenarioFile(name string) ([]byte, error) {
	return os.ReadFile(s.scenarioPath(name))
}

// WriteScenarioFile persists a scenario document. Returns true if the file
// already existed (i.e. this was an update).
func (s *Store) WriteScenarioFile(name string, data []byte) (existed bool, err error) {
	path := s.scenarioPath(name)
	if _, statErr := os.Stat(path); statErr == nil {
		existed = true
	}
	if err := os.MkdirAll(s.scenariosDir(), 0o755); err != nil {
		return existed, fmt.Errorf("create scenarios dir: %w", err)
	}
	return existed, os.WriteFile(path, data, 0o644)
}

// DeleteScenarioFile removes a stored scenario. Returns os.ErrNotExist if the
// file does not exist.
func (s *Store) DeleteScenarioFile(name string) error {
	return os.Remove(s.scenarioPath(name))
}

// CassetteInfo summarises a stored cassette for listings.
type CassetteInfo struct {
	Name       string    `json:"name"`
	Entries    int       `json:"entries"`
	Bytes      int64     `json:"bytes"`
	ModifiedAt time.Time `json:"modifiedAt"`
}

// ListCassettes returns summary info for all stored cassettes.
func (s *Store) ListCassettes() ([]CassetteInfo, error) {
	entries, err := os.ReadDir(s.cassettesDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list cassettes: %w", err)
	}
	var infos []CassetteInfo
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".jsonl")
		fi, err := e.Info()
		if err != nil {
			continue
		}
		count, _ := s.countCassetteEntries(name)
		infos = append(infos, CassetteInfo{
			Name:       name,
			Entries:    count,
			Bytes:      fi.Size(),
			ModifiedAt: fi.ModTime().UTC(),
		})
	}
	return infos, nil
}

func (s *Store) countCassetteEntries(name string) (int, error) {
	f, err := os.Open(s.cassettePath(name))
	if err != nil {
		return 0, err
	}
	defer f.Close()
	count := 0
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for scanner.Scan() {
		if len(strings.TrimSpace(scanner.Text())) > 0 {
			count++
		}
	}
	return count, scanner.Err()
}

// AppendCassetteEntry appends one exchange to a cassette JSONL file, assigning
// the next sequence number. Concurrent appends are serialized per file.
func (s *Store) AppendCassetteEntry(name string, entry *cassetteEntry) error {
	if err := os.MkdirAll(s.cassettesDir(), 0o755); err != nil {
		return fmt.Errorf("create cassettes dir: %w", err)
	}
	path := s.cassettePath(name)

	mu := s.fileMu(path)
	mu.Lock()
	defer mu.Unlock()

	count, err := s.countCassetteEntries(name)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("count cassette entries: %w", err)
	}
	entry.Seq = count

	line, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	line = append(line, '\n')

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open cassette: %w", err)
	}
	defer f.Close()
	_, err = f.Write(line)
	return err
}

// ReadCassette parses every entry of a cassette. Returns os.ErrNotExist if no
// cassette file exists for the name.
func (s *Store) ReadCassette(name string) ([]cassetteEntry, error) {
	data, err := os.ReadFile(s.cassettePath(name))
	if err != nil {
		return nil, err
	}
	var entries []cassetteEntry
	for i, line := range splitLines(data) {
		if len(line) == 0 {
			continue
		}
		var e cassetteEntry
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, fmt.Errorf("cassette %q line %d: %w", name, i+1, err)
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// DeleteCassette removes a stored cassette. Returns os.ErrNotExist if the file
// does not exist.
func (s *Store) DeleteCassette(name string) error {
	return os.Remove(s.cassettePath(name))
}

// CassetteModTime returns the modification time of a cassette file, used to
// invalidate the simulator's parsed-cassette cache.
func (s *Store) CassetteModTime(name string) (time.Time, error) {
	fi, err := os.Stat(s.cassettePath(name))
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}
