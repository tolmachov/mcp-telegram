//go:build !darwin

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

type fileStore struct {
	path string
}

// NewStore creates a new config store backed by a JSON file.
func NewStore() (Store, error) {
	path, err := configPath()
	if err != nil {
		return nil, err
	}
	return &fileStore{path: path}, nil
}

func configPath() (string, error) {
	path, err := resolveConfigPath()
	if err != nil {
		return "", fmt.Errorf("cannot determine config storage location: %w; set XDG_STATE_HOME or ensure HOME is set", err)
	}
	return path, nil
}

func resolveConfigPath() (string, error) {
	stateDir, err := xdg.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "config.json"), nil
}

func (s *fileStore) load() (map[string]string, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return make(map[string]string), nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parsing config file: %w", err)
	}

	return m, nil
}

func (s *fileStore) save(m map[string]string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding config: %w", err)
	}
	if err := xdg.WriteFileAtomic(s.path, data, 0o600, ".config.*.json.tmp"); err != nil {
		return fmt.Errorf("saving config: %w", err)
	}
	return nil
}

func (s *fileStore) Get(key string) (string, error) {
	m, err := s.load()
	if err != nil {
		return "", err
	}

	val, ok := m[key]
	if !ok {
		return "", ErrNotFound
	}

	return val, nil
}

func (s *fileStore) Set(key, value string) error {
	m, err := s.load()
	if err != nil {
		return err
	}

	m[key] = value

	return s.save(m)
}

func (s *fileStore) Delete(key string) error {
	m, err := s.load()
	if err != nil {
		return err
	}

	delete(m, key)

	return s.save(m)
}

func (s *fileStore) List() ([]string, error) {
	m, err := s.load()
	if err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	// Sort for stable output, matching the darwin vault's ConfigList so the
	// `config list` command behaves identically across platforms.
	sort.Strings(keys)

	return keys, nil
}

func (s *fileStore) LoadAll() (map[string]string, error) {
	return s.load()
}
