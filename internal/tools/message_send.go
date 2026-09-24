package tools

import (
	"context"
	"fmt"
	"time"
	"unicode/utf16"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/presentation"
	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// telegramMaxMessageLength is Telegram's per-message text limit, measured in
// UTF-16 code units per the API docs.
const telegramMaxMessageLength = 4096

// errMessageTooLong returns the shared over-limit error for a message text
// field (SendMessage.message, EditMessage.new_text), or nil when text fits.
func errMessageTooLong(field, text string) *mcp.CallToolResult {
	n := 0
	for _, r := range text {
		n += utf16.RuneLen(r)
	}
	if n <= telegramMaxMessageLength {
		return nil
	}
	return errResult(fmt.Sprintf(
		"%s is too long: %d UTF-16 code units (Telegram limit is %d). Split it into multiple messages or shorten the text.",
		field, n, telegramMaxMessageLength,
	))
}

// Mode values accepted by SendMessage. Reply is not a mode — it's an
// orthogonal property that combines with any mode via ReplyToMessageID.
const (
	sendModeDefault  = ""         // treated as sendModeSend
	sendModeSend     = "send"     // deliver immediately (optionally as a reply)
	sendModeSchedule = "schedule" // queue for future delivery via Telegram's schedule
	sendModeDraft    = "draft"    // save as a draft in the Telegram app, do not send
)

// MessageSendHandler handles the SendMessage tool.
type MessageSendHandler struct {
	peers *tgclient.Resolver
}

// NewMessageSendHandler creates a new MessageSendHandler.
func NewMessageSendHandler(peers *tgclient.Resolver) *MessageSendHandler {
	return &MessageSendHandler{peers: peers}
}

// SendMessageInput is the input for the SendMessage tool.
//
// Mode selects the delivery strategy:
//   - "send" (default): deliver immediately.
//   - "schedule": queue for future delivery at ScheduleAt (RFC3339). If
//     the time is under ~10s away, Telegram delivers immediately and the
//     response status is "sent_immediate".
//   - "draft": save as a draft in the Telegram app without sending.
//
// ReplyToMessageID is orthogonal to Mode — setting it turns any mode
// into a threaded reply. Scheduled handles ("s:...") are rejected as
// reply targets because unsent messages cannot be replied to.
type SendMessageInput struct {
	ChatID           int64  `json:"chat_id" jsonschema:"The ID of the chat to send the message to"`
	Message          string `json:"message" jsonschema:"The message text to send"`
	Mode             string `json:"mode,omitempty" jsonschema:"Delivery mode: \"send\" (default\\, deliver immediately)\\, \"schedule\" (queue for future delivery — requires schedule_at)\\, or \"draft\" (save in Telegram app without sending)."`
	ReplyToMessageID string `json:"reply_to_message_id,omitempty" jsonschema:"Optional opaque handle of a message to reply to. Works with any mode. Scheduled handles (\"s:...\") are not valid reply targets."`
	ScheduleAt       string `json:"schedule_at,omitempty" jsonschema:"Future send time in RFC3339 (e.g. 2026-04-10T15:30:00Z). Required when mode=\"schedule\". Times under ~10s from now are delivered immediately by Telegram."`
}

// SendMessageResult is the typed output of SendMessage. The SDK serialises
// this struct as both the text content and the structured output, so MCP
// clients that support structured results can parse message_id reliably for
// follow-up calls (replies, edits, deletes) without scraping free-form text.
//
// Status values:
//   - "sent": regular immediate send
//   - "scheduled": queued for future delivery
//   - "sent_immediate": schedule request collapsed into an immediate send
//     because the requested time was too close (Telegram's ~10s floor)
//   - "drafted": saved as a draft in the app, not sent
//
// MessageID is an opaque handle — "s:<id>" for a queued scheduled message,
// "<id>" for a delivered regular message, or empty for drafts (Telegram
// does not return an ID when saving a draft).
type SendMessageResult struct {
	Status             string `json:"status"`
	ChatID             int64  `json:"chat_id"`
	MessageID          string `json:"message_id,omitempty"`
	Date               string `json:"date,omitempty"`
	ScheduleAt         string `json:"schedule_at,omitempty"`
	RepliedToMessageID string `json:"replied_to_message_id,omitempty"`
	Note               string `json:"note,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *MessageSendHandler) Register(s *mcp.Server) {
	AddTool(s, &mcp.Tool{
		Name:        "SendMessage",
		Description: "Send a message to a chat. Modes (mutually exclusive): \"send\" (default) delivers immediately; \"schedule\" stores on Telegram's servers and delivers at schedule_at (RFC3339, must be in future; delays < ~10s are sent immediately); \"draft\" saves locally in the Telegram app without sending. reply_to_message_id (opaque handle) works with any mode for threaded replies. Returns status: sent | scheduled | sent_immediate | drafted.",
		InputSchema: inputSchemaWithEnums[SendMessageInput](map[string][]string{
			"mode": {"send", "schedule", "draft"},
		}),
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *MessageSendHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in SendMessageInput) (*mcp.CallToolResult, *SendMessageResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	if in.Message == "" {
		return errResult("message is required and must be non-empty"), nil, nil
	}
	if errRes := errMessageTooLong("message", in.Message); errRes != nil {
		return errRes, nil, nil
	}

	// Normalise and validate mode first so the LLM gets a clear error when
	// it misspells the value instead of silently falling back to "send".
	mode := in.Mode
	switch mode {
	case sendModeDefault, sendModeSend, sendModeSchedule, sendModeDraft:
	default:
		return errResult(fmt.Sprintf("invalid mode %q: expected one of \"send\", \"schedule\", \"draft\" (or omit for default send)", in.Mode)), nil, nil
	}

	// Parse reply target once up front — shared by all modes. Reject
	// scheduled handles because unsent messages can't be reply targets.
	var replyToID int
	var replyHandle string
	if in.ReplyToMessageID != "" {
		var errRes *mcp.CallToolResult
		replyToID, errRes = parseRegularRef("reply_to_message_id", in.ReplyToMessageID, "reply to")
		if errRes != nil {
			return errRes, nil, nil
		}
		replyHandle = presentation.FormatRegularRef(replyToID)
	}

	// Schedule-specific validation: ScheduleAt required for mode=schedule,
	// rejected for every other mode to avoid silent no-ops.
	var scheduleUnix int
	var scheduleAtOut string
	if mode == sendModeSchedule {
		if in.ScheduleAt == "" {
			return errResult("schedule_at is required when mode=\"schedule\". Provide an RFC3339 timestamp in the future (e.g. 2026-04-10T15:30:00Z)."), nil, nil
		}
		t, errRes := parseFutureSchedule(in.ScheduleAt)
		if errRes != nil {
			return errRes, nil, nil
		}
		scheduleUnix = int(t.Unix())
		scheduleAtOut = t.UTC().Format(time.RFC3339)
	} else if in.ScheduleAt != "" {
		return errResult(fmt.Sprintf("schedule_at is only valid when mode=\"schedule\" (got mode=%q). Set mode=\"schedule\" or remove schedule_at.", mode)), nil, nil
	}

	// Draft mode: messages.saveDraft supports reply_to_msg_id but does not
	// return a message ID, so the response has no MessageID.
	if mode == sendModeDraft {
		draftReq := &tg.MessagesSaveDraftRequest{Message: in.Message}
		if replyToID > 0 {
			draftReq.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: replyToID}
		}
		if _, err := tgclient.WithPeer(ctx, h.peers, in.ChatID, func(p tgclient.Peer) (bool, error) {
			draftReq.Peer = p.Input
			return h.peers.Client().MessagesSaveDraft(ctx, draftReq)
		}); err != nil {
			return nil, nil, failed(fmt.Sprintf("save draft in chat %d", in.ChatID), err)
		}
		return nil, &SendMessageResult{
			Status:             "drafted",
			ChatID:             in.ChatID,
			RepliedToMessageID: replyHandle,
			Note:               "Saved as draft in the Telegram app. Not sent. Open Telegram to edit or send it.",
		}, nil
	}

	// Send or schedule path.
	sendReq := &tg.MessagesSendMessageRequest{
		Message:  in.Message,
		RandomID: tgclient.RandomID(),
	}
	if replyToID > 0 {
		sendReq.ReplyTo = &tg.InputReplyToMessage{ReplyToMsgID: replyToID}
	}
	if scheduleUnix > 0 {
		sendReq.ScheduleDate = scheduleUnix
	}

	updates, err := tgclient.WithPeer(ctx, h.peers, in.ChatID, func(p tgclient.Peer) (tg.UpdatesClass, error) {
		sendReq.Peer = p.Input
		return h.peers.Client().MessagesSendMessage(ctx, sendReq)
	})
	if err != nil {
		return nil, nil, failed(fmt.Sprintf("send message to chat %d", in.ChatID), err)
	}

	res := &SendMessageResult{
		ChatID:             in.ChatID,
		RepliedToMessageID: replyHandle,
	}

	if mode == sendModeSchedule {
		// Try the scheduled extractor first; fall back to the regular one
		// because Telegram delivers sub-~10s schedules immediately and
		// returns a regular UpdateNewMessage instead of
		// UpdateNewScheduledMessage.
		msgID, date := extractScheduledMessageID(updates)
		if msgID > 0 {
			res.Status = "scheduled"
			res.MessageID = presentation.FormatScheduledRef(msgID)
			res.ScheduleAt = scheduleAtOut
			res.Note = "Stored on Telegram's servers — delivered automatically at schedule_at even if you're offline."
			if date > 0 {
				res.Date = formatUnixRFC3339(date)
			}
			return nil, res, nil
		}
		// Fell through to immediate send.
		fillSent(ctx, req, res, "sent_immediate", updates)
		res.ScheduleAt = scheduleAtOut
		if res.Note == "" {
			res.Note = "schedule_at was under ~10 seconds away — Telegram delivered the message immediately instead of queueing it."
		}
		return nil, res, nil
	}

	// Regular immediate send (mode=send or default).
	fillSent(ctx, req, res, "sent", updates)
	return nil, res, nil
}

// fillSent records in res, under status, the regular message updates says a
// send delivered. When Telegram answered with an update the extractor cannot
// read, the message was delivered but its ID is unknown: the note says so and
// the operator hears of it.
func fillSent(ctx context.Context, req *mcp.CallToolRequest, res *SendMessageResult, status string, updates tg.UpdatesClass) {
	res.Status = status
	msgID, date := extractSentMessageID(updates)
	if msgID > 0 {
		res.MessageID = presentation.FormatRegularRef(msgID)
	} else {
		mcpLog(ctx, req.Session, logLevelWarning, "SendMessage", map[string]any{
			"action": "message_id_extraction_failed",
			"note":   "Telegram returned an unrecognised update type; message_id in response is unreliable",
		})
		res.Note = "message_id unavailable: Telegram returned an unrecognised update type. The message was delivered but cannot be referenced for edits or deletes until fetched via GetMessages."
	}
	if date > 0 {
		res.Date = formatUnixRFC3339(date)
	}
}

// extractSentMessageID pulls the new message ID + date out of an UpdatesClass
// returned by messages.sendMessage for a regular (non-scheduled) send.
// Returns (0, 0) when no matching update is found — callers should treat
// that as a "delivered but ID unknown" case rather than a hard failure.
func extractSentMessageID(updates tg.UpdatesClass) (int, int) {
	switch u := updates.(type) {
	case *tg.UpdateShortSentMessage:
		return u.ID, u.Date
	case *tg.UpdateShortMessage:
		// Telegram may return UpdateShortMessage for direct-message sends
		// (a single-recipient PM where the server can omit full Updates).
		return u.ID, u.Date
	}
	return firstMessageInUpdates(updates, tg.UpdateNewMessageTypeID, tg.UpdateNewChannelMessageTypeID)
}

// extractScheduledMessageID pulls the new message ID + date out of an
// UpdatesClass returned by messages.sendMessage when ScheduleDate was set.
// When Telegram queues the message it returns UpdateNewScheduledMessage;
// when the requested schedule is too close (sub-~10s) Telegram delivers
// immediately and returns a regular UpdateNewMessage instead — in that
// case this helper returns (0, 0) and the caller should fall back to
// extractSentMessageID to recover the ID.
func extractScheduledMessageID(updates tg.UpdatesClass) (int, int) {
	return firstMessageInUpdates(updates, tg.UpdateNewScheduledMessageTypeID)
}
