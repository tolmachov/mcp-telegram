package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/yosida95/uritemplate/v3"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// chatInfoTemplate parses telegram://chats/{chat_id}/info URIs to extract the
// chat ID. The official Go SDK does not surface URI template variables to
// resource handlers (the handler receives only the raw URI), so we re-parse
// here using the same template library the SDK uses internally for matching.
// The path sits under the telegram://chats collection and mirrors the pinned
// message resources at telegram://chats/{id}/messages.
var chatInfoTemplate = uritemplate.MustNew("telegram://chats/{chat_id}/info")

// RegisterChatTemplate registers a URI template that exposes any chat as a
// browsable MCP resource at telegram://chats/{id}/info. Per MCP spec, resource
// templates let one registration serve many URIs — instead of registering one
// resource per chat, the host can construct any URI matching the template and
// the server fetches the data on demand.
//
// This complements the GetChatInfo tool: the tool is model-controlled (Claude
// decides when to call it), while the resource is application-controlled
// (the host UI can let users browse chats from a sidebar without Claude's
// involvement).
func RegisterChatTemplate(s *mcp.Server, client *tg.Client) {
	s.AddResourceTemplate(&mcp.ResourceTemplate{
		URITemplate: chatInfoTemplate.Raw(),
		Name:        "Telegram Chat Info",
		Description: "Information about a specific Telegram chat (title, type, member count, unread/mention/pin state). Use the numeric chat ID; for usernames, resolve via the ResolveUsername tool first.",
		MIMEType:    "application/json",
	}, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		chatID, err := parseChatInfoURI(request.Params.URI)
		if err != nil {
			return nil, err
		}

		info, err := tgdata.GetChatInfo(ctx, client, chatID)
		if err != nil {
			return nil, fmt.Errorf("getting chat info for %d: %w", chatID, err)
		}

		data, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("marshaling chat info: %w", err)
		}

		return &mcp.ReadResourceResult{
			Contents: []*mcp.ResourceContents{{
				URI:      request.Params.URI,
				MIMEType: "application/json",
				Text:     string(data),
			}},
		}, nil
	})
}

// parseChatInfoURI extracts the chat_id from a telegram://chats/{chat_id}/info URI.
func parseChatInfoURI(uri string) (int64, error) {
	match := chatInfoTemplate.Match(uri)
	if match == nil {
		return 0, fmt.Errorf("uri %q does not match telegram://chats/{chat_id}/info", uri)
	}
	chatIDStr := match.Get("chat_id").String()
	chatID, err := strconv.ParseInt(chatIDStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("chat_id %q is not a valid integer", chatIDStr)
	}
	return chatID, nil
}
