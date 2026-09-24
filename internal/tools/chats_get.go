package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsGetHandler handles the GetChats tool with server-side pagination.
// Every call without a cursor reloads all chats into the shared
// tgdata.ChatsCache and returns the first page of that snapshot. A cursor
// names the snapshot it came from, so the next pages come from the same
// listing without hitting the Telegram API, even after other readers
// (SearchChats, completion, the chats resource) have loaded newer ones: a
// page that hands out a cursor keeps its snapshot in the cache, for a bounded
// time and among a bounded number of kept ones.
type ChatsGetHandler struct {
	cache *tgdata.ChatsCache
}

// NewChatsGetHandler creates a new ChatsGetHandler. Panics if cache is nil.
func NewChatsGetHandler(cache *tgdata.ChatsCache) *ChatsGetHandler {
	if cache == nil {
		panic("NewChatsGetHandler: nil cache")
	}
	return &ChatsGetHandler{cache: cache}
}

// GetChatsInput is the input for the GetChats tool.
type GetChatsInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum number of chats per page (1-500; default 100)"`
	Cursor string `json:"cursor,omitempty" jsonschema:"Opaque pagination cursor from a previous GetChats response; continues the same listing. Omit to load fresh data."`
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
	// The warning is set when the underlying listing is truncated, so a
	// chat's absence from Chats/Total does not prove it doesn't exist.
	partialOutcome
}

// truncatedChatsWarning explains an incomplete dialog listing to the model. Used
// by both GetChats and SearchChats so they describe the condition identically.
const truncatedChatsWarning = "Chat listing is incomplete: dialog pagination stalled before all chats were fetched, so some chats may be missing. A chat's absence is not proof it doesn't exist. Retry GetChats (omit cursor) to reload."

// Register adds the tool to the MCP server.
func (h *ChatsGetHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "GetChats",
		Description: "Get a paginated list of chats, groups, and channels. First call loads all chats (may be slow) and returns the first page. Pass the returned cursor to get subsequent pages of the same listing from cache. Omit cursor to force a fresh reload. To find a specific chat by name, use SearchChats instead.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: new(true)},
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

	snap, err := h.cache.Load(ctx, onProgress, true)
	if err != nil {
		return nil, nil, failed("get chats", err)
	}

	return nil, h.pageFrom(snap, 0, limit), nil
}

// handleWithCursor serves a page from the cache using the provided cursor.
func (h *ChatsGetHandler) handleWithCursor(cursor string, limit int) (*mcp.CallToolResult, *getChatsOutput, error) {
	sid, offset, err := ParseChatsCursor(cursor)
	if err != nil {
		return ErrResult(fmt.Sprintf("Invalid cursor: %v. Call GetChats without cursor to start fresh.", err)), nil, nil
	}

	snap, ok := h.cache.Snapshot(sid)
	if !ok {
		return ErrResult("Cursor expired (its chat listing is too old, was replaced by newer loads, or the server restarted). Call GetChats without cursor to start fresh."), nil, nil
	}
	if offset >= len(snap.Chats) {
		return ErrResult(fmt.Sprintf("Cursor offset %d is beyond the cached list (%d chats). Call GetChats without cursor to start fresh.", offset, len(snap.Chats))), nil, nil
	}

	return nil, h.pageFrom(snap, offset, limit), nil
}

// pageFrom builds a getChatsOutput for the given window of snap.
func (h *ChatsGetHandler) pageFrom(snap *tgdata.ChatsSnapshot, offset, limit int) *getChatsOutput {
	total := len(snap.Chats)
	end := min(offset+limit, total)
	page := snap.Chats[offset:end]
	hasMore := end < total

	out := &getChatsOutput{
		Chats: page,
		Count: len(page),
		Total: total,
	}
	if hasMore {
		h.cache.Keep(snap)
		out.HasMore = true
		out.NextCursor = FormatChatsCursor(snap.ID, end)
		out.PaginationHint = fmt.Sprintf("Showing %d–%d of %d chats. Pass next_cursor to get more.", offset+1, end, total)
	}
	if snap.Truncated {
		out.warn(truncatedChatsWarning)
	}
	return out
}
