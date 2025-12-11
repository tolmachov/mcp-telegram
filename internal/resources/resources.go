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

// ResourceTemplateHandler defines the interface for dynamic resource template handlers
type ResourceTemplateHandler interface {
	Template() mcp.ResourceTemplate
	Handle(ctx context.Context, request mcp.ReadResourceRequest) ([]mcp.ResourceContents, error)
}

// RegisterResources registers all resource handlers with the MCP server
func RegisterResources(s *server.MCPServer, resources []ResourceHandler, templates []ResourceTemplateHandler) {
	for _, r := range resources {
		s.AddResource(r.Resource(), r.Handle)
	}
	for _, t := range templates {
		s.AddResourceTemplate(t.Template(), t.Handle)
	}
}
