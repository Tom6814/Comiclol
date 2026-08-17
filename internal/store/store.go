// Package store is a tiny file-backed JSON document store.
//
// Each collection is one JSON file written atomically (write-temp + rename).
// A process-wide mutex serialises writes per collection — plenty for a personal
// single-user app, zero external dependencies, and trivially inspectable.
package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

type Store struct {
	root string
	mu   sync.Mutex
}

func New(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) path(name string) string {
	return filepath.Join(s.root, name+".json")
}

// Read loads a collection into dst. Missing file is not an error (dst untouched).
func (s *Store) Read(name string, dst any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, dst)
}

// Write persists a collection atomically.
func (s *Store) Write(name string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	final := s.path(name)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

// Update loads (if present), lets fn mutate, and persists. fn receives the
// typed pointer; missing-file initial value is up to the caller via fn.
func (s *Store) Update(name string, makeZero func() any, fn func(v any) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := makeZero()
	if data, err := os.ReadFile(s.path(name)); err == nil && len(data) > 0 {
		if err := json.Unmarshal(data, v); err != nil {
			return err
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := fn(v); err != nil {
		return err
	}
	out, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	final := s.path(name)
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}
