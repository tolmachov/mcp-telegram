package sessionstore

import (
	"context"
	"sync"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// Memory is an in-process Store for tests.
type Memory struct {
	mu    sync.Mutex
	blobs map[tgid.UserID][]byte
}

func NewMemory() *Memory {
	return &Memory{blobs: map[tgid.UserID][]byte{}}
}

func (m *Memory) Session(userID tgid.UserID) session.Storage {
	return memorySession{store: m, userID: userID}
}

func (m *Memory) Exists(_ context.Context, userID tgid.UserID) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[userID]
	return ok, nil
}

func (m *Memory) Delete(_ context.Context, userID tgid.UserID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, userID)
	return nil
}

type memorySession struct {
	store  *Memory
	userID tgid.UserID
}

func (s memorySession) LoadSession(_ context.Context) ([]byte, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	data, ok := s.store.blobs[s.userID]
	if !ok || len(data) == 0 {
		return nil, session.ErrNotFound
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	return cp, nil
}

func (s memorySession) StoreSession(_ context.Context, data []byte) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	s.store.blobs[s.userID] = cp
	return nil
}
