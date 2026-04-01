//go:build !darwin

package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type fileStore struct {
	path string
}

// NewStore creates a new config store backed by a JSON file.
func NewStore() Store {
	return &fileStore{
		path: configPath(),
	}
}

func configPath() string {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		homeDir, _ := os.UserHomeDir()
		stateHome = filepath.Join(homeDir, ".local", "state")
	}

	configDir := filepath.Join(stateHome, "mcp-telegram")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "warning: failed to create config directory %s: %v\n", configDir, err)
	}

	return filepath.Join(configDir, "config.json")
}

func (s *fileStore) load() (map[string]string, error) {
	data, err := os.ReadFile(s.path)
	if os.IsNotExist(err) {
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

	if err := os.WriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("writing config file: %w", err)
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

	var keys []string
	for key := range m {
		keys = append(keys, key)
	}

	return keys, nil
}

func (s *fileStore) LoadAll() (map[string]string, error) {
	return s.load()
}
