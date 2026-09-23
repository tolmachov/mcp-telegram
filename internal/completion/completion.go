// Package completion implements the MCP completion/complete handler.
//
// MCP completion supplies argument suggestions for prompts (ref/prompt) and
// resource-template variables (ref/resource) as the user types. This server
// backs those suggestions with the user's Telegram chats:
//   - prompt argument "chat" (chat-catchup, find-and-reply) → chat titles / @usernames
//   - resource variable "chat_id" (telegram://chats/{chat_id}/info) → numeric chat IDs
//   - prompt argument "period" (daily-digest, chat-catchup) → day / week / month
package completion

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/lithammer/fuzzysearch/fuzzy"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/summarize"
	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// maxCompletionValues caps the suggestion list; the MCP spec recommends
// returning at most 100 completion values per request.
const maxCompletionValues = 100

// snapshotLoader returns the current chat snapshot. It is abstracted so tests
// can exercise the completer without a live Telegram client.
type snapshotLoader func(ctx context.Context) (*tgdata.ChatsSnapshot, error)

type completer struct {
	load snapshotLoader
	// cands holds the candidates derived from the latest snapshot seen, so
	// they are built once per snapshot rather than on every keystroke.
	cands atomic.Pointer[candidateSet]
}

// candidateList is a deduplicated suggestion list. vals[i] is handed back to
// the client; labels[i] is what the query is fuzzy-matched against.
type candidateList struct {
	vals   []string
	labels []string
}

// candidateSet is the candidates derived from one chat snapshot.
type candidateSet struct {
	snapshotID int64
	byName     candidateList // @usernames or titles, for prompt arguments
	byID       candidateList // numeric chat IDs, for the chat resource template
}

// Handler returns an MCP CompletionHandler that suggests values for prompt
// arguments and resource-template variables backed by the shared chat cache.
// Completion fires on every keystroke; the cache keeps that from hitting the
// Telegram API each time.
func Handler(chats *tgdata.ChatsCache) func(context.Context, *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
	c := &completer{
		load: func(ctx context.Context) (*tgdata.ChatsSnapshot, error) {
			return chats.Load(ctx, nil, false)
		},
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
		return result(filterPrefix(summarize.PeriodNames(), value)), nil
	case "chat":
		return result(c.completeChats(ctx, value, false)), nil
	case "chat_id":
		return result(c.completeChats(ctx, value, true)), nil
	default:
		return emptyResult(), nil
	}
}

// completeChats returns matching chat suggestions. When asID is true the
// returned values are numeric chat IDs (for the telegram://chats/{chat_id}/info
// resource template); otherwise they are @usernames or titles (for prompt
// arguments that accept "Chat ID, @username, or chat title").
//
// On any Telegram error it returns an empty list rather than failing the RPC —
// a broken completion should never surface as a hard error to the client.
func (c *completer) completeChats(ctx context.Context, value string, asID bool) []string {
	set, err := c.candidates(ctx)
	if err != nil {
		return nil
	}
	list := set.byName
	if asID {
		list = set.byID
	}

	query := strings.TrimSpace(value)
	if query == "" {
		// No query yet: offer the first chats (GetChats returns pinned and
		// recently-active chats first), capped to the limit.
		return list.vals[:min(len(list.vals), maxCompletionValues)]
	}

	ranks := fuzzy.RankFindNormalizedFold(query, list.labels)
	sort.Sort(ranks)

	vals := make([]string, 0, min(len(ranks), maxCompletionValues))
	for _, r := range ranks[:min(len(ranks), maxCompletionValues)] {
		vals = append(vals, list.vals[r.OriginalIndex])
	}
	return vals
}

// candidates returns the candidates for the current snapshot, rebuilding them
// only when the snapshot has changed. Concurrent rebuilds for the same
// snapshot produce identical sets, so the last store winning is harmless.
func (c *completer) candidates(ctx context.Context) (*candidateSet, error) {
	snap, err := c.load(ctx)
	if err != nil {
		return nil, err
	}
	if set := c.cands.Load(); set != nil && set.snapshotID == snap.ID {
		return set, nil
	}
	set := &candidateSet{
		snapshotID: snap.ID,
		byName:     buildCandidates(snap.Chats, false),
		byID:       buildCandidates(snap.Chats, true),
	}
	c.cands.Store(set)
	return set, nil
}

// buildCandidates derives one deduplicated suggestion list from chats.
func buildCandidates(chats []tgdata.ChatInfo, asID bool) candidateList {
	list := candidateList{
		vals:   make([]string, 0, len(chats)),
		labels: make([]string, 0, len(chats)),
	}
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
		list.vals = append(list.vals, val)
		list.labels = append(list.labels, label)
	}
	return list
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
