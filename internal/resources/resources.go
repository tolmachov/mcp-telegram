package resources

import (
	"context"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// ResourceHandler defines the interface for static resource handlers
type ResourceHandler interface {
	Resource() mcp.Resource
	Handle(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error)
}

// RegisterResources registers all resource handlers with the MCP server
func RegisterResources(s *server.MCPServer, handlers []ResourceHandler) {
	for _, r := range handlers {
		s.AddResource(r.Resource(), r.Handle)
	}
}
