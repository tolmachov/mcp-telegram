package tools

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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
		{"version zero", base64.RawURLEncoding.EncodeToString([]byte(`{"v":0,"s":1,"o":0}`))},
		{"missing version", base64.RawURLEncoding.EncodeToString([]byte(`{"s":1,"o":0}`))},
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

func makeChats(n int) []tgdata.ChatInfo {
	chats := make([]tgdata.ChatInfo, n)
	for i := range chats {
		chats[i] = tgdata.ChatInfo{ID: int64(i + 1), Name: "chat"}
	}
	return chats
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

	h := &ChatsGetHandler{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chats := makeChats(tt.total)
			out := h.pageFrom(chats, sid, tt.offset, tt.limit)

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

func TestHandleWithCursorErrors(t *testing.T) {
	chats := makeChats(5)
	const sid int64 = 99

	h := &ChatsGetHandler{}
	h.cache = chats
	h.sessionID = sid

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

	t.Run("nil cache", func(t *testing.T) {
		h2 := &ChatsGetHandler{}
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
