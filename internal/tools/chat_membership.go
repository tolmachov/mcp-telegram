package tools

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// Membership status values reported by JoinChat / LeaveChat.
const (
	statusJoined         = "joined"
	statusAlreadyMember  = "already_member"
	statusRequested      = "requested"
	statusActionRequired = "action_required"
	statusLeft           = "left"
	statusNotMember      = "not_member"
	statusCancelled      = "cancelled"
)

// chat-reference kinds returned by classifyChatRef.
const (
	chatRefInvite   = "invite"
	chatRefUsername = "username"
	chatRefID       = "id"
)

// JoinChatHandler handles the JoinChat tool.
type JoinChatHandler struct {
	client *tg.Client
}

// NewJoinChatHandler creates a new JoinChatHandler.
func NewJoinChatHandler(client *tg.Client) *JoinChatHandler {
	return &JoinChatHandler{client: client}
}

// LeaveChatHandler handles the LeaveChat tool.
type LeaveChatHandler struct {
	client *tg.Client
}

// NewLeaveChatHandler creates a new LeaveChatHandler.
func NewLeaveChatHandler(client *tg.Client) *LeaveChatHandler {
	return &LeaveChatHandler{client: client}
}

// JoinChatInput is the input for the JoinChat tool.
type JoinChatInput struct {
	Chat string `json:"chat" jsonschema:"Public @username, numeric chat ID, or an invite link (t.me/+hash, t.me/joinchat/hash). For public channels/supergroups you haven't joined yet, prefer @username — a bare numeric ID only works if the chat is already known to the session."`
}

// JoinChatResult is the typed output of JoinChat. Title/Kind/ChatID are
// best-effort: some join responses carry no chat updates (e.g. an invite-link
// join, or a web-view join result), in which case they are omitted. When the
// join is gated behind an in-app web view (verification/captcha), Status is
// "action_required" and Detail explains what the user must do to finish.
type JoinChatResult struct {
	Status string `json:"status"` // "joined" | "already_member" | "requested" | "action_required"
	Chat   string `json:"chat"`
	ChatID int64  `json:"chat_id,omitempty"`
	Title  string `json:"title,omitempty"`
	Kind   string `json:"kind,omitempty"` // "channel" | "supergroup" | "chat"
	Detail string `json:"detail,omitempty"`
}

// LeaveChatInput is the input for the LeaveChat tool.
type LeaveChatInput struct {
	Chat    string `json:"chat" jsonschema:"Public @username or numeric chat ID of a chat you are currently a member of"`
	Confirm bool   `json:"confirm,omitempty" jsonschema:"Set to true to proceed with leaving. Leaving is destructive (rejoining a private chat needs a fresh invite link), so only set this after the user has confirmed. When false/omitted, the host may present its own confirmation prompt, and in non-interactive clients the call is cancelled without leaving."`
}

// LeaveChatResult is the typed output of LeaveChat.
type LeaveChatResult struct {
	Status string `json:"status"` // "left" | "not_member" | "cancelled"
	Chat   string `json:"chat"`
	ChatID int64  `json:"chat_id,omitempty"`
	Kind   string `json:"kind,omitempty"` // "channel" | "chat" (supergroups report as "channel")
}

// Register adds the JoinChat tool to the MCP server.
func (h *JoinChatHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "JoinChat",
		Description: "Join a Telegram channel, group, or supergroup. Accepts a public @username, a numeric chat ID, or an invite link (t.me/+hash or t.me/joinchat/hash) for private chats. Joining is reversible — use LeaveChat to undo. Some chats require admin approval; in that case the result status is \"requested\" rather than \"joined\". If joining is gated behind an in-app verification step, the status is \"action_required\" and detail explains what the user must do in an official Telegram client to finish. Legacy basic groups can only be joined via an invite link.",
		Annotations: &mcp.ToolAnnotations{OpenWorldHint: ptrTrue()},
	}, h.handle)
}

