package tools

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

func TestChatsCursorRoundTrip(t *testing.T) {
	tests := []struct {
		sessionID int64
		offset    int
	}{
		{42, 0},
		{-1, 100},
		{9999999999, 500},
	}
	for _, tt := range tests {
		cursor := FormatChatsCursor(tt.sessionID, tt.offset)
		gotSession, gotOffset, err := ParseChatsCursor(cursor)
		if err != nil {
			t.Fatalf("ParseChatsCursor(%q) error: %v", cursor, err)
		}
		if gotSession != tt.sessionID || gotOffset != tt.offset {
			t.Errorf("round-trip mismatch: got (%d, %d), want (%d, %d)", gotSession, gotOffset, tt.sessionID, tt.offset)
		}
	}
}

func TestFormatChatsCursorPanicsOnNegativeOffset(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("FormatChatsCursor with negative offset did not panic")
		}
	}()
	FormatChatsCursor(1, -1)
}

func TestParseChatsCursorInvalid(t *testing.T) {
	tests := []struct {
		name   string
		cursor string
	}{
		{"empty", ""},
		{"whitespace", " abc "},
		{"not base64", "!!!"},
		{"not json", base64.RawURLEncoding.EncodeToString([]byte("not json"))},
		{"future version", base64.RawURLEncoding.EncodeToString([]byte(`{"v":99,"s":1,"o":0}`))},
		{"negative offset", base64.RawURLEncoding.EncodeToString([]byte(`{"v":1,"s":1,"o":-5}`))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := ParseChatsCursor(tt.cursor)
			if err == nil {
				t.Errorf("ParseChatsCursor(%q) expected error, got nil", tt.cursor)
			}
		})
	}
}

func TestParseChatsCursorRejectsLegacyVersion(t *testing.T) {
	for _, cursor := range []string{
		base64.RawURLEncoding.EncodeToString([]byte(`{"v":0,"s":7,"o":3}`)),
		base64.RawURLEncoding.EncodeToString([]byte(`{"s":7,"o":3}`)),
	} {
		_, _, err := ParseChatsCursor(cursor)
		if err == nil {
			t.Errorf("ParseChatsCursor(%q) accepted obsolete cursor", cursor)
		}
	}
}

func makeChats(n int) []tgdata.ChatInfo {
	chats := make([]tgdata.ChatInfo, n)
	for i := range chats {
		chats[i] = tgdata.ChatInfo{ID: int64(i + 1), Name: "chat"}
	}
	return chats
}

// seededChatsCache returns a cache already holding one snapshot of chats,
// kept as a handed-out cursor keeps it.
func seededChatsCache(t *testing.T, chats []tgdata.ChatInfo, truncated bool) (*tgdata.ChatsCache, *tgdata.ChatsSnapshot) {
	t.Helper()
	cache := tgdata.NewChatsCache(t.Context(), func(context.Context, tgdata.ProgressFunc) (*tgdata.ChatsList, error) {
		return &tgdata.ChatsList{Chats: chats, Count: len(chats), Truncated: truncated}, nil
	})
	snap, err := cache.Load(t.Context(), nil, false)
	require.NoError(t, err)
	cache.Keep(snap)
	return cache, snap
}

func TestPageFrom(t *testing.T) {
	const sid int64 = 42

	tests := []struct {
		name       string
		total      int
		offset     int
		limit      int
		wantCount  int
		wantMore   bool
		wantCurOff int // expected cursor offset; -1 if no cursor
	}{
		{"empty list", 0, 0, 10, 0, false, -1},
		{"exactly one page", 5, 0, 5, 5, false, -1},
		{"limit exceeds total", 5, 0, 10, 5, false, -1},
		{"first of multiple pages", 10, 0, 3, 3, true, 3},
		{"middle page", 10, 3, 3, 3, true, 6},
		{"last page partial", 10, 9, 3, 1, false, -1},
		{"last page exact boundary", 10, 6, 4, 4, false, -1},
		{"single item", 1, 0, 100, 1, false, -1},
	}

	h := &ChatsGetHandler{cache: tgdata.NewChatsCache(t.Context(), nil)}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out := h.pageFrom(&tgdata.ChatsSnapshot{ID: sid, Chats: makeChats(tt.total)}, tt.offset, tt.limit)

			if out.Count != tt.wantCount {
				t.Errorf("Count = %d, want %d", out.Count, tt.wantCount)
			}
			if out.Count != len(out.Chats) {
				t.Errorf("Count (%d) != len(Chats) (%d)", out.Count, len(out.Chats))
			}
			if out.Total != tt.total {
				t.Errorf("Total = %d, want %d", out.Total, tt.total)
			}
			if out.HasMore != tt.wantMore {
				t.Errorf("HasMore = %v, want %v", out.HasMore, tt.wantMore)
			}

			if tt.wantCurOff < 0 {
				if out.NextCursor != "" {
					t.Errorf("expected no cursor, got %q", out.NextCursor)
				}
			} else {
				gotSid, gotOff, err := ParseChatsCursor(out.NextCursor)
				if err != nil {
					t.Fatalf("ParseChatsCursor(NextCursor) error: %v", err)
				}
				if gotSid != sid {
					t.Errorf("cursor sessionID = %d, want %d", gotSid, sid)
				}
				if gotOff != tt.wantCurOff {
					t.Errorf("cursor offset = %d, want %d", gotOff, tt.wantCurOff)
				}

			}

			if tt.wantMore && out.PaginationHint == "" {
				t.Error("HasMore=true but PaginationHint is empty")
			}
		})
	}
}

