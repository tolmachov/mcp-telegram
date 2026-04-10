package resources

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ResourceHandler is the interface every static resource handler implements.
// Each handler registers itself directly with the server, choosing whether
// to expose a single resource (most common) or multiple via templates.
type ResourceHandler interface {
	Register(s *mcp.Server)
}

// RegisterResources registers all resource handlers with the MCP server.
func RegisterResources(s *mcp.Server, handlers []ResourceHandler) {
	for _, r := range handlers {
		r.Register(s)
	}
}