// Register adds the LeaveChat tool to the MCP server.
func (h *LeaveChatHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "LeaveChat",
		Description: "Leave a Telegram channel, group, or supergroup you are a member of. Accepts a public @username or a numeric chat ID. This removes the chat from your dialog list; rejoining a private chat afterwards requires a fresh invite link, so the host may ask you to confirm. You cannot leave a channel you own.",
		Annotations: &mcp.ToolAnnotations{DestructiveHint: ptrTrue(), OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *JoinChatHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in JoinChatInput) (*mcp.CallToolResult, *JoinChatResult, error) {
	chat := strings.TrimSpace(in.Chat)
	if chat == "" {
		return errResult("chat is required: pass a public @username, a numeric chat ID, or an invite link (t.me/+hash)."), nil, nil
	}

	kind, value := classifyChatRef(chat)
	switch kind {
	case chatRefInvite:
		return h.joinByInvite(ctx, req, chat, value)
	case chatRefUsername:
		return h.joinByUsername(ctx, req, chat, value)
	default: // chatRefID
		return h.joinByID(ctx, req, chat, value)
	}
}

// joinByInvite imports a private invite hash via messages.importChatInvite.
func (h *JoinChatHandler) joinByInvite(ctx context.Context, req *mcp.CallToolRequest, chat, hash string) (*mcp.CallToolResult, *JoinChatResult, error) {
	res, err := h.client.MessagesImportChatInvite(ctx, hash)
	if err != nil {
		return h.joinError(ctx, req, chat, &JoinChatResult{Status: statusAlreadyMember, Chat: chat}, err)
	}
	return nil, joinResultFrom(chat, statusJoined, res), nil
}

// joinByUsername resolves a public @username to a channel and joins it.
func (h *JoinChatHandler) joinByUsername(ctx context.Context, req *mcp.CallToolRequest, chat, username string) (*mcp.CallToolResult, *JoinChatResult, error) {
	input, channel, err := resolveChannelByUsername(ctx, h.client, username)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to resolve @%s: %v. You can only join channels and supergroups by username; for a private chat use its invite link instead.", username, err)), nil, nil
	}
	known := &JoinChatResult{Status: statusAlreadyMember, Chat: chat}
	fillJoinResultFromChannel(known, channel)

	res, err := h.client.ChannelsJoinChannel(ctx, input)
	if err != nil {
		return h.joinError(ctx, req, chat, known, err)
	}
	out := joinResultFrom(chat, statusJoined, res)
	if out.ChatID == 0 {
		fillJoinResultFromChannel(out, channel)
	}
	return nil, out, nil
}

// joinByID resolves a numeric ID to a peer and joins it (channels/supergroups
// only — basic groups and users cannot be joined by ID).
func (h *JoinChatHandler) joinByID(ctx context.Context, req *mcp.CallToolRequest, chat, value string) (*mcp.CallToolResult, *JoinChatResult, error) {
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return errResult(fmt.Sprintf("invalid chat reference %q: expected a public @username, a numeric chat ID, or an invite link.", chat)), nil, nil
	}
	peer, err := tgclient.ResolvePeer(ctx, h.client, id)
	if err != nil {
		return errResolvePeer(id, err), nil, nil
	}
	ch, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		return errResult(fmt.Sprintf("chat %d is not a channel or supergroup. Only channels/supergroups can be joined by ID; basic groups and private chats require an invite link.", id)), nil, nil
	}
	input := &tg.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash}
	known := &JoinChatResult{Status: statusAlreadyMember, Chat: chat, ChatID: ch.ChannelID}

	res, err := h.client.ChannelsJoinChannel(ctx, input)
	if err != nil {
		return h.joinError(ctx, req, chat, known, err)
	}
	out := joinResultFrom(chat, statusJoined, res)
	if out.ChatID == 0 {
		out.ChatID = ch.ChannelID
	}
	return nil, out, nil
}

