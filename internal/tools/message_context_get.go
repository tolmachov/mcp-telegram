package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/messages"
	"github.com/tolmachov/mcp-telegram/internal/presentation"
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
	ChatID   int64                  `json:"chat_id"`
	AnchorID string                 `json:"anchor_id"`
	Messages []presentation.Message `json:"messages"`
	Count    int                    `json:"count"`
}

// Register adds the tool to the MCP server.
func (h *MessageContextGetHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "GetMessageContext",
		Description: "Get a window of messages around a specific anchor message: `before` messages before it, the anchor itself, and `after` messages after it, returned in chronological order. Defaults to 5+5 (max 50 each). Useful for understanding a message in conversation context. For plain pagination, use GetMessages.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *MessageContextGetHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in GetMessageContextInput) (*mcp.CallToolResult, *getMessageContextOutput, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	if in.MessageID == "" {
		return errResult("message_id is required. Pass an opaque handle returned by GetMessages or ResolveMessageLink."), nil, nil
	}
	msgID, errRes := parseRegularRef("message_id", in.MessageID, "get context around")
	if errRes != nil {
		return errRes, nil, nil
	}

	before := clampWindow(in.Before)
	after := clampWindow(in.After)

	result, err := h.provider.FetchContext(ctx, in.ChatID, msgID, before, after)
	if err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "GetMessageContext", map[string]any{
			"action":  "provider_fetch_failed",
			"chat_id": in.ChatID,
			"msg_id":  msgID,
			"error":   err.Error(),
		})
		return errResult(fmt.Sprintf("Failed to get message context: %v", err)), nil, nil
	}

	out := &getMessageContextOutput{
		ChatID:   in.ChatID,
		AnchorID: presentation.FormatRegularRef(msgID),
		Messages: make([]presentation.Message, 0, len(result.Messages)),
	}
	for _, m := range result.Messages {
		out.Messages = append(out.Messages, presentation.FromMessage(m, false))
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
