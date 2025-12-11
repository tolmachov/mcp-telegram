package tools

import (
	"context"
	"fmt"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/mark3labs/mcp-go/mcp"
)

// UsernameResolveHandler handles the ResolveUsername tool
type UsernameResolveHandler struct {
	client *tg.Client
}

// NewUsernameResolveHandler creates a new UsernameResolveHandler
func NewUsernameResolveHandler(client *tg.Client) *UsernameResolveHandler {
	return &UsernameResolveHandler{client: client}
}

// Tool returns the MCP tool definition
func (h *UsernameResolveHandler) Tool() mcp.Tool {
	return mcp.NewTool("ResolveUsername",
		mcp.WithDescription("Resolve a Telegram username to get user/chat/channel ID and information."),
		mcp.WithString("username",
			mcp.Description("The username to resolve (with or without @ prefix)"),
			mcp.Required(),
		),
	)
}

// Handle processes the ResolveUsername tool request
func (h *UsernameResolveHandler) Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	username := mcp.ParseString(request, "username", "")
	if username == "" {
		return mcp.NewToolResultError("username is required"), nil
	}

	// Remove @ prefix if present
	username = strings.TrimPrefix(username, "@")

	// Resolve username
	resolved, err := h.client.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return mcp.NewToolResultError(fmt.Sprintf("Failed to resolve username @%s: %v", username, err)), nil
	}

	var results []string

	// Check users
	for _, user := range resolved.Users {
		if u, ok := user.(*tg.User); ok {
			result := fmt.Sprintf("type=user id=%d", u.ID)
			if u.Username != "" {
				result += fmt.Sprintf(" username=@%s", u.Username)
			}
			name := u.FirstName
			if u.LastName != "" {
				name += " " + u.LastName
			}
			if name != "" {
				result += fmt.Sprintf(" name='%s'", name)
			}
			if u.Bot {
				result += " bot=true"
			}
			if u.Verified {
				result += " verified=true"
			}
			if u.Premium {
				result += " premium=true"
			}
			results = append(results, result)
		}
	}

	// Check chats
	for _, chat := range resolved.Chats {
		switch c := chat.(type) {
		case *tg.Chat:
			result := fmt.Sprintf("type=chat id=%d title='%s' participants=%d",
				c.ID, c.Title, c.ParticipantsCount)
			results = append(results, result)
		case *tg.Channel:
			channelType := "channel"
			if c.Megagroup {
				channelType = "supergroup"
			}
			result := fmt.Sprintf("type=%s id=%d title='%s'",
				channelType, c.ID, c.Title)
			if c.Username != "" {
				result += fmt.Sprintf(" username=@%s", c.Username)
			}
			if c.ParticipantsCount > 0 {
				result += fmt.Sprintf(" participants=%d", c.ParticipantsCount)
			}
			if c.Verified {
				result += " verified=true"
			}
			results = append(results, result)
		}
	}

	if len(results) == 0 {
		return mcp.NewToolResultError(fmt.Sprintf("Username @%s not found", username)), nil
	}

	return mcp.NewToolResultText(strings.Join(results, "\n")), nil
}
