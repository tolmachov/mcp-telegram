//go:build !darwin

package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating home directory: %w", err)
		}
		if homeDir == "" {
			return "", fmt.Errorf("locating home directory: UserHomeDir returned empty path")
		}
		stateHome = filepath.Join(homeDir, ".local", "state")
	}
	if !filepath.IsAbs(stateHome) {
		return "", fmt.Errorf("state home %q is not absolute", stateHome)
	}

	configDir := filepath.Join(stateHome, "mcp-telegram")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		return "", fmt.Errorf("creating config directory %s: %w", configDir, err)
	}

	return filepath.Join(configDir, "config.json"), nil
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

	// Write-then-rename makes the update atomic: a crash (or SIGKILL) in
	// the middle of the write leaves either the old file fully intact or
	// the new one fully written — never a truncated mix. The temp file
	// lives in the same directory so os.Rename is a single inode op on
	// POSIX (no cross-device copy).
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".config.*.json.tmp")
	if err != nil {
		return fmt.Errorf("creating temp config file: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if any subsequent step fails — the successful
	// Rename path below re-names this file so the cleanup becomes a no-op.
	defer func() {
		// os.ErrNotExist is expected on the success path (rename consumed the file).
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Debug("cleanup of temp config file failed", "path", tmpPath, "err", err)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		if cerr := tmp.Close(); cerr != nil {
			slog.Debug("closing temp config file after chmod failure", "error", cerr)
		}
		return fmt.Errorf("setting temp config file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		if cerr := tmp.Close(); cerr != nil {
			slog.Debug("closing temp config file after write failure", "error", cerr)
		}
		return fmt.Errorf("writing temp config file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		if cerr := tmp.Close(); cerr != nil {
			slog.Debug("closing temp config file after sync failure", "error", cerr)
		}
		return fmt.Errorf("syncing temp config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp config file: %w", err)
	}

	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("renaming temp config file into place: %w", err)
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
