package tools

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// Handler defines the interface for MCP tool handlers
type Handler interface {
	Tool() mcp.Tool
	Handle(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error)
}

// RegisterTools registers all handlers with the MCP server
func RegisterTools(s *server.MCPServer, handlers []Handler) {
	for _, h := range handlers {
		s.AddTool(h.Tool(), h.Handle)
	}
}

// truncateRunes truncates string to n runes without allocating a full []rune slice.
// If the string is longer than n runes, it returns the first n runes followed by "...".
func truncateRunes(s string, n int) string {
	i := 0
	for j := range s {
		if i == n {
			return s[:j] + "..."
		}
		i++
	}
	return s
}
