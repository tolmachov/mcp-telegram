package summarize

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// SamplingProvider implements Provider using MCP Sampling.
type SamplingProvider struct {
	mcpServer *server.MCPServer
}

// NewSamplingProvider creates a new SamplingProvider.
func NewSamplingProvider(mcpServer *server.MCPServer) *SamplingProvider {
	return &SamplingProvider{mcpServer: mcpServer}
}

// Summarize sends a prompt via MCP Sampling and returns the response.
func (p *SamplingProvider) Summarize(ctx context.Context, prompt string) (string, error) {
	samplingRequest := mcp.CreateMessageRequest{
		CreateMessageParams: mcp.CreateMessageParams{
			Messages: []mcp.SamplingMessage{
				{
					Role: mcp.RoleUser,
					Content: mcp.TextContent{
						Type: "text",
						Text: prompt,
					},
				},
			},
			MaxTokens: 2000,
		},
	}

	result, err := p.mcpServer.RequestSampling(ctx, samplingRequest)
	if err != nil {
		return "", fmt.Errorf("requesting sampling: %w", err)
	}

	return getTextFromContent(result.Content), nil
}

func getTextFromContent(content any) string {
	switch c := content.(type) {
	case mcp.TextContent:
		return c.Text
	case string:
		return c
	default:
		return fmt.Sprintf("%v", content)
	}
}
