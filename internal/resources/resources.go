package resources

import (
	"encoding/json"
	"fmt"

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

// jsonResource renders v as the indented JSON contents of the resource at uri.
func jsonResource(uri string, v any) (*mcp.ReadResourceResult, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshaling %s: %w", uri, err)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: "application/json",
			Text:     string(data),
		}},
	}, nil
}
