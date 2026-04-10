// Package prompts registers MCP prompts that guide multi-step Telegram workflows.
//
// Prompts are short, parameterized recipes that an MCP client can render as a
// slash-command or quick action. Each prompt expands into a user message that
// instructs the assistant which mcp-telegram tools to call and in what order.
package prompts

import (
	"context"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Register adds all mcp-telegram prompts to the given MCP server.
func Register(s *mcp.Server) {
	s.AddPrompt(&mcp.Prompt{
		Name:        "daily-digest",
		Description: "Summarize what happened across all chats over a recent period (default: today). Walks unread/active chats and produces a per-chat digest.",
		Arguments: []*mcp.PromptArgument{
			{Name: "period", Description: "Time period: 'day' (default), 'week', or 'month'"},
		},
	}, handleDailyDigest)

	s.AddPrompt(&mcp.Prompt{
		Name:        "chat-catchup",
		Description: "Catch up on what was missed in a specific chat. Wraps SummarizeChat with sensible defaults focused on key decisions and action items.",
		Arguments: []*mcp.PromptArgument{
			{Name: "chat", Description: "Chat ID, @username, or chat title to catch up on", Required: true},
			{Name: "period", Description: "Time period: 'day', 'week' (default), or 'month'"},
		},
	}, handleChatCatchup)

	s.AddPrompt(&mcp.Prompt{
		Name:        "find-and-reply",
		Description: "Find a specific message in a chat by content/sender and draft a reply to it. Requires user confirmation before sending.",
		Arguments: []*mcp.PromptArgument{
			{Name: "chat", Description: "Chat ID, @username, or chat title to search in", Required: true},
			{Name: "query", Description: "What to look for (e.g. 'message from Alice about deadline')", Required: true},
			{Name: "reply", Description: "Reply text or instruction (e.g. 'agree and propose Friday 3pm')", Required: true},
		},
	}, handleFindAndReply)
}

func handleDailyDigest(_ context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	period := request.Params.Arguments["period"]
	if period == "" {
		period = "day"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Build a digest of Telegram activity over the last %s:\n", period)
	b.WriteString("1. Use GetChats to list active chats; focus on chats with unread messages or recent activity.\n")
	b.WriteString("2. For each chat that looks relevant, call SummarizeChat with goal='key updates and action items' ")
	fmt.Fprintf(&b, "and period='%s'.\n", period)
	b.WriteString("3. Group the per-chat summaries into a single digest, ordered by importance (chats with mentions or unread > 0 first).\n")
	b.WriteString("4. End with a short bullet list of action items the user should follow up on.\n")
	b.WriteString("Do NOT send any messages — this is read-only.")
	return promptUser(b.String()), nil
}

func handleChatCatchup(_ context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	chat := request.Params.Arguments["chat"]
	if chat == "" {
		return nil, fmt.Errorf("argument 'chat' is required")
	}
	period := request.Params.Arguments["period"]
	if period == "" {
		period = "week"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Catch me up on chat %q (last %s):\n", chat, period)
	b.WriteString("1. Resolve the chat ID:\n")
	b.WriteString("   - If it looks like a username (starts with @), use ResolveUsername.\n")
	b.WriteString("   - Otherwise, use SearchChats to find the chat by title.\n")
	b.WriteString("   - If multiple matches, ask me which one before proceeding.\n")
	fmt.Fprintf(&b, "2. Call SummarizeChat with goal='key decisions, action items, and anything I should respond to' and period='%s'.\n", period)
	b.WriteString("3. After the summary, list any messages that look like they need a reply, with the message ID and a one-line excerpt.\n")
	b.WriteString("Do NOT send any replies yet — wait for my instruction.")
	return promptUser(b.String()), nil
}

func handleFindAndReply(_ context.Context, request *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
	chat := request.Params.Arguments["chat"]
	if chat == "" {
		return nil, fmt.Errorf("argument 'chat' is required")
	}
	query := request.Params.Arguments["query"]
	if query == "" {
		return nil, fmt.Errorf("argument 'query' is required")
	}
	reply := request.Params.Arguments["reply"]
	if reply == "" {
		return nil, fmt.Errorf("argument 'reply' is required")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Find a message in chat %q matching: %s\n", chat, query)
	b.WriteString("Then draft a reply: ")
	b.WriteString(reply)
	b.WriteString("\n\nSteps:\n")
	b.WriteString("1. Resolve the chat ID via ResolveUsername (if @username) or SearchChats (if title).\n")
	b.WriteString("2. Use GetMessages to fetch recent messages (start with limit=50). If you don't find a match, paginate with offset_id.\n")
	b.WriteString("3. Pick the message that best matches the query. If ambiguous, show me the top 3 candidates with their IDs and ask which one.\n")
	b.WriteString("4. Once confirmed, use SendMessage with mode=\"draft\" and reply_to_message_id set to the message's opaque handle, and show me the draft.\n")
	b.WriteString("5. ONLY after I explicitly approve, call SendMessage with mode=\"send\" (the default) and the same reply_to_message_id. Never send without confirmation.")
	return promptUser(b.String()), nil
}

func promptUser(text string) *mcp.GetPromptResult {
	return &mcp.GetPromptResult{
		Messages: []*mcp.PromptMessage{
			{Role: "user", Content: &mcp.TextContent{Text: text}},
		},
	}
}
