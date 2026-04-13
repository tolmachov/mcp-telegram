package tools

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgclient"
)

// MessageLinkResolveHandler handles the ResolveMessageLink tool. It turns a
// t.me message URL into the numeric chat_id and opaque message handle the
// rest of the toolbox expects, saving the model a two-step dance of
// ResolveUsername → GetMessages when it already has a direct link.
type MessageLinkResolveHandler struct {
	client *tg.Client
}

// NewMessageLinkResolveHandler creates a new MessageLinkResolveHandler.
func NewMessageLinkResolveHandler(client *tg.Client) *MessageLinkResolveHandler {
	return &MessageLinkResolveHandler{client: client}
}

// ResolveMessageLinkInput is the input for the ResolveMessageLink tool.
type ResolveMessageLinkInput struct {
	Link string `json:"link" jsonschema:"Telegram message URL. Supported forms: https://t.me/<username>/<message_id>, https://t.me/<username>/<topic_id>/<message_id>, https://t.me/c/<internal_id>/<message_id>, https://t.me/c/<internal_id>/<topic_id>/<message_id>. The tg:// scheme and telegram.me host are also accepted."`
}

// ResolveMessageLinkResult is the typed output of ResolveMessageLink.
//
// ChatID and MessageID are always populated. For forum links, TopicMessageID
// is the opaque regular-message handle of the topic/thread root, ready to feed
// into SearchMessages.top_msg_id. TopicID is kept as the raw numeric message
// ID for backward compatibility and is zero for non-forum links.
//
// Public-link only (populated together when resolved via username; omitted for
// private channel links):
//
//	ChatTitle, Username
//
// Private-link only (offline resolution, no API call):
//
//	Hint
type ResolveMessageLinkResult struct {
	ChatID         int64  `json:"chat_id"`
	MessageID      string `json:"message_id"`
	TopicID        int    `json:"topic_id,omitempty"`
	TopicMessageID string `json:"topic_message_id,omitempty"`

	// Public-link only.
	ChatTitle string `json:"chat_title,omitempty"`
	Username  string `json:"username,omitempty"`

	// Private-link only.
	Hint string `json:"next_step_hint,omitempty"`
}

// Register adds the tool to the MCP server.
func (h *MessageLinkResolveHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "ResolveMessageLink",
		Description: "Parse a Telegram message URL into a chat_id + opaque message handle ready for GetMessages / GetMessageContext / DeleteMessage. Supports public (t.me/<username>/<id>), private-channel (t.me/c/<internal_id>/<id>), and forum (…/<topic_id>/<id>) link forms. Forum links also return topic_message_id, an opaque handle ready for SearchMessages.top_msg_id. Returns chat metadata from Telegram for public links; private-channel links are resolved offline (no API call) and carry no chat_title.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *MessageLinkResolveHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, in ResolveMessageLinkInput) (*mcp.CallToolResult, *ResolveMessageLinkResult, error) {
	if in.Link == "" {
		return errResult("link is required. Expected a t.me message URL such as https://t.me/durov/123 or https://t.me/c/1234567890/42."), nil, nil
	}

	parsed, err := parseTMeLink(in.Link)
	if err != nil {
		return errResult(fmt.Sprintf("invalid link %q: %v. Supported forms: https://t.me/<username>/<message_id>, https://t.me/c/<internal_id>/<message_id>, with an optional <topic_id> segment in between for forum chats.", in.Link, err)), nil, nil
	}

	out := &ResolveMessageLinkResult{
		MessageID: FormatRegularRef(parsed.MessageID),
		TopicID:   parsed.TopicID,
	}
	if parsed.TopicID > 0 {
		out.TopicMessageID = FormatRegularRef(parsed.TopicID)
	}

	if parsed.Username != "" {
		resolved, err := h.client.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
			Username: parsed.Username,
		})
		if err != nil {
			return errResult(fmt.Sprintf("failed to resolve username @%s from link: %v", parsed.Username, err)), nil, nil
		}
		chatID, title, found := chatIDFromResolved(resolved)
		if !found {
			return errResult(fmt.Sprintf("username @%s resolved with no usable chat or user. The link may point to a private or deleted entity.", parsed.Username)), nil, nil
		}
		out.ChatID = chatID
		out.ChatTitle = title
		out.Username = parsed.Username
	} else {
		// Private channel form: t.me/c/<internal>/<id>. The URL's "c" segment
		// is the raw channel ID without the -100 prefix; the rest of the
		// toolbox expects the user-facing signed form. We don't hit the API
		// here — ChatInfoGet can enrich on demand if the caller needs metadata.
		out.ChatID = -(tgclient.ChannelIDPrefix + parsed.ChannelRaw)
		out.Hint = "Private channel link — chat_title omitted (no API call). Call GetChatInfo with chat_id if you need the title/members."
	}

	return nil, out, nil
}

