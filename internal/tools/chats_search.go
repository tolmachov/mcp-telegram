package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsSearchHandler handles the SearchChats tool.
type ChatsSearchHandler struct {
	client *tg.Client
}

// NewChatsSearchHandler creates a new ChatsSearchHandler.
func NewChatsSearchHandler(client *tg.Client) *ChatsSearchHandler {
	return &ChatsSearchHandler{client: client}
}

// SearchChatsInput is the input for the SearchChats tool.
type SearchChatsInput struct {
	Query string `json:"query" jsonschema:"Search query to match against chat names"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum number of results to return (default: 10\\, max: 50)"`
}

// SearchResult represents a single search result with a match score.
type SearchResult struct {
	tgdata.ChatInfo
	Score int `json:"score"` // Lower is a better match (Levenshtein distance)
}

// SearchResultsList represents the search results.
type SearchResultsList struct {
	Query   string         `json:"query"`
	Results []SearchResult `json:"results"`
	Count   int            `json:"count"`
	Warning string         `json:"warning,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *ChatsSearchHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "SearchChats",
		Description: "Search for chats, groups, and channels by name using fuzzy matching. Searches local chats first, then globally. Returns up to `limit` results (default 10, max 50). Preferred over GetChats when looking for a specific chat.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *ChatsSearchHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SearchChatsInput) (*mcp.CallToolResult, *SearchResultsList, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return errResult("query parameter is required (search by chat title or @username; partial matches work)."), nil, nil
	}

	limit := clampLimit(in.Limit, 10, 50)

	// Get all user's chats for local fuzzy search first.
	onProgress := func(current int, message string) {
		sendProgress(ctx, req, float64(current), 0, message)
	}
	chatsList, err := tgdata.GetChats(ctx, h.client, onProgress)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to get chats: %v", err)), nil, nil
	}

	// Perform local fuzzy search first.
	results := h.fuzzySearchLocal(query, chatsList.Chats, limit)

	// Only search globally if we have room for more results.
	var globalSearchWarning string
	if len(results) < limit {
		globalResults, err := h.searchGlobal(ctx, query)
		if err != nil {
			mcpLog(ctx, req.Session, logLevelWarning, "SearchChats", map[string]any{
				"action": "global_search_failed",
				"query":  query,
				"error":  err.Error(),
			})
			globalSearchWarning = "Global search failed; results may be incomplete (local matches only)."
		} else if len(globalResults) > 0 {
			results = h.addGlobalResults(query, results, globalResults, limit)
		}
	}

	return nil, &SearchResultsList{
		Query:   query,
		Results: results,
		Count:   len(results),
		Warning: globalSearchWarning,
	}, nil
}

// searchGlobal performs Telegram's global search by username.
func (h *ChatsSearchHandler) searchGlobal(ctx context.Context, query string) ([]tgdata.ChatInfo, error) {
	found, err := h.client.ContactsSearch(ctx, &tg.ContactsSearchRequest{
		Q:     query,
		Limit: 20,
	})
	if err != nil {
		return nil, fmt.Errorf("searching contacts: %w", err)
	}

	var results []tgdata.ChatInfo

	// Process users; bots are intentionally excluded (not meaningful chat targets).
	for _, user := range found.Users {
		u, ok := user.(*tg.User)
		if !ok || u.Bot {
			continue
		}

		results = append(results, tgdata.ChatInfo{
			ID:       u.ID,
			Type:     tgdata.ChatTypeUser,
			Name:     tgclient.UserName(u),
			Username: u.Username,
		})
	}

	// Process chats.
	for _, chat := range found.Chats {
		switch c := chat.(type) {
		case *tg.Chat:
			results = append(results, tgdata.ChatInfo{
				ID:   c.ID,
				Type: tgdata.ChatTypeGroup,
				Name: c.Title,
			})
		case *tg.Channel:
			chatType := tgdata.ChatTypeChannel
			if c.Megagroup {
				chatType = tgdata.ChatTypeSupergroup
			}
			results = append(results, tgdata.ChatInfo{
				ID:       c.ID,
				Type:     chatType,
				Name:     c.Title,
				Username: c.Username,
			})
		}
	}

	return results, nil
}

// fuzzySearchLocal performs fuzzy search on local chats only.
func (h *ChatsSearchHandler) fuzzySearchLocal(query string, chats []tgdata.ChatInfo, limit int) []SearchResult {
	names := make([]string, len(chats))
	for i, chat := range chats {
		names[i] = chat.Name
	}

	matches := fuzzy.RankFindNormalizedFold(query, names)

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

// addGlobalResults adds global search results to fill remaining slots.
func (h *ChatsSearchHandler) addGlobalResults(query string, localResults []SearchResult, globalChats []tgdata.ChatInfo, limit int) []SearchResult {
	if len(localResults) >= limit {
		return localResults
	}

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
