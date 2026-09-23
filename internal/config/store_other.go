//go:build !darwin

package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

// storeName names the store in errors.
const storeName = "config store"

// fileStore uses one atomically replaced 0600 file per key. Independent keys
// cannot overwrite each other, even when separate processes update them at the
// same time.
type fileStore struct{ dir string }

func NewStore() (Store, error) {
	dir, err := configPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating config directory: %w", err)
	}
	//nolint:gosec // G302: 0o700 is the correct restrictive mode for a directory (needs the execute bit to be traversable).
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("tightening config directory permissions: %w", err)
	}
	return &fileStore{dir: dir}, nil
}

func configPath() (string, error) {
	stateDir, err := xdg.StateDir()
	if err != nil {
		return "", fmt.Errorf("cannot determine config storage location: %w; set XDG_STATE_HOME or ensure HOME is set", err)
	}
	return filepath.Join(stateDir, "config-v2"), nil
}

func (s *fileStore) keyPath(key string) string {
	name := base64.RawURLEncoding.EncodeToString([]byte(key))
	return filepath.Join(s.dir, name)
}

func (s *fileStore) Get(key string) (string, error) {
	data, err := os.ReadFile(s.keyPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("reading config key %s: %w", key, err)
	}
	return string(data), nil
}

func (s *fileStore) Set(key, value string) error {
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("config key is empty")
	}
	if err := xdg.WriteFileAtomic(s.keyPath(key), []byte(value), 0o600, ".config-key-*.tmp"); err != nil {
		return fmt.Errorf("saving config key %s: %w", key, err)
	}
	return nil
}

func (s *fileStore) Delete(key string) error {
	err := os.Remove(s.keyPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting config key %s: %w", key, err)
	}
	return nil
}

func (s *fileStore) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("listing config keys: %w", err)
	}
	keys := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(entry.Name())
		if err != nil || len(raw) == 0 {
			continue
		}
		keys = append(keys, string(raw))
	}
	sort.Strings(keys)
	return keys, nil
}