// parsedLink captures the pieces we extract from a t.me URL. Exactly one of
// Username and ChannelRaw is populated; use newPublicParsedLink or
// newPrivateParsedLink to construct values so the invariant is enforced.
type parsedLink struct {
	Username   string
	ChannelRaw int64
	TopicID    int
	MessageID  int
}

// newPublicParsedLink constructs a parsedLink for a public username-based link.
// ChannelRaw is always zero (the XOR invariant).
func newPublicParsedLink(username string, topicID, msgID int) parsedLink {
	return parsedLink{Username: username, TopicID: topicID, MessageID: msgID}
}

// newPrivateParsedLink constructs a parsedLink for a private channel link.
// Username is always empty (the XOR invariant).
func newPrivateParsedLink(channelRaw int64, topicID, msgID int) parsedLink {
	return parsedLink{ChannelRaw: channelRaw, TopicID: topicID, MessageID: msgID}
}

// parseTMeLink parses a Telegram message URL into its components. The parser
// is deliberately strict: it rejects invite links, bare chat links (no
// message id), reserved path segments, and malformed ids. Pure function,
// no network.
func parseTMeLink(raw string) (parsedLink, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return parsedLink{}, fmt.Errorf("empty link")
	}

	// Strip a leading @ if the user pasted an @ before the host.
	raw = strings.TrimPrefix(raw, "@")

	// Accept bare "t.me/..." (no scheme) by prepending one so url.Parse can
	// split the host cleanly.
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return parsedLink{}, fmt.Errorf("parse url: %w", err)
	}

	host := strings.ToLower(u.Host)
	switch host {
	case "t.me", "telegram.me", "telegram.dog":
		// ok
	case "resolve":
		// tg://resolve?domain=foo&post=123 form — handled below via query.
	default:
		return parsedLink{}, fmt.Errorf("unsupported host %q (expected t.me or telegram.me)", u.Host)
	}

	// tg://resolve?domain=foo&post=123 form.
	if host == "resolve" || u.Scheme == "tg" {
		q := u.Query()
		domain := q.Get("domain")
		post := q.Get("post")
		if domain == "" || post == "" {
			return parsedLink{}, fmt.Errorf("tg://resolve link missing domain or post")
		}
		id, err := strconv.Atoi(post)
		if err != nil || id <= 0 {
			return parsedLink{}, fmt.Errorf("invalid post id %q", post)
		}
		if err := validateUsername(domain); err != nil {
			return parsedLink{}, err
		}
		topicID := 0
		if thread := q.Get("thread"); thread != "" {
			tid, err := strconv.Atoi(thread)
			if err != nil || tid <= 0 {
				return parsedLink{}, fmt.Errorf("invalid thread id %q", thread)
			}
			topicID = tid
		}
		return newPublicParsedLink(domain, topicID, id), nil
	}

	// Path form: split and trim empty segments from both ends.
	segments := make([]string, 0, 4)
	for s := range strings.SplitSeq(u.Path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	if len(segments) == 0 {
		return parsedLink{}, fmt.Errorf("link has no path segments")
	}

	// Invite links look like t.me/joinchat/<hash> or t.me/+<hash>. These
	// aren't messages — reject with a targeted error so the model knows
	// to use a different tool.
	switch {
	case segments[0] == "joinchat":
		return parsedLink{}, fmt.Errorf("invite link (joinchat) is not a message link")
	case strings.HasPrefix(segments[0], "+"):
		return parsedLink{}, fmt.Errorf("invite link (+ prefix) is not a message link")
	case segments[0] == "addstickers", segments[0] == "share", segments[0] == "iv", segments[0] == "proxy", segments[0] == "socks":
		return parsedLink{}, fmt.Errorf("reserved path %q is not a message link", segments[0])
	}

	// Private-channel form: /c/<internal>/<msg_id> or /c/<internal>/<topic>/<msg_id>.
	if segments[0] == "c" {
		if len(segments) < 3 {
			return parsedLink{}, fmt.Errorf("private channel link missing message id")
		}
		channelRaw, err := strconv.ParseInt(segments[1], 10, 64)
		if err != nil || channelRaw <= 0 {
			return parsedLink{}, fmt.Errorf("invalid channel id %q", segments[1])
		}
		switch len(segments) {
		case 3:
			id, err := strconv.Atoi(segments[2])
			if err != nil || id <= 0 {
				return parsedLink{}, fmt.Errorf("invalid message id %q", segments[2])
			}
			return newPrivateParsedLink(channelRaw, 0, id), nil
		case 4:
			tid, err := strconv.Atoi(segments[2])
			if err != nil || tid <= 0 {
				return parsedLink{}, fmt.Errorf("invalid topic id %q", segments[2])
			}
			id, err := strconv.Atoi(segments[3])
			if err != nil || id <= 0 {
				return parsedLink{}, fmt.Errorf("invalid message id %q", segments[3])
			}
			return newPrivateParsedLink(channelRaw, tid, id), nil
		default:
			return parsedLink{}, fmt.Errorf("private channel link has too many segments")
		}
	}

	// Public username form: /<username>/<msg_id> or /<username>/<topic>/<msg_id>.
	if len(segments) < 2 {
		return parsedLink{}, fmt.Errorf("link missing message id (expected /<username>/<id>)")
	}
	if err := validateUsername(segments[0]); err != nil {
		return parsedLink{}, err
	}
	switch len(segments) {
	case 2:
		id, err := strconv.Atoi(segments[1])
		if err != nil || id <= 0 {
			return parsedLink{}, fmt.Errorf("invalid message id %q", segments[1])
		}
		return newPublicParsedLink(segments[0], 0, id), nil
	case 3:
		tid, err := strconv.Atoi(segments[1])
		if err != nil || tid <= 0 {
			return parsedLink{}, fmt.Errorf("invalid topic id %q", segments[1])
		}
		id, err := strconv.Atoi(segments[2])
		if err != nil || id <= 0 {
			return parsedLink{}, fmt.Errorf("invalid message id %q", segments[2])
		}
		return newPublicParsedLink(segments[0], tid, id), nil
	default:
		return parsedLink{}, fmt.Errorf("link has too many path segments")
	}
}

