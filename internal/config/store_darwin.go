//go:build darwin

package config

import (
	"errors"
	"fmt"

	"github.com/keybase/go-keychain"
)

const (
	keychainService       = "mcp-telegram"
	keychainAccountPrefix = "config:"
)

type keychainStore struct{}

// NewStore creates a new config store backed by macOS Keychain.
func NewStore() Store {
	return &keychainStore{}
}

func keychainAccount(key string) string {
	return keychainAccountPrefix + key
}

func (s *keychainStore) Get(key string) (string, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(keychainService)
	query.SetAccount(keychainAccount(key))
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)

	results, err := keychain.QueryItem(query)
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("querying keychain for %s: %w", key, err)
	}

	if len(results) == 0 {
		return "", ErrNotFound
	}

	return string(results[0].Data), nil
}

func (s *keychainStore) Set(key, value string) error {
	// Delete existing item first.
	deleteItem := keychain.NewItem()
	deleteItem.SetSecClass(keychain.SecClassGenericPassword)
	deleteItem.SetService(keychainService)
	deleteItem.SetAccount(keychainAccount(key))
	_ = keychain.DeleteItem(deleteItem)

	item := keychain.NewItem()
	item.SetSecClass(keychain.SecClassGenericPassword)
	item.SetService(keychainService)
	item.SetAccount(keychainAccount(key))
	item.SetLabel("mcp-telegram config: " + key)
	item.SetData([]byte(value))
	item.SetSynchronizable(keychain.SynchronizableNo)
	item.SetAccessible(keychain.AccessibleWhenUnlocked)

	if err := keychain.AddItem(item); err != nil {
		return fmt.Errorf("storing %s in keychain: %w", key, err)
	}

	return nil
}

func (s *keychainStore) Delete(key string) error {
	item := keychain.NewItem()
	item.SetSecClass(keychain.SecClassGenericPassword)
	item.SetService(keychainService)
	item.SetAccount(keychainAccount(key))

	err := keychain.DeleteItem(item)
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting %s from keychain: %w", key, err)
	}

	return nil
}

func (s *keychainStore) List() ([]string, error) {
	var keys []string

	for key := range allowedKeys {
		_, err := s.Get(key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}

	return keys, nil
}

func (s *keychainStore) LoadAll() (map[string]string, error) {
	values := make(map[string]string)

	for key := range allowedKeys {
		val, err := s.Get(key)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		values[key] = val
	}

	return values, nil
}
