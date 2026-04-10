package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageReadHandler handles the MarkAsRead tool.
type MessageReadHandler struct {
	client *tg.Client
}

// NewMessageReadHandler creates a new MessageReadHandler.
func NewMessageReadHandler(client *tg.Client) *MessageReadHandler {
	return &MessageReadHandler{client: client}
}

// MarkAsReadInput is the input for the MarkAsRead tool.
type MarkAsReadInput struct {
	ChatIDs []int64 `json:"chat_ids" jsonschema:"List of chat IDs to mark as read (max 100)"`
}

// MarkAsReadFailure describes a single chat that failed to be marked as read.
type MarkAsReadFailure struct {
	ChatID int64  `json:"chat_id"`
	Error  string `json:"error"`
}

// MarkAsReadResult is the typed output of MarkAsRead.
type MarkAsReadResult struct {
	Successful int                 `json:"successful"`
	Failed     int                 `json:"failed"`
	SuccessIDs []int64             `json:"success_ids,omitempty"`
	Failures   []MarkAsReadFailure `json:"failures,omitempty"`
	TotalChats int                 `json:"total_chats"`
}

// markReadResult is the internal per-chat outcome before formatting.
type markReadResult struct {
	chatID  int64
	success bool
	err     error
}

// Register adds the tool to the MCP server.
func (h *MessageReadHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "MarkAsRead",
		Description: "Mark all messages in one or more chats as read. Accepts up to 100 chat IDs at once. This cannot be undone.",
		Annotations: &mcp.ToolAnnotations{
			IdempotentHint: true,
			OpenWorldHint:  ptrTrue(),
		},
	}, h.handle)
}

func (h *MessageReadHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in MarkAsReadInput) (*mcp.CallToolResult, *MarkAsReadResult, error) {
	if len(in.ChatIDs) == 0 {
		return errResult("chat_ids is required and must not be empty. Use GetChats to discover chat IDs first."), nil, nil
	}
	if len(in.ChatIDs) > 100 {
		return errResult("Cannot process more than 100 chats at once. Split the request into batches."), nil, nil
	}

	results := make([]markReadResult, 0, len(in.ChatIDs))

	for _, chatID := range in.ChatIDs {
		err := h.markChatAsRead(ctx, chatID)
		results = append(results, markReadResult{
			chatID:  chatID,
			success: err == nil,
			err:     err,
		})
		// Continue even on error — partial failures are surfaced via the result
		// payload AND via a structured Warning log so MCP clients with a log
		// panel can flag them.
		if err != nil {
			mcpLog(ctx, req.Session, logLevelWarning, "MarkAsRead", map[string]any{
				"chat_id": chatID,
				"error":   err.Error(),
			})
		}
	}

	out := &MarkAsReadResult{TotalChats: len(results)}
	for _, r := range results {
		if r.success {
			out.Successful++
			out.SuccessIDs = append(out.SuccessIDs, r.chatID)
		} else {
			out.Failed++
			out.Failures = append(out.Failures, MarkAsReadFailure{
				ChatID: r.chatID,
				Error:  fmt.Sprintf("%v", r.err),
			})
		}
	}
	return nil, out, nil
}

// markChatAsRead marks a single chat as read.
func (h *MessageReadHandler) markChatAsRead(ctx context.Context, chatID int64) error {
	peer, err := tgclient.ResolvePeer(ctx, h.client, chatID)
	if err != nil {
		return fmt.Errorf("failed to resolve peer: %w", err)
	}

	switch p := peer.(type) {
	case *tg.InputPeerChannel:
		_, err = h.client.ChannelsReadHistory(ctx, &tg.ChannelsReadHistoryRequest{
			Channel: &tg.InputChannel{
				ChannelID:  p.ChannelID,
				AccessHash: p.AccessHash,
			},
		})
		if err != nil {
			return fmt.Errorf("failed to mark channel as read: %w", err)
		}
	default:
		_, err = h.client.MessagesReadHistory(ctx, &tg.MessagesReadHistoryRequest{
			Peer: peer,
		})
		if err != nil {
			return fmt.Errorf("failed to mark chat as read: %w", err)
		}
	}

	return nil
}
