package sessionstoretest

import (
	"crypto/rand"
	"testing"
	"time"
)

// TestList pins that List reports stored sessions and that the injected clock
// stamps writes.
func TestList(t *testing.T) {
	ctx := t.Context()
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	store := NewWithClock(t, func() time.Time { return stamp })
	key := make([]byte, 32)
	_, _ = rand.Read(key)

	if err := store.Session(7, "0123456789abcdef0123456789abcdef", key).StoreSession(ctx, []byte("a")); err != nil {
		t.Fatalf("store a: %v", err)
	}

	refs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 1 {
		t.Fatalf("List returned %d refs, want 1", len(refs))
	}
	for _, r := range refs {
		if r.UserID != 7 || !r.UpdatedAt.Equal(stamp) {
			t.Errorf("ref = %+v, want user 7 at %v", r, stamp)
		}
	}
}
