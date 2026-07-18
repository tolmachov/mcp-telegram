package sessionstore

import (
	"context"
	"sync"
	"time"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// memKey identifies one session: a user plus a per-authorization session id.
type memKey struct {
	userID tgid.UserID
	sid    string
}

// memBlob is one stored session blob and its last-write time.
type memBlob struct {
	data      []byte
	updatedAt time.Time
}

// Memory is an in-process Store for tests. Now is the write-timestamp clock;
// tests may override it (before use) to make List/sweep behavior deterministic.
type Memory struct {
	Now func() time.Time

	mu    sync.Mutex
	blobs map[memKey]memBlob
}

func NewMemory() *Memory {
	return &Memory{Now: time.Now, blobs: map[memKey]memBlob{}}
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

func (m *Memory) List(_ context.Context) ([]SessionRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	refs := make([]SessionRef, 0, len(m.blobs))
	for k, b := range m.blobs {
		refs = append(refs, SessionRef{UserID: k.userID, SID: k.sid, UpdatedAt: b.updatedAt})
	}
	return refs, nil
}

type memorySession struct {
	store *Memory
	key   memKey
}

func (s memorySession) LoadSession(_ context.Context) ([]byte, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	b, ok := s.store.blobs[s.key]
	if !ok || len(b.data) == 0 {
		return nil, session.ErrNotFound
	}
	cp := make([]byte, len(b.data))
	copy(cp, b.data)
	return cp, nil
}

func (s memorySession) StoreSession(_ context.Context, data []byte) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	s.store.blobs[s.key] = memBlob{data: cp, updatedAt: s.store.Now()}
	return nil
}
