package tools

import (
	"context"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// muteForeverUntil is Telegram's sentinel value for "muted forever":
// int32 max, i.e. ~year 2038 in Unix seconds. Passing this to
// account.updateNotifySettings.mute_until pins the chat to permanently
// muted until the user explicitly unmutes.
const muteForeverUntil = 2147483647

// ChatMuteHandler handles the SetChatMute tool.
type ChatMuteHandler struct {
	client *tg.Client
}

// NewChatMuteHandler creates a new ChatMuteHandler.
func NewChatMuteHandler(client *tg.Client) *ChatMuteHandler {
	return &ChatMuteHandler{client: client}
}

// SetChatMuteInput is the input for the SetChatMute tool. Muted selects
// the action (mute vs unmute) and DurationSeconds controls the mute
// window when muting. DurationSeconds is ignored when Muted is false.
type SetChatMuteInput struct {
	ChatID          int64 `json:"chat_id" jsonschema:"The ID of the chat to mute or unmute"`
	Muted           bool  `json:"muted" jsonschema:"true to mute\\, false to unmute"`
	DurationSeconds int   `json:"duration_seconds,omitempty" jsonschema:"Mute duration in seconds when muted=true. 0 = mute forever. Ignored when muted=false. Must be >= 0."`
}

// SetChatMuteResult is the typed output of SetChatMute.
type SetChatMuteResult struct {
	Status          string `json:"status"` // "muted" | "unmuted"
	ChatID          int64  `json:"chat_id"`
	DurationSeconds int    `json:"duration_seconds,omitempty"`
	Forever         bool   `json:"forever,omitempty"`
	MutedUntil      string `json:"muted_until,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *ChatMuteHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "SetChatMute",
		Description: "Mute or unmute chat notifications. When muted=true: duration_seconds=0 mutes forever, positive value mutes for N seconds (each call resets the timer — not idempotent). When muted=false: unmutes immediately (idempotent); duration_seconds is ignored.",
		// Note: IdempotentHint is intentionally not set. The tool is
		// idempotent only for muted=false; muting with a duration resets
		// the timer on every call. MCP annotations apply at the tool
		// level, not per-call, so we can't truthfully claim idempotence.
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *ChatMuteHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in SetChatMuteInput) (*mcp.CallToolResult, *SetChatMuteResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	if in.DurationSeconds < 0 {
		return errResult("duration_seconds must be >= 0 (0 = mute forever)"), nil, nil
	}

	peer, err := tgclient.ResolvePeer(ctx, h.client, in.ChatID)
	if err != nil {
		return errResolvePeer(in.ChatID, err), nil, nil
	}

	notifyPeer, ok := toInputNotifyPeer(peer)
	if !ok {
		return errResult("Unsupported peer type"), nil, nil
	}

	// Unmute path: mute_until=0 restores default settings.
	if !in.Muted {
		if _, err := h.client.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{
			Peer:     notifyPeer,
			Settings: tg.InputPeerNotifySettings{MuteUntil: 0},
		}); err != nil {
			return errResult(fmt.Sprintf("Failed to unmute chat: %v", err)), nil, nil
		}
		return nil, &SetChatMuteResult{
			Status: "unmuted",
			ChatID: in.ChatID,
		}, nil
	}

	// Mute path: either forever (sentinel) or a relative duration from now.
	var muteUntil int
	if in.DurationSeconds == 0 {
		muteUntil = muteForeverUntil
	} else {
		muteUntil = int(time.Now().Unix()) + in.DurationSeconds
	}

	if _, err := h.client.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{
		Peer:     notifyPeer,
		Settings: tg.InputPeerNotifySettings{MuteUntil: muteUntil},
	}); err != nil {
		return errResult(fmt.Sprintf("Failed to mute chat: %v", err)), nil, nil
	}

	res := &SetChatMuteResult{
		Status:          "muted",
		ChatID:          in.ChatID,
		DurationSeconds: in.DurationSeconds,
	}
	if in.DurationSeconds == 0 {
		res.Forever = true
	} else {
		res.MutedUntil = time.Unix(int64(muteUntil), 0).UTC().Format(time.RFC3339)
	}
	return nil, res, nil
}

// toInputNotifyPeer converts an InputPeer into the InputNotifyPeer wrapper
// expected by AccountUpdateNotifySettings.
func toInputNotifyPeer(peer tg.InputPeerClass) (tg.InputNotifyPeerClass, bool) {
	switch p := peer.(type) {
	case *tg.InputPeerUser:
		return &tg.InputNotifyPeer{
			Peer: &tg.InputPeerUser{UserID: p.UserID, AccessHash: p.AccessHash},
		}, true
	case *tg.InputPeerChat:
		return &tg.InputNotifyPeer{
			Peer: &tg.InputPeerChat{ChatID: p.ChatID},
		}, true
	case *tg.InputPeerChannel:
		return &tg.InputNotifyPeer{
			Peer: &tg.InputPeerChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash},
		}, true
	default:
		return nil, false
	}
}