// joinError maps a join failure to either a soft success (already a member /
// request sent) or a hard error. alreadyResult carries any chat info we
// already know so an "already a member" outcome still reports it.
func (h *JoinChatHandler) joinError(ctx context.Context, req *mcp.CallToolRequest, chat string, alreadyResult *JoinChatResult, err error) (*mcp.CallToolResult, *JoinChatResult, error) {
	switch {
	case tgerr.Is(err, "USER_ALREADY_PARTICIPANT"):
		alreadyResult.Status = statusAlreadyMember
		return nil, alreadyResult, nil
	case tgerr.Is(err, "INVITE_REQUEST_SENT"):
		alreadyResult.Status = statusRequested
		return nil, alreadyResult, nil
	}
	mcpLog(ctx, req.Session, logLevelWarning, "JoinChat", map[string]any{
		"chat":  chat,
		"error": err.Error(),
	})
	if res, ok := floodWaitResult("join", err); ok {
		return res, nil, nil
	}
	return errResult(fmt.Sprintf("Failed to join %q: %v", chat, err)), nil, nil
}

func (h *LeaveChatHandler) handle(ctx context.Context, req *mcp.CallToolRequest, in LeaveChatInput) (*mcp.CallToolResult, *LeaveChatResult, error) {
	chat := strings.TrimSpace(in.Chat)
	if chat == "" {
		return errResult("chat is required: pass a public @username or a numeric chat ID of a chat you're a member of."), nil, nil
	}

	peer, errRes := h.resolvePeer(ctx, chat)
	if errRes != nil {
		return errRes, nil, nil
	}

	// Reject peers that can't be left before prompting, so the confirmation
	// dialog only appears for actionable requests (mirrors message_delete.go,
	// which resolves first and confirms last).
	channelPeer, isChannel := peer.(*tg.InputPeerChannel)
	chatPeer, isChat := peer.(*tg.InputPeerChat)
	if !isChannel && !isChat {
		return errResult(fmt.Sprintf("%q is a private (one-to-one) chat, not a group or channel — there's nothing to leave. Use DeleteMessage or your client to clear the conversation instead.", chat)), nil, nil
	}

	// in.Confirm lets the caller confirm in-band (per Server.Instructions, the
	// model asks the user before destructive actions). This is the only path
	// that works in non-interactive clients, where the elicitation form below
	// can never be accepted. Fall back to elicitation only when confirm is unset.
	if !in.Confirm {
		confirmed, err := confirmDestructive(ctx, req, fmt.Sprintf(
			"Leave chat %q? You'll lose access; rejoining a private chat requires a fresh invite link.", chat,
		))
		if err != nil {
			return errResult(fmt.Sprintf("confirmation failed: %v", err)), nil, nil
		}
		if !confirmed {
			// Return a populated result (not a bare text result with a nil typed
			// output): the SDK fills StructuredContent from the zero value of a nil
			// pointer output, so a text-only cancel surfaces as an empty
			// {"chat":"","status":""} that masks the reason. An explicit
			// "cancelled" status keeps the outcome legible.
			return nil, &LeaveChatResult{Status: statusCancelled, Chat: chat}, nil
		}
	}

	if isChannel {
		return h.leaveChannel(ctx, req, chat, channelPeer)
	}
	return h.leaveBasicChat(ctx, req, chat, chatPeer)
}

// resolvePeer turns a @username or numeric ID into an InputPeer, returning a
// ready-to-send error result on failure.
func (h *LeaveChatHandler) resolvePeer(ctx context.Context, chat string) (tg.InputPeerClass, *mcp.CallToolResult) {
	kind, value := classifyChatRef(chat)
	switch kind {
	case chatRefUsername:
		input, _, err := resolveChannelByUsername(ctx, h.client, value)
		if err != nil {
			return nil, errResult(fmt.Sprintf("Failed to resolve @%s: %v. You can only leave channels and supergroups by username.", value, err))
		}
		return &tg.InputPeerChannel{ChannelID: input.ChannelID, AccessHash: input.AccessHash}, nil
	case chatRefInvite:
		return nil, errResult("LeaveChat does not accept invite links. Pass the chat's @username or numeric ID instead (find it with GetChats or SearchChats).")
	default: // chatRefID
		id, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, errResult(fmt.Sprintf("invalid chat reference %q: expected a public @username or a numeric chat ID.", chat))
		}
		peer, err := tgclient.ResolvePeer(ctx, h.client, id)
		if err != nil {
			return nil, errResolvePeer(id, err)
		}
		return peer, nil
	}
}

