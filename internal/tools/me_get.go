package tools

import (
	"context"
	"fmt"

	"github.com/gotd/td/tg"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

// MeGetHandler handles the GetMe tool.
type MeGetHandler struct {
	client *tg.Client
}

// NewMeGetHandler creates a new MeGetHandler.
func NewMeGetHandler(client *tg.Client) *MeGetHandler {
	return &MeGetHandler{client: client}
}

// GetMeInput is the (empty) input for the GetMe tool.
type GetMeInput struct{}

// Register adds the tool to the MCP server using the typed mcp.AddTool helper.
// The output is the existing tgdata.UserInfo struct so the StructuredContent
// payload matches the JSON shape clients already see today.
func (h *MeGetHandler) Register(s *mcp.Server) {
	mcp.AddTool(s, &mcp.Tool{
		Name:        "GetMe",
		Description: "Get information about the currently authenticated Telegram user, including ID, name, username, and phone number.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptrTrue()},
	}, h.handle)
}

func (h *MeGetHandler) handle(ctx context.Context, _ *mcp.CallToolRequest, _ GetMeInput) (*mcp.CallToolResult, *tgdata.UserInfo, error) {
	info, err := tgdata.GetCurrentUser(ctx, h.client)
	if err != nil {
		return errResult(fmt.Sprintf("Failed to get current user: %v", err)), nil, nil
	}
	return nil, info, nil
}
