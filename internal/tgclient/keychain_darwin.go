//go:build darwin

package tgclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/session"
	"github.com/keybase/go-keychain"
)

const (
	keychainService = "mcp-telegram"
	keychainAccount = "telegram-session"
)

// SessionStorage implements session.Storage using macOS Keychain.
type SessionStorage struct{}

// NewSessionStorage creates a new SessionStorage.
func NewSessionStorage() *SessionStorage {
	return &SessionStorage{}
}

// LoadSession loads session data from Keychain.
func (s *SessionStorage) LoadSession(_ context.Context) ([]byte, error) {
	query := keychain.NewItem()
	query.SetSecClass(keychain.SecClassGenericPassword)
	query.SetService(keychainService)
	query.SetAccount(keychainAccount)
	query.SetMatchLimit(keychain.MatchLimitOne)
	query.SetReturnData(true)

	results, err := keychain.QueryItem(query)
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("querying keychain: %w", err)
	}

	if len(results) == 0 {
		return nil, session.ErrNotFound
	}

	return results[0].Data, nil
}

// StoreSession stores session data in Keychain.
func (s *SessionStorage) StoreSession(_ context.Context, data []byte) error {
	// First, try to delete existing item
	deleteItem := keychain.NewItem()
	deleteItem.SetSecClass(keychain.SecClassGenericPassword)
	deleteItem.SetService(keychainService)
	deleteItem.SetAccount(keychainAccount)
	_ = keychain.DeleteItem(deleteItem) // Ignore error if not found

	// Add new item
	item := keychain.NewItem()
	item.SetSecClass(keychain.SecClassGenericPassword)
	item.SetService(keychainService)
	item.SetAccount(keychainAccount)
	item.SetLabel("Telegram MCP Session")
	item.SetData(data)
	item.SetSynchronizable(keychain.SynchronizableNo)
	item.SetAccessible(keychain.AccessibleWhenUnlocked)

	if err := keychain.AddItem(item); err != nil {
		return fmt.Errorf("adding keychain item: %w", err)
	}
	return nil
}

// DeleteSession removes session data from Keychain.
func (s *SessionStorage) DeleteSession() error {
	item := keychain.NewItem()
	item.SetSecClass(keychain.SecClassGenericPassword)
	item.SetService(keychainService)
	item.SetAccount(keychainAccount)

	err := keychain.DeleteItem(item)
	if errors.Is(err, keychain.ErrorItemNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("deleting keychain item: %w", err)
	}
	return nil
}