func (h *LeaveChatHandler) leaveChannel(ctx context.Context, req *mcp.CallToolRequest, chat string, p *tg.InputPeerChannel) (*mcp.CallToolResult, *LeaveChatResult, error) {
	_, err := h.client.ChannelsLeaveChannel(ctx, &tg.InputChannel{ChannelID: p.ChannelID, AccessHash: p.AccessHash})
	if err != nil {
		if tgerr.Is(err, "USER_NOT_PARTICIPANT") {
			return nil, &LeaveChatResult{Status: statusNotMember, Chat: chat, ChatID: p.ChannelID, Kind: "channel"}, nil
		}
		if tgerr.Is(err, "USER_CREATOR") {
			return errResult(fmt.Sprintf("Cannot leave %q: you are its owner. Transfer ownership or delete the channel instead.", chat)), nil, nil
		}
		mcpLog(ctx, req.Session, logLevelWarning, "LeaveChat", map[string]any{"chat": chat, "error": err.Error()})
		if res, ok := floodWaitResult("leave", err); ok {
			return res, nil, nil
		}
		return errResult(fmt.Sprintf("Failed to leave %q: %v", chat, err)), nil, nil
	}
	return nil, &LeaveChatResult{Status: statusLeft, Chat: chat, ChatID: p.ChannelID, Kind: "channel"}, nil
}

func (h *LeaveChatHandler) leaveBasicChat(ctx context.Context, req *mcp.CallToolRequest, chat string, p *tg.InputPeerChat) (*mcp.CallToolResult, *LeaveChatResult, error) {
	_, err := h.client.MessagesDeleteChatUser(ctx, &tg.MessagesDeleteChatUserRequest{
		ChatID: p.ChatID,
		UserID: &tg.InputUserSelf{},
	})
	if err != nil {
		if tgerr.Is(err, "USER_NOT_PARTICIPANT") {
			return nil, &LeaveChatResult{Status: statusNotMember, Chat: chat, ChatID: p.ChatID, Kind: "chat"}, nil
		}
		mcpLog(ctx, req.Session, logLevelWarning, "LeaveChat", map[string]any{"chat": chat, "error": err.Error()})
		if res, ok := floodWaitResult("leave", err); ok {
			return res, nil, nil
		}
		return errResult(fmt.Sprintf("Failed to leave %q: %v", chat, err)), nil, nil
	}
	return nil, &LeaveChatResult{Status: statusLeft, Chat: chat, ChatID: p.ChatID, Kind: "chat"}, nil
}

// classifyChatRef determines how a chat reference string should be interpreted:
// an invite link/hash, a public @username, or a numeric ID. Hash extraction is
// purely local (no network call). A non-numeric, non-link string is treated as
// a username (with or without a leading @).
func classifyChatRef(s string) (kind, value string) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ""
	}
	// tg://join?invite=HASH
	if strings.HasPrefix(s, "tg://join") {
		if u, err := url.Parse(s); err == nil {
			if h := u.Query().Get("invite"); h != "" {
				return chatRefInvite, h
			}
		}
	}
	// Bare invite hash: +HASH
	if hash, ok := strings.CutPrefix(s, "+"); ok {
		return chatRefInvite, hash
	}
	// A t.me link: invite (t.me/+hash, t.me/joinchat/hash) or public (t.me/<username>).
	if k, v, ok := chatRefFromURL(s); ok {
		return k, v
	}
	// @username
	if username, ok := strings.CutPrefix(s, "@"); ok {
		return chatRefUsername, username
	}
	// Numeric ID (a leading - is accepted for legacy Bot-API-marked IDs).
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return chatRefID, s
	}
	// Anything else is treated as a bare username.
	return chatRefUsername, s
}

