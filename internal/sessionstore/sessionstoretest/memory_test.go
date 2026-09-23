package sessionstoretest

import (
	"testing"
	"time"
)

// TestMemoryList pins that List reports stored sessions and that the
// injectable clock stamps writes.
func TestMemoryList(t *testing.T) {
	ctx := t.Context()
	m := NewMemory()
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	m.Now = func() time.Time { return stamp }

	if err := m.Session(7, "0123456789abcdef0123456789abcdef", nil).StoreSession(ctx, []byte("a")); err != nil {
		t.Fatalf("store a: %v", err)
	}

	refs, err := m.List(ctx)
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
