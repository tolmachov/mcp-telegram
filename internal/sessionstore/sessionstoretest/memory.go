// Package sessionstoretest provides an in-process sessionstore.Store for tests
// in other packages. The store is an Encrypted one over an in-memory backend,
// so tests exercise the same identity validation and encryption as
// production.
package sessionstoretest

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"sync"
	"testing"
	"time"

	"github.com/gotd/td/session"

	"github.com/tolmachov/mcp-telegram/internal/keyring"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// New returns an empty in-memory store whose blobs are stamped by the wall
// clock.
func New(t testing.TB) sessionstore.Store { return newStore(t, time.Now, nil) }

// NewWithClock returns an empty in-memory store whose blob and tombstone write
// times (the storage mtime other backends take from the filesystem or bucket)
// come from now, so List and sweep behaviour can be made deterministic. Grant
// expiry uses the time the caller passes in, as on every backend.
func NewWithClock(t testing.TB, now func() time.Time) sessionstore.Store {
	return newStore(t, now, nil)
}

// NewWithGrantWriteHook returns an empty in-memory store that calls hook
// before every backend grant write. A non-nil error from hook fails that
// write without it landing, the way an unreachable backend does, so tests can
// fault the grant rules without reaching past them.
func NewWithGrantWriteHook(t testing.TB, hook func() error) sessionstore.Store {
	return newStore(t, time.Now, hook)
}

func newStore(t testing.TB, now func() time.Time, grantWriteHook func() error) sessionstore.Store {
	t.Helper()
	master := make([]byte, keyring.MasterKeyLen)
	_, _ = rand.Read(master) // crypto/rand.Read never returns an error
	ring, err := keyring.Parse([]string{base64.StdEncoding.EncodeToString(master)})
	if err != nil {
		t.Fatalf("sessionstoretest: building key ring: %v", err)
	}
	m := &memory{
		now: now, grantWriteHook: grantWriteHook,
		blobs: map[memKey]memBlob{}, revoked: map[memKey]time.Time{}, grants: map[string]memGrant{},
	}
	return sessionstore.Encrypted(m, sessionstore.NewCipher(ring, "https://sessionstoretest.invalid"))
}

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

// memGrant is one authorization-code family's refresh-grant record and its
// write counter, the version StoreGrant compares.
type memGrant struct {
	record  sessionstore.GrantRecord
	version int64
}

// memory is the in-process storage backend behind New.
type memory struct {
	now func() time.Time
	// grantWriteHook, when set, runs before every StoreGrant and fails it
	// with its error.
	grantWriteHook func() error

	mu      sync.Mutex
	blobs   map[memKey]memBlob
	revoked map[memKey]time.Time
	grants  map[string]memGrant
}

func (m *memory) Session(userID tgid.UserID, sid string) session.Storage {
	return memorySession{store: m, key: memKey{userID, sid}}
}

func (m *memory) Exists(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[memKey{userID, sid}]
	return ok && len(b.data) > 0, nil
}

func (m *memory) Delete(_ context.Context, userID tgid.UserID, sid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.blobs, memKey{userID, sid})
	return nil
}

func (m *memory) List(_ context.Context) ([]sessionstore.SessionRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	refs := make([]sessionstore.SessionRef, 0, len(m.blobs))
	for k, b := range m.blobs {
		refs = append(refs, sessionstore.SessionRef{UserID: k.userID, SID: k.sid, UpdatedAt: b.updatedAt})
	}
	return refs, nil
}

func (m *memory) Revoke(_ context.Context, userID tgid.UserID, sid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := memKey{userID, sid}
	m.revoked[k] = m.now()
	delete(m.blobs, k)
	return nil
}

func (m *memory) Revoked(_ context.Context, userID tgid.UserID, sid string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.revoked[memKey{userID, sid}]
	return ok, nil
}

func (m *memory) ListRevoked(_ context.Context) ([]sessionstore.SessionRef, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	refs := make([]sessionstore.SessionRef, 0, len(m.revoked))
	for k, t := range m.revoked {
		refs = append(refs, sessionstore.SessionRef{UserID: k.userID, SID: k.sid, UpdatedAt: t})
	}
	return refs, nil
}

func (m *memory) DeleteRevoked(_ context.Context, userID tgid.UserID, sid string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.revoked, memKey{userID, sid})
	return nil
}

func (m *memory) LoadGrant(_ context.Context, family string) (sessionstore.GrantRecord, int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	g := m.grants[family]
	return g.record, g.version, nil
}

func (m *memory) StoreGrant(_ context.Context, family string, grant sessionstore.GrantRecord, version int64) error {
	if m.grantWriteHook != nil {
		if err := m.grantWriteHook(); err != nil {
			return err
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	current := m.grants[family]
	if current.version != version {
		return &sessionstore.GrantConflictError{Current: current.record, Version: current.version}
	}
	m.grants[family] = memGrant{record: grant, version: current.version + 1}
	return nil
}

// SweepAuthState deletes the expired grants. Memory records always decode,
// so it never reports an undecodable one.
func (m *memory) SweepAuthState(_ context.Context, now time.Time) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for family, grant := range m.grants {
		if grant.record.Expired(now) {
			delete(m.grants, family)
		}
	}
	return nil, nil
}

type memorySession struct {
	store *memory
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
	s.store.blobs[s.key] = memBlob{data: cp, updatedAt: s.store.now()}
	return nil
}