// chatRefFromURL classifies a Telegram link (t.me / telegram.me / telegram.dog,
// with or without a scheme) as either an invite (t.me/+hash, t.me/joinchat/hash)
// or a public username (t.me/<username>). Parsing is purely local (no network).
// ok is false when s is not a recognised Telegram link, so the caller can fall
// through to its other classification rules.
func chatRefFromURL(s string) (kind, value string, ok bool) {
	raw := s
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", false
	}
	switch strings.ToLower(u.Host) {
	case "t.me", "telegram.me", "telegram.dog":
	default:
		return "", "", false
	}
	segments := make([]string, 0, 2)
	for seg := range strings.SplitSeq(u.Path, "/") {
		if seg != "" {
			segments = append(segments, seg)
		}
	}
	if len(segments) == 0 {
		return "", "", false
	}
	if hash, hasPlus := strings.CutPrefix(segments[0], "+"); hasPlus {
		return chatRefInvite, hash, true
	}
	if segments[0] == "joinchat" {
		if len(segments) < 2 {
			return "", "", false
		}
		return chatRefInvite, segments[1], true
	}
	// Public username link: t.me/<username>.
	return chatRefUsername, segments[0], true
}

// resolveChannelByUsername resolves a public @username to the channel/supergroup
// it names, returning both the InputChannel needed for join/leave and the full
// *tg.Channel for metadata. Users and basic chats are rejected — only
// channels/supergroups have a public username you can act on this way.
func resolveChannelByUsername(ctx context.Context, client *tg.Client, username string) (*tg.InputChannel, *tg.Channel, error) {
	resolved, err := resolvePublicUsername(ctx, client, username)
	if err != nil {
		return nil, nil, err
	}
	input, channel, err := resolvedChannel(resolved)
	if err != nil {
		return nil, nil, fmt.Errorf("@%s is %w", strings.TrimPrefix(strings.TrimSpace(username), "@"), err)
	}
	return input, channel, nil
}

// joinResultFrom builds a JoinChatResult from the join response. The Ok variant
// carries chat updates we mine for metadata. The WebView variant means the join
// did NOT complete — Telegram requires the user to finish an in-app web view
// (verification/captcha) — so we downgrade the status to "action_required"
// instead of falsely reporting a successful join. The response used to carry the
// web view URL; since the schema behind gotd v0.161.0 it only carries the bot and
// query IDs, so there is nothing to hand back but an explanation.
func joinResultFrom(chat, status string, res tg.MessagesChatInviteJoinResultClass) *JoinChatResult {
	out := &JoinChatResult{Status: status, Chat: chat}
	switch r := res.(type) {
	case *tg.MessagesChatInviteJoinResultOk:
		if id, title, kind := chatInfoFromUpdates(r.Updates); id != 0 {
			out.ChatID, out.Title, out.Kind = id, title, kind
		}
	case *tg.MessagesChatInviteJoinResultWebView:
		out.Status = statusActionRequired
		out.Detail = "Telegram gated this join behind an in-app web view (verification/captcha). " +
			"Open the chat or invite link in an official Telegram client and complete it there."
	}
	return out
}

// fillJoinResultFromChannel populates chat metadata from a resolved *tg.Channel.
func fillJoinResultFromChannel(out *JoinChatResult, channel *tg.Channel) {
	if channel == nil {
		return
	}
	out.ChatID = channel.ID
	out.Title = channel.Title
	out.Kind = "channel"
	if channel.Megagroup {
		out.Kind = "supergroup"
	}
}

// chatInfoFromUpdates pulls the first channel/chat out of an UpdatesClass so a
// join can report which chat was joined. Returns id=0 when none is present.
func chatInfoFromUpdates(u tg.UpdatesClass) (id int64, title, kind string) {
	var chats []tg.ChatClass
	switch v := u.(type) {
	case *tg.Updates:
		chats = v.Chats
	case *tg.UpdatesCombined:
		chats = v.Chats
	}
	for _, c := range chats {
		switch ch := c.(type) {
		case *tg.Channel:
			k := "channel"
			if ch.Megagroup {
				k = "supergroup"
			}
			return ch.ID, ch.Title, k
		case *tg.Chat:
			return ch.ID, ch.Title, "chat"
		}
	}
	return 0, "", ""
}