// TestGetChatsKeepsOnlyCursoredSnapshots verifies a fresh load keeps its
// snapshot for cursors only when a page hands one out.
func TestGetChatsKeepsOnlyCursoredSnapshots(t *testing.T) {
	cache := tgdata.NewChatsCache(t.Context(), func(context.Context, tgdata.ProgressFunc) (*tgdata.ChatsList, error) {
		return &tgdata.ChatsList{Chats: makeChats(5), Count: 5}, nil
	})
	h := NewChatsGetHandler(cache)

	_, whole, err := h.handle(t.Context(), &mcp.CallToolRequest{}, GetChatsInput{Limit: 10})
	require.NoError(t, err)
	require.Empty(t, whole.NextCursor)
	newest, err := cache.Load(t.Context(), nil, false)
	require.NoError(t, err)
	_, kept := cache.Snapshot(newest.ID)
	assert.False(t, kept, "a listing served whole needs no cursor")

	_, paged, err := h.handle(t.Context(), &mcp.CallToolRequest{}, GetChatsInput{Limit: 2})
	require.NoError(t, err)
	sid, _, err := ParseChatsCursor(paged.NextCursor)
	require.NoError(t, err)
	_, kept = cache.Snapshot(sid)
	assert.True(t, kept, "the snapshot a cursor names is kept")
}

func TestHandleWithCursorErrors(t *testing.T) {
	chats := makeChats(5)
	cache, snap := seededChatsCache(t, chats, false)
	sid := snap.ID

	h := &ChatsGetHandler{cache: cache}

	t.Run("session mismatch", func(t *testing.T) {
		cursor := FormatChatsCursor(sid+1, 0)
		result, out, err := h.handleWithCursor(cursor, 10)
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if out != nil {
			t.Fatal("expected nil output on error")
		}
		if result == nil || !result.IsError {
			t.Fatal("expected error result")
		}
		text := result.Content[0].(*mcp.TextContent).Text
		if !strings.Contains(text, "expired") {
			t.Errorf("expected 'expired' in error, got: %s", text)
		}
	})

	t.Run("offset beyond cache", func(t *testing.T) {
		cursor := FormatChatsCursor(sid, len(chats))
		result, out, err := h.handleWithCursor(cursor, 10)
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if out != nil {
			t.Fatal("expected nil output on error")
		}
		if result == nil || !result.IsError {
			t.Fatal("expected error result")
		}
		text := result.Content[0].(*mcp.TextContent).Text
		if !strings.Contains(text, "beyond") {
			t.Errorf("expected 'beyond' in error, got: %s", text)
		}
	})

	t.Run("valid cursor returns page", func(t *testing.T) {
		cursor := FormatChatsCursor(sid, 2)
		result, out, err := h.handleWithCursor(cursor, 2)
		if err != nil {
			t.Fatalf("unexpected Go error: %v", err)
		}
		if result != nil {
			t.Fatalf("unexpected error result: %v", result)
		}
		if out.Count != 2 {
			t.Errorf("Count = %d, want 2", out.Count)
		}
		if !out.HasMore {
			t.Error("expected HasMore=true")
		}
	})

	t.Run("unloaded cache", func(t *testing.T) {
		h2 := &ChatsGetHandler{cache: tgdata.NewChatsCache(t.Context(), nil)}
		cursor := FormatChatsCursor(0, 0)
		result, out, _ := h2.handleWithCursor(cursor, 10)
		if out != nil {
			t.Fatal("expected nil output on error")
		}
		if result == nil || !result.IsError {
			t.Fatal("expected error result")
		}
	})
}
