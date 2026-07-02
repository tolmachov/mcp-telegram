package summarize

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// SamplingProvider implements Provider using MCP Sampling. Each instance is
// bound to a single MCP server session captured at request time, since
// sampling is a per-session operation in the official SDK.
type SamplingProvider struct {
	session *mcp.ServerSession
}

// NewSamplingProvider creates a new SamplingProvider bound to the given session.
// Pass the session that's active for the current tool call (req.Session inside
// a CallToolRequest handler). Pass nil for non-MCP contexts (tests, direct CLI).
func NewSamplingProvider(session *mcp.ServerSession) *SamplingProvider {
	return &SamplingProvider{session: session}
}

// ErrSamplingUnsupported is returned when the connected MCP client did not
// declare the sampling capability during initialization. Callers should surface
// this clearly so the user can switch to a non-sampling provider (anthropic,
// gemini, ollama) via the --summarize-provider flag.
var ErrSamplingUnsupported = errors.New("client does not support MCP sampling; configure --summarize-provider=anthropic|gemini|ollama (and the matching API key) to use a direct LLM provider instead")

// Summarize sends a prompt via MCP Sampling and returns the response.
//
// Spec compliance: per MCP capability negotiation rules, the client must
// advertise `sampling` during initialize. We check the capability up front and
// return ErrSamplingUnsupported with actionable guidance if it's missing,
// rather than letting the call fail with a generic transport error.
func (p *SamplingProvider) Summarize(ctx context.Context, prompt string) (string, error) {
	if p.session == nil {
		return "", ErrSamplingUnsupported
	}
	iparams := p.session.InitializeParams()
	if iparams == nil || iparams.Capabilities == nil || iparams.Capabilities.Sampling == nil {
		return "", ErrSamplingUnsupported
	}

	result, err := p.session.CreateMessage(ctx, &mcp.CreateMessageParams{
		Messages: []*mcp.SamplingMessage{
			{
				Role:    "user",
				Content: &mcp.TextContent{Text: prompt},
			},
		},
		MaxTokens: 2000,
	})
	if err != nil {
		return "", fmt.Errorf("requesting sampling: %w", err)
	}

	text := contentText(result.Content)
	if text == "" {
		// Non-text content (image/audio) or an empty reply. Returning ("", nil)
		// would let the rolling summarizer silently drop the accumulated summary
		// for this batch; fail loudly so the caller can surface partial work.
		return "", fmt.Errorf("sampling returned no text content")
	}
	return text, nil
}

// contentText extracts the textual payload from a Content interface, returning
// the empty string for non-text content kinds.
func contentText(c mcp.Content) string {
	if tc, ok := c.(*mcp.TextContent); ok {
		return tc.Text
	}
	return ""
}
