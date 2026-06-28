package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// UsernameResolveHandler handles the ResolveUsername tool.
type UsernameResolveHandler struct {
	client *tg.Client
}

// NewUsernameResolveHandler creates a new UsernameResolveHandler.
func NewUsernameResolveHandler(client *tg.Client) *UsernameResolveHandler {
	return &UsernameResolveHandler{client: client}
}

// ResolveUsernameInput is the input for the ResolveUsername tool.
type ResolveUsernameInput struct {
	Username string `json:"username" jsonschema:"The username to resolve (with or without @ prefix)"`
}

// ResolveUsernameEntity is a single resolved entity (user, bot, chat,
// channel, or supergroup). Kind is always set; other fields are
// populated when available from the Telegram API response.
type ResolveUsernameEntity struct {
	Kind              string `json:"kind"` // "user" | "bot" | "chat" | "channel" | "supergroup"
	ID                int64  `json:"id"`
	Username          string `json:"username,omitempty"`
	Title             string `json:"title,omitempty"`
	FirstName         string `json:"first_name,omitempty"`
	LastName          string `json:"last_name,omitempty"`
	ParticipantsCount int    `json:"participants_count,omitempty"`
	Verified          bool   `json:"verified,omitempty"`
	Premium           bool   `json:"premium,omitempty"`
}

// ResolveUsernameResult is the typed output of ResolveUsername. Returns
// a list because a single @username can resolve to multiple entities in
// Telegram's response (e.g., a bot inside a chat), and the model may
// need to pick the right one.
type ResolveUsernameResult struct {
	Query    string                  `json:"query"`
	Entities []ResolveUsernameEntity `json:"entities"`
}

// Register adds the tool to the MCP server.
func (h *UsernameResolveHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ResolveUsername",
		Description: "Resolve a Telegram username to get user/chat/channel ID and information. Use this when you have a @username but need a numeric chat ID for other tools. Returns structured entities with kind, id, and metadata.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *UsernameResolveHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in ResolveUsernameInput) (*mcp.CallToolResult, *ResolveUsernameResult, error) {
	if strings.TrimSpace(in.Username) == "" {
		return errResult("username is required (e.g. '@durov' or 'durov'). For chats without a public @username, use SearchChats by title instead."), nil, nil
	}

	username := strings.TrimPrefix(strings.TrimSpace(in.Username), "@")

	resolved, err := h.client.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return errResult(fmt.Sprintf("Failed to resolve username @%s: %v", username, err)), nil, nil
	}

	out := &ResolveUsernameResult{
		Query:    "@" + username,
		Entities: make([]ResolveUsernameEntity, 0, len(resolved.Users)+len(resolved.Chats)),
	}

	for _, user := range resolved.Users {
		u, ok := user.(*tg.User)
		if !ok {
			continue
		}
		e := ResolveUsernameEntity{
			Kind:      "user",
			ID:        u.ID,
			Username:  u.Username,
			FirstName: u.FirstName,
			LastName:  u.LastName,
			Verified:  u.Verified,
			Premium:   u.Premium,
		}
		if u.Bot {
			e.Kind = "bot"
		}
		out.Entities = append(out.Entities, e)
	}

	for _, chat := range resolved.Chats {
		switch c := chat.(type) {
		case *tg.Chat:
			out.Entities = append(out.Entities, ResolveUsernameEntity{
				Kind:              "chat",
				ID:                c.ID,
				Title:             c.Title,
				ParticipantsCount: c.ParticipantsCount,
			})
		case *tg.Channel:
			kind := "channel"
			if c.Megagroup {
				kind = "supergroup"
			}
			out.Entities = append(out.Entities, ResolveUsernameEntity{
				Kind:              kind,
				ID:                c.ID,
				Username:          c.Username,
				Title:             c.Title,
				ParticipantsCount: c.ParticipantsCount,
				Verified:          c.Verified,
			})
		}
	}

	if len(out.Entities) == 0 {
		return errResult(fmt.Sprintf("Username @%s not found. The user/chat may not exist, may be private, or may not have a public @username. Try SearchChats with a partial title instead.", username)), nil, nil
	}

	return nil, out, nil
}
