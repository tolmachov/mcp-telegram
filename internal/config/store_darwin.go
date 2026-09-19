//go:build darwin

package config

import (
	"errors"

	"github.com/tolmachov/mcp-telegram/internal/secret"
)

// keychainStore is a thin adapter over the shared macOS Keychain vault. The
// vault is what actually reads/writes independent generic-password items; this
// type exists to satisfy the Store interface and translate secret.ErrNotFound
// into config.ErrNotFound.
type keychainStore struct {
	vault *secret.Vault
}

// NewStore returns a config store backed by the versioned Keychain service.
// Every operation observes Keychain directly; there is no process cache.
func NewStore() (Store, error) {
	return &keychainStore{vault: secret.Shared()}, nil
}

func (s *keychainStore) Get(key string) (string, error) {
	val, err := s.vault.ConfigGet(key)
	if errors.Is(err, secret.ErrNotFound) {
		return "", ErrNotFound
	}
	return val, err
}

func (s *keychainStore) Set(key, value string) error {
	return s.vault.ConfigSet(key, value)
}

func (s *keychainStore) Delete(key string) error {
	return s.vault.ConfigDelete(key)
}

func (s *keychainStore) List() ([]string, error) {
	return s.vault.ConfigList()
}

func (s *keychainStore) LoadAll() (map[string]string, error) {
	return s.vault.ConfigLoadAll()
}
