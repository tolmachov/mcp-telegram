package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageReactionHandler handles the SetReaction tool.
type MessageReactionHandler struct {
	client *tg.Client
}

// NewSetReactionHandler creates a new MessageReactionHandler.
func NewSetReactionHandler(client *tg.Client) *MessageReactionHandler {
	return &MessageReactionHandler{client: client}
}

// SetReactionInput is the input for the SetReaction tool.
//
// Emojis fully replaces the current set of your reactions on the message:
// a non-empty list sets exactly those emoji reactions, while an empty or
// omitted list removes all of your reactions. Only regular (delivered)
// messages can carry reactions, so scheduled handles ("s:...") are rejected.
type SetReactionInput struct {
	ChatID    int64    `json:"chat_id" jsonschema:"The ID of the chat containing the message"`
	MessageID string   `json:"message_id" jsonschema:"Opaque regular-message handle from GetMessages or SendMessage (e.g. \"42\")"`
	Emojis    []string `json:"emojis,omitempty" jsonschema:"Emoji reactions to set (e.g. [\"👍\"]). Empty or omitted removes all your reactions from the message."`
	Big       bool     `json:"big,omitempty" jsonschema:"Show a bigger and longer reaction animation"`
}

const (
	statusReacted = "reacted"
	statusCleared = "cleared"
)

// SetReactionResult is the typed output of SetReaction.
type SetReactionResult struct {
	Status    string   `json:"status"` // "reacted" | "cleared"
	ChatID    int64    `json:"chat_id"`
	MessageID string   `json:"message_id"`
	Emojis    []string `json:"emojis,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *MessageReactionHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "SetReaction",
		Description: "Set or clear your emoji reactions on a message. Pass one or more emoji in `emojis` to react (this replaces any reactions you previously set); pass an empty list (or omit it) to remove all your reactions. Only standard emoji reactions are supported (not custom or paid). Works on regular messages only — scheduled handles (\"s:...\") are rejected. The chat must permit the chosen reaction, otherwise Telegram returns an error.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *MessageReactionHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SetReactionInput) (*mcp.CallToolResult, *SetReactionResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	ref, err := ParseMessageRef(in.MessageID)
	if err != nil {
		return errInvalidMessageID(in.MessageID, err), nil, nil
	}
	if ref.Scheduled {
		return errCannotOnScheduled("react to"), nil, nil
	}

	peer, err := tgclient.ResolvePeer(ctx, h.client, in.ChatID)
	if err != nil {
		return errResolvePeer(in.ChatID, err), nil, nil
	}

	sendReq := &tg.MessagesSendReactionRequest{
		Peer:  peer,
		MsgID: ref.ID,
		Big:   in.Big,
	}

	// Drop empty entries so a stray "" doesn't turn into an invalid reaction.
	emojis := make([]string, 0, len(in.Emojis))
	for _, e := range in.Emojis {
		if e != "" {
			emojis = append(emojis, e)
		}
	}

	status := statusCleared
	if len(emojis) > 0 {
		reactions := make([]tg.ReactionClass, 0, len(emojis))
		for _, e := range emojis {
			reactions = append(reactions, &tg.ReactionEmoji{Emoticon: e})
		}
		sendReq.SetReaction(reactions)
		sendReq.AddToRecent = true
		status = statusReacted
	}
	// When no reactions are set the Reaction flag stays clear, which tells
	// Telegram to remove all of the current user's reactions from the message.

	if _, err := h.client.MessagesSendReaction(ctx, sendReq); err != nil {
		mcpLog(ctx, req.Session, logLevelWarning, "SetReaction", map[string]any{
			"chat_id":    in.ChatID,
			"message_id": in.MessageID,
			"error":      err.Error(),
		})
		return errResult(fmt.Sprintf("Failed to set reaction: %v", err)), nil, nil
	}

	return nil, &SetReactionResult{
		Status:    status,
		ChatID:    in.ChatID,
		MessageID: in.MessageID,
		Emojis:    emojis,
	}, nil
}
