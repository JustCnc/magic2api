package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// stateStore persists plugin-owned JSON next to the host's data dir.
type stateStore struct {
	dir string
	mu  sync.Mutex
}

func newStateStore(dir string) (*stateStore, error) {
	if strings.TrimSpace(dir) == "" {
		dir = defaultDataDir
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %s: %w", dir, err)
	}
	return &stateStore{dir: dir}, nil
}

func (s *stateStore) path(name string) string {
	return filepath.Join(s.dir, name)
}

// readJSON decodes a file into v; a missing file is not an error.
func (s *stateStore) readJSON(name string, v any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path(name))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if len(raw) == 0 {
		return false, nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return false, err
	}
	return true, nil
}

// writeJSON replaces a file atomically so a crash cannot truncate it.
func (s *stateStore) writeJSON(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path(name) + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path(name))
}

func (s *stateStore) remove(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.path(name))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *stateStore) dirPath() string { return s.dir }
