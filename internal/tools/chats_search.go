package tools

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gotd/td/tg"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// ChatsSearchHandler handles the SearchChats tool.
type ChatsSearchHandler struct {
	client *tg.Client
	cache  *ChatsCache
}

// NewChatsSearchHandler creates a new ChatsSearchHandler. It shares the chat
// snapshot held by cache with GetChats, so a local search reuses an already
// loaded listing instead of re-paginating every dialog.
func NewChatsSearchHandler(client *tg.Client, cache *ChatsCache) *ChatsSearchHandler {
	return &ChatsSearchHandler{client: client, cache: cache}
}

// SearchChatsInput is the input for the SearchChats tool.
type SearchChatsInput struct {
	Query string `json:"query" jsonschema:"Search query to match against chat names"`
	Limit int    `json:"limit,omitempty" jsonschema:"Maximum number of results to return (default: 10\\, max: 50)"`
}

// SearchResult represents a single search result with a match distance.
// Results are already sorted best-first; Distance is exposed for transparency.
type SearchResult struct {
	tgdata.ChatInfo
	// Distance is an edit-distance-style score where LOWER means a better
	// match (0 is exact). Named "distance" rather than "score" so the model
	// doesn't assume higher is better and re-rank against the intended order.
	Distance int `json:"distance"`
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
	AddTool(s, &mcp.Tool{
		Name:        "SearchChats",
		Description: "Search for chats, groups, and channels by name using fuzzy matching. Searches local chats first, then globally. Returns up to `limit` results (default 10, max 50). Preferred over GetChats when looking for a specific chat.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *ChatsSearchHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SearchChatsInput) (*mcp.CallToolResult, *SearchResultsList, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return errResult("query parameter is required (search by chat title or @username; partial matches work)."), nil, nil
	}

	limit := clampLimit(in.Limit, 10, 50)

	// Get all user's chats for local fuzzy search first. Reuse the shared
	// snapshot (loading it once if cold) rather than re-listing every dialog.
	onProgress := func(current int, message string) {
		sendProgress(ctx, req, float64(current), 0, message)
	}
	chats, _, truncated, err := h.cache.load(ctx, onProgress, false)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to get chats: %v", err)), nil, nil
	}

	results := scoreChats(query, chats)

	var warnings []string
	if truncated {
		warnings = append(warnings, truncatedChatsWarning)
	}
	var globalResults []tgdata.ChatInfo
	var globalErr error
	if h.client != nil {
		globalResults, globalErr = h.searchGlobal(ctx, query)
	}
	if globalErr != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "SearchChats", map[string]any{
			"action": "global_search_failed",
			"query":  query,
			"error":  globalErr.Error(),
		})
		warnings = append(warnings, "Global search failed; results may be incomplete (local matches only).")
	} else if len(globalResults) > 0 {
		results = mergeSearchResults(results, scoreChats(query, globalResults))
	}
	if len(results) > limit {
		results = results[:limit]
	}

	return nil, &SearchResultsList{
		Query:   query,
		Results: results,
		Count:   len(results),
		Warning: strings.Join(warnings, " "),
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

	// Bots are intentionally excluded (not meaningful chat targets).
	for _, user := range found.Users {
		if u, ok := user.(*tg.User); ok && !u.Bot {
			results = append(results, tgdata.ChatInfoFromUser(u))
		}
	}
	for _, chat := range found.Chats {
		if info, ok := tgdata.ChatInfoFromChat(chat); ok {
			results = append(results, info)
		}
	}

	return results, nil
}

func scoreChats(query string, chats []tgdata.ChatInfo) []SearchResult {
	query = strings.ToLower(strings.TrimSpace(query))
	queryNoAt := strings.TrimPrefix(query, "@")
	results := make([]SearchResult, 0, len(chats))
	for _, chat := range chats {
		candidates := []string{strings.ToLower(chat.Name), strings.ToLower(chat.Username), strconv.FormatInt(chat.ID, 10)}
		best := -1
		for _, candidate := range candidates {
			candidate = strings.TrimPrefix(strings.TrimSpace(candidate), "@")
			if candidate == "" || (!strings.Contains(candidate, queryNoAt) && !fuzzy.MatchNormalizedFold(queryNoAt, candidate)) {
				continue
			}
			distance := fuzzy.LevenshteinDistance(queryNoAt, candidate)
			denom := max(utf8.RuneCountInString(queryNoAt), utf8.RuneCountInString(candidate))
			score := 0
			if denom > 0 {
				score = distance * 1000 / denom
			}
			if best < 0 || score < best {
				best = score
			}
		}
		if best >= 0 {
			results = append(results, SearchResult{ChatInfo: chat, Distance: best})
		}
	}
	sortSearchResults(results)
	return results
}

func mergeSearchResults(groups ...[]SearchResult) []SearchResult {
	byID := make(map[int64]SearchResult)
	for _, group := range groups {
		for _, result := range group {
			previous, exists := byID[result.ID]
			if !exists || result.Distance < previous.Distance {
				byID[result.ID] = result
			}
		}
	}
	merged := make([]SearchResult, 0, len(byID))
	for _, result := range byID {
		merged = append(merged, result)
	}
	sortSearchResults(merged)
	return merged
}

func sortSearchResults(results []SearchResult) {
	sort.Slice(results, func(i, j int) bool {
		if results[i].Distance != results[j].Distance {
			return results[i].Distance < results[j].Distance
		}
		if results[i].Name != results[j].Name {
			return results[i].Name < results[j].Name
		}
		return results[i].ID < results[j].ID
	})
}
