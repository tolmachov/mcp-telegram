package sessionstore

import (
	"context"
	"sync"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// memKey identifies one session: a user plus a per-authorization session id.
type memKey struct {
	userID tgid.UserID
	sid    string
}

// Memory is an in-process Store for tests.
type Memory struct {
	mu    sync.Mutex
	blobs map[memKey][]byte
}

func NewMemory() *Memory {
	return &Memory{blobs: map[memKey][]byte{}}
}

func (m *Memory) Session(userID tgid.UserID, sid string, _ []byte) session.Storage {
	return memorySession{store: m, key: memKey{userID, sid}}
}

func (m *Memory) Exists(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[memKey{userID, sid}]
	return ok, nil
}

func (m *Memory) Delete(_ context.Context, userID tgid.UserID, sid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, memKey{userID, sid})
	return nil
}

type memorySession struct {
	store *Memory
	key   memKey
}

func (s memorySession) LoadSession(_ context.Context) ([]byte, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	data, ok := s.store.blobs[s.key]
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
	s.store.blobs[s.key] = cp
	return nil
}
