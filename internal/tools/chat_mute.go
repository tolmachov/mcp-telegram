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
	peers  *tgclient.Resolver
}

// NewChatMuteHandler creates a new ChatMuteHandler.
func NewChatMuteHandler(peers *tgclient.Resolver) *ChatMuteHandler {
	return &ChatMuteHandler{client: peers.Client(), peers: peers}
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
	AddTool(s, &mcp.Tool{
		Name:        "SetChatMute",
		Description: "Mute or unmute chat notifications. When muted=true: duration_seconds=0 mutes forever, positive value mutes for N seconds (each call resets the timer — not idempotent). When muted=false: unmutes immediately (idempotent); duration_seconds is ignored.",
		// Note: IdempotentHint is intentionally not set. The tool is
		// idempotent only for muted=false; muting with a duration resets
		// the timer on every call. MCP annotations apply at the tool
		// level, not per-call, so we can't truthfully claim idempotence.
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: new(true)},
	}, h.handle)
}

func (h *ChatMuteHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in SetChatMuteInput) (*mcp.CallToolResult, *SetChatMuteResult, error) {
	if in.ChatID == 0 {
		return errChatIDRequired(), nil, nil
	}
	if in.DurationSeconds < 0 {
		return errResult("duration_seconds must be >= 0 (0 = mute forever)"), nil, nil
	}

	// mute_until=0 restores default settings (unmute); otherwise mute either
	// forever (sentinel) or for a relative duration from now.
	var muteUntil int
	op := fmt.Sprintf("mute chat %d", in.ChatID)
	switch {
	case !in.Muted:
		op = fmt.Sprintf("unmute chat %d", in.ChatID)
	case in.DurationSeconds == 0:
		muteUntil = muteForeverUntil
	default:
		muteUntil = int(time.Now().Unix()) + in.DurationSeconds
	}
	if _, err := tgclient.WithPeer(ctx, h.peers, in.ChatID, func(p tgclient.Peer) (bool, error) {
		return h.client.AccountUpdateNotifySettings(ctx, &tg.AccountUpdateNotifySettingsRequest{
			Peer:     &tg.InputNotifyPeer{Peer: p.Input},
			Settings: tg.InputPeerNotifySettings{MuteUntil: muteUntil},
		})
	}); err != nil {
		return nil, nil, failed(op, err)
	}
	if !in.Muted {
		return nil, &SetChatMuteResult{Status: "unmuted", ChatID: in.ChatID}, nil
	}

	res := &SetChatMuteResult{
		Status:          "muted",
		ChatID:          in.ChatID,
		DurationSeconds: in.DurationSeconds,
	}
	if in.DurationSeconds == 0 {
		res.Forever = true
	} else {
		res.MutedUntil = formatUnixRFC3339(muteUntil)
	}
	return nil, res, nil
}
