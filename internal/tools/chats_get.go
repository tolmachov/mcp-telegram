package tools

import (
	"context"
	"fmt"
	"sync"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsGetHandler handles the GetChats tool with server-side pagination.
// All chats are loaded on the first call (no cursor) and cached; subsequent
// calls with a cursor serve pages from the cache without hitting Telegram API.
type ChatsGetHandler struct {
	client    *tg.Client
	mu        sync.RWMutex
	cache     []tgdata.ChatInfo
	sessionID int64
}

// NewChatsGetHandler creates a new ChatsGetHandler. Panics if client is nil.
func NewChatsGetHandler(client *tg.Client) *ChatsGetHandler {
	if client == nil {
		panic("NewChatsGetHandler: nil client")
	}
	return &ChatsGetHandler{client: client}
}

// GetChatsInput is the input for the GetChats tool.
type GetChatsInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of chats per page (1-500; default 100)"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque pagination cursor from a previous GetChats response. Omit to load fresh data."`
}

// getChatsOutput is the structured output for the GetChats tool.
// Count is intentionally redundant with len(Chats) — LLMs parse flat
// top-level fields more reliably than computing array lengths.
type getChatsOutput struct {
	Chats          []tgdata.ChatInfo `json:"chats"`
	Count          int               `json:"count"` // == len(Chats)
	Total          int               `json:"total"`
	HasMore        bool              `json:"has_more"`
	NextCursor     string            `json:"next_cursor,omitempty"`
	PaginationHint string            `json:"pagination_hint,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *ChatsGetHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "GetChats",
		Description: "Get a paginated list of chats, groups, and channels. First call loads all chats (may be slow) and returns the first page. Pass the returned cursor to get subsequent pages from cache. Omit cursor to force a fresh reload. To find a specific chat by name, use SearchChats instead.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *ChatsGetHandler) handle(ctx context.Context, req *mcp.CallToolRequest, input GetChatsInput) (*mcp.CallToolResult, *getChatsOutput, error) {
	limit := clampLimit(input.Limit, 100, 500)

	if input.Cursor != "" {
		return h.handleWithCursor(input.Cursor, limit)
	}
	return h.handleFreshLoad(ctx, req, limit)
}

// handleFreshLoad fetches all chats from Telegram, caches them, and returns the first page.
func (h *ChatsGetHandler) handleFreshLoad(ctx context.Context, req *mcp.CallToolRequest, limit int) (*mcp.CallToolResult, *getChatsOutput, error) {
	onProgress := func(current int, message string) {
		sendProgress(ctx, req, float64(current), 0, message)
	}

	result, err := tgdata.GetChats(ctx, h.client, onProgress)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to get chats: %v", err)), nil, nil
	}

	sid := cryptoRandInt64() | 1 // guarantee non-zero to avoid collision with uninitialized sessionID
	h.mu.Lock()
	h.cache = result.Chats
	h.sessionID = sid
	h.mu.Unlock()

	return nil, h.pageFrom(result.Chats, sid, 0, limit), nil
}

// handleWithCursor serves a page from the cache using the provided cursor.
func (h *ChatsGetHandler) handleWithCursor(cursor string, limit int) (*mcp.CallToolResult, *getChatsOutput, error) {
	sid, offset, err := ParseChatsCursor(cursor)
	if err != nil {
		return errResult(fmt.Sprintf("Invalid cursor: %v. Call GetChats without cursor to start fresh.", err)), nil, nil
	}

	h.mu.RLock()
	defer h.mu.RUnlock()

	if h.cache == nil || sid != h.sessionID {
		return errResult("Cursor expired (cache was refreshed or server restarted). Call GetChats without cursor to start fresh."), nil, nil
	}
	if offset >= len(h.cache) {
		return errResult(fmt.Sprintf("Cursor offset %d is beyond the cached list (%d chats). Call GetChats without cursor to start fresh.", offset, len(h.cache))), nil, nil
	}

	return nil, h.pageFrom(h.cache, sid, offset, limit), nil
}

// pageFrom builds a getChatsOutput for the given slice window.
// Must be called while holding at least h.mu.RLock when using h.cache.
func (h *ChatsGetHandler) pageFrom(chats []tgdata.ChatInfo, sessionID int64, offset, limit int) *getChatsOutput {
	total := len(chats)
	end := min(offset+limit, total)
	page := chats[offset:end]
	hasMore := end < total

	out := &getChatsOutput{
		Chats: page,
		Count: len(page),
		Total: total,
	}
	if hasMore {
		out.HasMore = true
		out.NextCursor = FormatChatsCursor(sessionID, end)
		out.PaginationHint = fmt.Sprintf("Showing %d–%d of %d chats. Pass next_cursor to get more.", offset+1, end, total)
	}
	return out
}
