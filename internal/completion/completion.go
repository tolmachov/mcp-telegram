// Package completion implements the MCP completion/complete handler.
//
// MCP completion supplies argument suggestions for prompts (ref/prompt) and
// resource-template variables (ref/resource) as the user types. This server
// backs those suggestions with the user's Telegram chats:
//   - prompt argument "chat" (chat-catchup, find-and-reply) → chat titles / @usernames
//   - resource variable "chat_id" (telegram://chat/{chat_id}/info) → numeric chat IDs
//   - prompt argument "period" (daily-digest, chat-catchup) → day / week / month
package completion

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

const (
	// maxCompletionValues caps the suggestion list; the MCP spec recommends
	// returning at most 100 completion values per request.
	maxCompletionValues = 100
	// chatCacheTTL bounds how often the chat list is refetched. Completion
	// fires on every keystroke, so without a cache each character would hit
	// the Telegram API and the shared rate limiter.
	chatCacheTTL = 30 * time.Second
)

// periodValues are the accepted values for the "period" argument across prompts.
var periodValues = []string{"day", "week", "month"}

// chatLister returns the user's chats. It is abstracted so tests can exercise
// the completer without a live Telegram client.
type chatLister func(ctx context.Context) ([]tgdata.ChatInfo, error)

type completer struct {
	list chatLister

	mu       sync.Mutex
	cache    []tgdata.ChatInfo
	cachedAt time.Time
	now      func() time.Time
}

// Handler returns an MCP CompletionHandler that suggests values for prompt
// arguments and resource-template variables backed by the user's Telegram chats.
func Handler(client *tg.Client) func(context.Context, *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	c := &completer{
		list: func(ctx context.Context) ([]tgdata.ChatInfo, error) {
			chats, err := tgdata.GetChats(ctx, client, nil)
			if err != nil {
				return nil, err
			}
			return chats.Chats, nil
		},
		now: time.Now,
	}
	return c.handle
}

func (c *completer) handle(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	if req == nil || req.Params == nil {
		return emptyResult(), nil
	}
	value := req.Params.Argument.Value
	switch req.Params.Argument.Name {
	case "period":
		return result(filterPrefix(periodValues, value)), nil
	case "chat":
		return result(c.completeChats(ctx, value, false)), nil
	case "chat_id":
		return result(c.completeChats(ctx, value, true)), nil
	default:
		return emptyResult(), nil
	}
}

// completeChats returns matching chat suggestions. When asID is true the
// returned values are numeric chat IDs (for the telegram://chat/{chat_id}/info
// resource template); otherwise they are @usernames or titles (for prompt
// arguments that accept "Chat ID, @username, or chat title").
//
// On any Telegram error it returns an empty list rather than failing the RPC —
// a broken completion should never surface as a hard error to the client.
func (c *completer) completeChats(ctx context.Context, value string, asID bool) []string {
	chats, err := c.chats(ctx)
	if err != nil || len(chats) == 0 {
		return nil
	}

	type candidate struct {
		label string // fuzzy-matched against the query (name + @username + id)
		val   string // value handed back to the client
	}
	cands := make([]candidate, 0, len(chats))
	seen := make(map[string]struct{}, len(chats))
	for _, ch := range chats {
		var val string
		switch {
		case asID:
			val = strconv.FormatInt(ch.ID, 10)
		case ch.Username != "":
			val = "@" + ch.Username
		default:
			val = ch.Name
		}
		if val == "" {
			continue
		}
		if _, dup := seen[val]; dup {
			continue
		}
		seen[val] = struct{}{}

		// The label folds in name, username and id so the query matches
		// however the user references a chat (by title, handle, or raw id).
		label := ch.Name
		if ch.Username != "" {
			label += " @" + ch.Username
		}
		label += " " + strconv.FormatInt(ch.ID, 10)
		cands = append(cands, candidate{label: label, val: val})
	}

	query := strings.TrimSpace(value)
	if query == "" {
		// No query yet: offer the first chats (GetChats returns pinned and
		// recently-active chats first), capped to the limit.
		vals := make([]string, 0, min(len(cands), maxCompletionValues))
		for _, cand := range cands {
			vals = append(vals, cand.val)
			if len(vals) >= maxCompletionValues {
				break
			}
		}
		return vals
	}

	labels := make([]string, len(cands))
	for i, cand := range cands {
		labels[i] = cand.label
	}
	ranks := fuzzy.RankFindNormalizedFold(query, labels)
	sort.Sort(ranks)

	vals := make([]string, 0, min(len(ranks), maxCompletionValues))
	for _, r := range ranks {
		vals = append(vals, cands[r.OriginalIndex].val)
		if len(vals) >= maxCompletionValues {
			break
		}
	}
	return vals
}

// chats returns the cached chat list, refetching when the cache is empty or
// stale. The lock is held across the fetch so concurrent completion requests
// share a single in-flight load instead of stampeding the Telegram API.
func (c *completer) chats(ctx context.Context) ([]tgdata.ChatInfo, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cache != nil && c.now().Sub(c.cachedAt) < chatCacheTTL {
		return c.cache, nil
	}
	chats, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	c.cache = chats
	c.cachedAt = c.now()
	return chats, nil
}

// filterPrefix returns the values whose lowercase form starts with value.
func filterPrefix(values []string, value string) []string {
	q := strings.ToLower(strings.TrimSpace(value))
	if q == "" {
		return values
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if strings.HasPrefix(v, q) {
			out = append(out, v)
		}
	}
	return out
}

func result(values []string) *mcp.CompleteResult {
	if values == nil {
		values = []string{}
	}
	return &mcp.CompleteResult{
		Completion: mcp.CompletionResultDetails{
			Values: values,
			Total:  len(values),
		},
	}
}

func emptyResult() *mcp.CompleteResult {
	return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: []string{}}}
}
