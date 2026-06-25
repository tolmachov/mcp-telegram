//go:build darwin

package tgclient

import (
	"context"
	"errors"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/secret"
)

// sessionVault is the narrow slice of *secret.Vault that SessionStorage
// actually needs. Declaring it here (rather than taking *secret.Vault
// directly) lets tests substitute an in-memory fake without depending on
// the Keychain — the concrete *secret.Vault satisfies it structurally.
type sessionVault interface {
	SessionLoad() ([]byte, error)
	SessionStore(data []byte) error
	SessionDelete() error
}

// SessionStorage implements session.Storage over the shared macOS Keychain
// vault. The session bytes live in the same keychain item as the config blob,
// so reading/writing the session never triggers a separate password prompt.
type SessionStorage struct {
	vault sessionVault
}

// NewSessionStorage returns a session.Storage bound to the shared vault.
func NewSessionStorage() (*SessionStorage, error) {
	return &SessionStorage{vault: secret.Shared()}, nil
}

func (s *SessionStorage) LoadSession(_ context.Context) ([]byte, error) {
	data, err := s.vault.SessionLoad()
	if errors.Is(err, secret.ErrNotFound) {
		return nil, session.ErrNotFound
	}
	return data, err
}

func (s *SessionStorage) StoreSession(_ context.Context, data []byte) error {
	return s.vault.SessionStore(data)
}

func (s *SessionStorage) DeleteSession() error {
	return s.vault.SessionDelete()
}