// validateUsername enforces Telegram's public username rules at parse time:
// 5–32 chars, alphanumeric + underscores, must start with a letter. Catches
// obviously-wrong path segments (e.g. "joinchat" slipped through, or a
// numeric segment where a username was expected) before we hit the network.
func validateUsername(s string) error {
	if len(s) < 5 || len(s) > 32 {
		return fmt.Errorf("username %q has invalid length (expected 5–32 chars)", s)
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
			if i == 0 {
				return fmt.Errorf("username %q must start with a letter", s)
			}
		case r == '_':
			if i == 0 {
				return fmt.Errorf("username %q must start with a letter", s)
			}
		default:
			return fmt.Errorf("username %q contains invalid character %q", s, r)
		}
	}
	return nil
}

// chatIDFromResolved extracts the user-facing chat ID and display title from
// a ContactsResolveUsername response, converting channel/supergroup IDs to
// the -100-prefixed signed form used elsewhere in the toolbox.
func chatIDFromResolved(r *tg.ContactsResolvedPeer) (int64, string, bool) {
	for _, c := range r.Chats {
		switch chat := c.(type) {
		case *tg.Chat:
			return chat.ID, chat.Title, true
		case *tg.Channel:
			return -(tgclient.ChannelIDPrefix + chat.ID), chat.Title, true
		}
	}
	for _, u := range r.Users {
		if user, ok := u.(*tg.User); ok {
			name := strings.TrimSpace(user.FirstName + " " + user.LastName)
			return user.ID, name, true
		}
	}
	return 0, "", false
}
