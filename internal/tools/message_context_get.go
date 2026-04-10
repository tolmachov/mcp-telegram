package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
)

const (
	defaultContextWindow = 5
	maxContextWindow     = 50
)

// MessageContextGetHandler handles the GetMessageContext tool.
type MessageContextGetHandler struct {
	provider *messages.Provider
}

// NewMessageContextGetHandler creates a new MessageContextGetHandler.
func NewMessageContextGetHandler(provider *messages.Provider) *MessageContextGetHandler {
	return &MessageContextGetHandler{provider: provider}
}

// GetMessageContextInput is the input for the GetMessageContext tool.
type GetMessageContextInput struct {
	ChatID    int64  `json:"chat_id" jsonschema:"The chat ID containing the anchor message."`
	MessageID string `json:"message_id" jsonschema:"Opaque handle of the anchor message (regular handle only — scheduled handles are rejected). Get this from GetMessages or ResolveMessageLink."`
	Before    int    `json:"before,omitempty" jsonschema:"Number of messages before the anchor to include (default 5\\, max 50)."`
	After     int    `json:"after,omitempty" jsonschema:"Number of messages after the anchor to include (default 5\\, max 50)."`
}

type getMessageContextOutput struct {
	ChatID   int64        `json:"chat_id"`
	AnchorID string       `json:"anchor_id"`
	Messages []messageDTO `json:"messages"`
	Count    int          `json:"count"`
}

// Register adds the tool to the MCP server.
func (h *MessageContextGetHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "GetMessageContext",
		Description: "Get a window of messages around a specific anchor message: `before` messages before it, the anchor itself, and `after` messages after it, returned in chronological order. Defaults to 5+5 (max 50 each). Useful for understanding a message in conversation context. For plain pagination, use GetMessages.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *MessageContextGetHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in GetMessageContextInput) (*mcp.CallToolResult, *getMessageContextOutput, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	if in.MessageID == "" {
		return errResult("message_id is required. Pass an opaque handle returned by GetMessages or ResolveMessageLink."), nil, nil
	}
	ref, err := ParseMessageRef(in.MessageID)
	if err != nil {
		return errInvalidMessageID(in.MessageID, err), nil, nil
	}
	if ref.Scheduled {
		return errCannotOnScheduled("get context around"), nil, nil
	}

	before := clampWindow(in.Before)
	after := clampWindow(in.After)

	result, err := h.provider.FetchContext(ctx, in.ChatID, ref.ID, before, after)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to get message context: %v", err)), nil, nil
	}

	out := &getMessageContextOutput{
		ChatID:   in.ChatID,
		AnchorID: FormatRegularRef(ref.ID),
		Messages: make([]messageDTO, 0, len(result.Messages)),
	}
	for _, m := range result.Messages {
		out.Messages = append(out.Messages, toMessageDTO(m, false))
	}
	out.Count = len(out.Messages)
	return nil, out, nil
}

// clampWindow normalises a before/after parameter: negative → 0, unset → default, > max → max.
func clampWindow(n int) int {
	switch {
	case n < 0:
		return 0
	case n == 0:
		return defaultContextWindow
	case n > maxContextWindow:
		return maxContextWindow
	default:
		return n
	}
}
