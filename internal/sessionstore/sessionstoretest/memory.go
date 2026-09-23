// Package sessionstoretest provides an in-process sessionstore.Store for tests
// in other packages.
package sessionstoretest

import (
	"context"
	"sync"
	"time"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
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

// Memory is an in-process Store for tests. Now is the blob write-timestamp
// clock (the storage mtime other backends take from the filesystem or bucket);
// tests may override it (before use) to make List/sweep behaviour
// deterministic. Grant expiry uses the time the caller passes in, as on every
// backend.
type Memory struct {
	Now func() time.Time

	mu      sync.Mutex
	blobs   map[memKey]memBlob
	revoked map[memKey]time.Time
	grants  map[string]memGrant
}

// memGrant is one authorization-code family's refresh-grant record and its
// write counter, the version StoreGrant compares.
type memGrant struct {
	record  sessionstore.GrantRecord
	version int64
}

// NewMemory returns an empty in-memory store stamped by the wall clock.
func NewMemory() *Memory {
	return &Memory{Now: time.Now, blobs: map[memKey]memBlob{}, revoked: map[memKey]time.Time{}, grants: map[string]memGrant{}}
}

// Session returns the blob storage for one session. A malformed sid fails
// every operation with sessionstore.ErrInvalidSID.
func (m *Memory) Session(userID tgid.UserID, sid string, _ []byte) session.Storage {
	return memorySession{store: m, key: memKey{userID, sid}}
}

func (m *Memory) Exists(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !sessionstore.ValidSID(sid) {
		return false, sessionstore.ErrInvalidSID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[memKey{userID, sid}]
	return ok && len(b.data) > 0, nil
}

func (m *Memory) Delete(_ context.Context, userID tgid.UserID, sid string) error {
	if !sessionstore.ValidSID(sid) {
		return sessionstore.ErrInvalidSID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, memKey{userID, sid})
	return nil
}

func (m *Memory) List(_ context.Context) ([]sessionstore.SessionRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	refs := make([]sessionstore.SessionRef, 0, len(m.blobs))
	for k, b := range m.blobs {
		refs = append(refs, sessionstore.SessionRef{UserID: k.userID, SID: k.sid, UpdatedAt: b.updatedAt})
	}
	return refs, nil
}

func (m *Memory) Revoke(_ context.Context, userID tgid.UserID, sid string) error {
	if !sessionstore.ValidSID(sid) {
		return sessionstore.ErrInvalidSID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey{userID, sid}
	m.revoked[k] = m.Now()
	delete(m.blobs, k)
	return nil
}

func (m *Memory) Revoked(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	if !sessionstore.ValidSID(sid) {
		return false, sessionstore.ErrInvalidSID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.revoked[memKey{userID, sid}]
	return ok, nil
}

func (m *Memory) ListRevoked(_ context.Context) ([]sessionstore.SessionRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	refs := make([]sessionstore.SessionRef, 0, len(m.revoked))
	for k, t := range m.revoked {
		refs = append(refs, sessionstore.SessionRef{UserID: k.userID, SID: k.sid, UpdatedAt: t})
	}
	return refs, nil
}

func (m *Memory) DeleteRevoked(_ context.Context, userID tgid.UserID, sid string) error {
	if !sessionstore.ValidSID(sid) {
		return sessionstore.ErrInvalidSID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.revoked, memKey{userID, sid})
	return nil
}

func (m *Memory) LoadGrant(_ context.Context, family string) (sessionstore.GrantRecord, int64, error) {
	if !sessionstore.ValidSID(family) {
		return sessionstore.GrantRecord{}, 0, sessionstore.ErrInvalidSID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.grants[family]
	return g.record, g.version, nil
}

func (m *Memory) StoreGrant(_ context.Context, family string, grant sessionstore.GrantRecord, version int64) error {
	if !sessionstore.ValidSID(family) || !sessionstore.ValidSID(grant.SID) {
		return sessionstore.ErrInvalidSID
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.grants[family]
	if current.version != version {
		return sessionstore.ErrGrantConflict
	}
	m.grants[family] = memGrant{record: grant, version: current.version + 1}
	return nil
}

func (m *Memory) SweepAuthState(_ context.Context, now time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for family, grant := range m.grants {
		if grant.record.Expired(now) {
			delete(m.grants, family)
		}
	}
	return nil
}

type memorySession struct {
	store *Memory
	key   memKey
}

func (s memorySession) LoadSession(_ context.Context) ([]byte, error) {
	if !sessionstore.ValidSID(s.key.sid) {
		return nil, sessionstore.ErrInvalidSID
	}
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
	if !sessionstore.ValidSID(s.key.sid) {
		return sessionstore.ErrInvalidSID
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	cp := make([]byte, len(data))
	copy(cp, data)
	s.store.blobs[s.key] = memBlob{data: cp, updatedAt: s.store.Now()}
	return nil
}
