package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsSearchHandler handles the SearchChats tool
type ChatsSearchHandler struct {
	client *tg.Client
}

// NewChatsSearchHandler creates a new ChatsSearchHandler
func NewChatsSearchHandler(client *tg.Client) *ChatsSearchHandler {
	return &ChatsSearchHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *ChatsSearchHandler) Tool() mcp.Tool {
	return mcp.NewTool("SearchChats",
		mcp.WithDescription("Search for chats, groups, and channels by name using fuzzy matching."),
		mcp.WithString("query",
			mcp.Required(),
			mcp.Description("Search query to match against chat names"),
		),
		mcp.WithNumber("limit",
			mcp.Description("Maximum number of results to return (default: 10, max: 50)"),
		),
	)
}

// SearchResult represents a single search result with a match score
type SearchResult struct {
	tgdata.ChatInfo
	Score int `json:"score"` // Lower is a better match (Levenshtein distance)
}

// SearchResultsList represents the search results
type SearchResultsList struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
	Count   int            `json:"count"`
}

// Handle processes the SearchChats tool request
func (h *ChatsSearchHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	query := mcp.ParseString(request, "query", "")
	if query == "" {
		return mcp.NewToolResultError("query parameter is required"), nil
	}

	limit := mcp.ParseInt(request, "limit", 10)
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}

	// Get all user's chats for local fuzzy search first
	onProgress := func(current int, message string) {
		if srv := server.ServerFromContext(ctx); srv != nil {
			_ = srv.SendNotificationToClient(ctx, "notifications/progress", map[string]any{
				"progress": current,
				"message":  message,
			})
		}
	}
	chatsList, err := tgdata.GetChats(ctx, h.client, onProgress)
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to get chats: %v", err)), nil
	}

	// Perform local fuzzy search first
	results := h.fuzzySearchLocal(query, chatsList.Chats, limit)

	// Only search globally if we have room for more results
	if len(results) < limit {
		globalResults, err := h.searchGlobal(ctx, query)
		if err == nil && len(globalResults) > 0 {
			results = h.addGlobalResults(query, results, globalResults, limit)
		}
	}

	resultsList := SearchResultsList{
		Query:   query,
		Results: results,
		Count:   len(results),
	}

	data, err := json.MarshalIndent(resultsList, "", "  ")
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to marshal results: %v", err)), nil
	}

	return mcp.NewToolResultText(string(data)), nil
}

// searchGlobal performs Telegram's global search by username
func (h *ChatsSearchHandler) searchGlobal(ctx context.Context, query string) ([]tgdata.ChatInfo, error) {
	found, err := h.client.ContactsSearch(ctx, &tg.ContactsSearchRequest{
		Q:     query,
		Limit: 20,
	})
	if err != nil {
		return nil, fmt.Errorf("searching contacts: %w", err)
	}

	var results []tgdata.ChatInfo

	// Process users
	for _, user := range found.Users {
		u, ok := user.(*tg.User)
		if !ok || u.Bot {
			continue
		}

		chatType := "user"
		if u.Bot {
			chatType = "bot"
		}

		results = append(results, tgdata.ChatInfo{
			ID:       u.ID,
			Type:     chatType,
			Name:     tgclient.UserDisplayName(u),
			Username: u.Username,
		})
	}

	// Process chats
	for _, chat := range found.Chats {
		switch c := chat.(type) {
		case *tg.Chat:
			results = append(results, tgdata.ChatInfo{
				ID:   c.ID,
				Type: "group",
				Name: c.Title,
			})
		case *tg.Channel:
			chatType := "channel"
			if c.Megagroup {
				chatType = "supergroup"
			}
			// Convert to user-facing format with -100 prefix
			id := -1000000000000 - c.ID
			results = append(results, tgdata.ChatInfo{
				ID:       id,
				Type:     chatType,
				Name:     c.Title,
				Username: c.Username,
			})
		}
	}

	return results, nil
}

// fuzzySearchLocal performs fuzzy search on local chats only
func (h *ChatsSearchHandler) fuzzySearchLocal(query string, chats []tgdata.ChatInfo, limit int) []SearchResult {
	// Create a slice of chat names for fuzzy matching
	names := make([]string, len(chats))
	for i, chat := range chats {
		names[i] = chat.Name
	}

	// Find matches using fuzzy search (RankFindNormalizedFold is already case-insensitive)
	matches := fuzzy.RankFindNormalizedFold(query, names)

	// Sort by score (lower distance = better match)
	sort.Slice(matches, func(i, j int) bool {
		return matches[i].Distance < matches[j].Distance
	})

	seen := make(map[int64]bool)
	var results []SearchResult

	for _, match := range matches {
		if len(results) >= limit {
			break
		}
		if match.OriginalIndex < len(chats) {
			chat := chats[match.OriginalIndex]
			if !seen[chat.ID] {
				seen[chat.ID] = true
				results = append(results, SearchResult{
					ChatInfo: chat,
					Score:    match.Distance,
				})
			}
		}
	}

	return results
}

// addGlobalResults adds global search results to fill remaining slots
func (h *ChatsSearchHandler) addGlobalResults(query string, localResults []SearchResult, globalChats []tgdata.ChatInfo, limit int) []SearchResult {
	if len(localResults) >= limit {
		return localResults
	}

	// Track already seen IDs from local results
	seen := make(map[int64]bool)
	for _, r := range localResults {
		seen[r.ID] = true
	}

	results := localResults
	queryLower := strings.ToLower(query)

	for _, chat := range globalChats {
		if len(results) >= limit {
			break
		}
		if !seen[chat.ID] {
			seen[chat.ID] = true
			distance := fuzzy.LevenshteinDistance(queryLower, strings.ToLower(chat.Name))
			results = append(results, SearchResult{
				ChatInfo: chat,
				Score:    distance,
			})
		}
	}

	return results
}
