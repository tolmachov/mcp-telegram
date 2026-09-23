package tools

import (
	"context"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/presentation"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageEditHandler handles the EditMessage tool.
type MessageEditHandler struct {
	client *tg.Client
}

// NewMessageEditHandler creates a new MessageEditHandler.
func NewMessageEditHandler(client *tg.Client) *MessageEditHandler {
	return &MessageEditHandler{client: client}
}

// EditMessageInput is the input for the EditMessage tool.
//
// MessageID is an opaque handle. When it refers to a scheduled message
// ("s:42"), ScheduleAt is required — Telegram's messages.editMessage
// requires schedule_date to be set for scheduled edits, and the new
// value fully replaces the old schedule (so edits can also reschedule
// in one call). For regular messages, ScheduleAt must be empty.
type EditMessageInput struct {
	ChatID     int64  `json:"chat_id" jsonschema:"The ID of the chat containing the message"`
	MessageID  string `json:"message_id" jsonschema:"Opaque message handle from GetMessages or SendMessage (e.g. \"42\" or \"s:42\" for scheduled)"`
	NewText    string `json:"new_text" jsonschema:"The new text for the message"`
	ScheduleAt string `json:"schedule_at,omitempty" jsonschema:"New send time in RFC3339 (e.g. 2026-04-10T15:30:00Z). Required when message_id is a scheduled handle; must be omitted for regular messages. Setting this both edits the text and reschedules the pending delivery."`
}

const statusEdited = "edited"

const (
	kindRegular   = "regular"
	kindScheduled = "scheduled"
)

// EditMessageResult is the typed output of EditMessage.
type EditMessageResult struct {
	Status     string `json:"status"`
	Kind       string `json:"kind"` // "regular" | "scheduled"
	ChatID     int64  `json:"chat_id"`
	MessageID  string `json:"message_id"`
	NewText    string `json:"new_text"`
	EditedAt   string `json:"edited_at,omitempty"`
	ScheduleAt string `json:"schedule_at,omitempty"`
	Note       string `json:"note,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *MessageEditHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "EditMessage",
		Description: "Edit a message you previously sent. Only your own messages can be edited. For channel posts, admin rights may be required. To edit a scheduled (pending) message, pass its \"s:...\" handle and provide a new schedule_at — the edit both rewrites the text and sets the new send time.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *MessageEditHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in EditMessageInput) (*mcp.CallToolResult, *EditMessageResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	ref, err := presentation.ParseMessageRef(in.MessageID)
	if err != nil {
		return errInvalidMessageID("message_id", in.MessageID, err), nil, nil
	}
	if in.NewText == "" {
		return errResult("new_text is required"), nil, nil
	}
	if errRes := errMessageTooLong("new_text", in.NewText); errRes != nil {
		return errRes, nil, nil
	}

	editReq := &tg.MessagesEditMessageRequest{
		ID:      ref.ID,
		Message: in.NewText,
	}

	// Validate schedule_at against the handle kind. Telegram's editMessage
	// only accepts ScheduleDate when the target IS a scheduled message; for
	// regular messages we reject the field to avoid silent no-ops.
	if ref.Scheduled {
		if in.ScheduleAt == "" {
			return errResult("schedule_at is required when editing a scheduled message (\"s:...\"). Provide an RFC3339 timestamp in the future — it sets the new delivery time and fully replaces the old schedule."), nil, nil
		}
		t, errRes := parseFutureSchedule(in.ScheduleAt)
		if errRes != nil {
			return errRes, nil, nil
		}
		editReq.ScheduleDate = int(t.Unix())
	} else if in.ScheduleAt != "" {
		return errResult("schedule_at is only valid when editing a scheduled message (\"s:...\"). To reschedule a pending delivery, pass the scheduled handle instead."), nil, nil
	}

	peer, err := tgclient.ResolvePeer(ctx, h.client, in.ChatID)
	if err != nil {
		return nil, nil, failed("edit message", err)
	}
	editReq.Peer = peer

	updates, err := h.client.MessagesEditMessage(ctx, editReq)
	if err != nil {
		return nil, nil, failed("edit message", err)
	}

	editedMsgID, date := extractEditedMessageID(updates)
	if editedMsgID == 0 {
		mcpLog(ctx, req.Session, logLevelWarning, "EditMessage", map[string]any{
			"action":  "edited_message_id_extraction_failed",
			"chat_id": in.ChatID,
			"note":    "Telegram returned an unrecognised update type; falling back to input handle",
		})
	}

	res := &EditMessageResult{
		Status:  statusEdited,
		ChatID:  in.ChatID,
		NewText: in.NewText,
	}
	if ref.Scheduled {
		res.Kind = kindScheduled
		res.ScheduleAt = in.ScheduleAt
		if editedMsgID > 0 {
			res.MessageID = presentation.FormatScheduledRef(editedMsgID)
		} else {
			// Telegram did not return the updated handle — preserve the input.
			res.MessageID = ref.Format()
			res.Note = "message_id may be stale: Telegram returned an unrecognised update type. Verify via GetMessages with include_scheduled=true."
		}
	} else {
		res.Kind = kindRegular
		if editedMsgID > 0 {
			res.MessageID = presentation.FormatRegularRef(editedMsgID)
		} else {
			res.MessageID = ref.Format()
			res.Note = "message_id may be stale: Telegram returned an unrecognised update type. Verify via GetMessages."
		}
	}
	if date > 0 {
		res.EditedAt = formatUnixRFC3339(date)
	}
	return nil, res, nil
}

// extractEditedMessageID pulls the edited message ID + date out of an
// UpdatesClass returned by messages.editMessage. Covers both
// UpdateEditMessage (for regular chats and scheduled messages) and
// UpdateEditChannelMessage (for channels/supergroups).
func extractEditedMessageID(updates tg.UpdatesClass) (int, int) {
	return firstMessageInUpdates(updates, tg.UpdateEditMessageTypeID, tg.UpdateEditChannelMessageTypeID)
}
