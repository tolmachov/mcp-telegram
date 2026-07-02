package summarize

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const anthropicAPIURL = "https://api.anthropic.com/v1/messages"

// AnthropicProvider implements Provider using Anthropic API.
type AnthropicProvider struct {
	apiKey string
	model  string
	client *http.Client
}

// NewAnthropicProvider creates a new AnthropicProvider.
func NewAnthropicProvider(apiKey, model string) *AnthropicProvider {
	if model == "" {
		model = "claude-haiku-4-5-20251001"
	}
	return &AnthropicProvider{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{
			Timeout: 3 * time.Minute,
		},
	}
}

type anthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens"`
	Messages  []anthropicMessage `json:"messages"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type anthropicResponse struct {
	Content []anthropicContent `json:"content"`
	Error   *anthropicError    `json:"error,omitempty"`
}

type anthropicContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

func (e *anthropicError) Error() string {
	return fmt.Sprintf("%s (%s)", e.Message, e.Type)
}

// Summarize sends a prompt to Anthropic and returns the response.
func (p *AnthropicProvider) Summarize(ctx context.Context, prompt string) (string, error) {
	reqBody := anthropicRequest{
		Model:     p.model,
		MaxTokens: 4096,
		Messages: []anthropicMessage{
			{Role: "user", Content: prompt},
		},
	}

	var anthropicResp anthropicResponse
	if err := postJSON(ctx, p.client, "anthropic", anthropicAPIURL, map[string]string{
		"x-api-key":         p.apiKey,
		"anthropic-version": "2023-06-01",
	}, reqBody, &anthropicResp); err != nil {
		return "", err
	}

	if anthropicResp.Error != nil {
		return "", fmt.Errorf("anthropic: %w", anthropicResp.Error)
	}

	if len(anthropicResp.Content) == 0 {
		return "", fmt.Errorf("no content in response")
	}

	var textParts []string
	for _, content := range anthropicResp.Content {
		if content.Type != "text" {
			continue
		}
		text := strings.TrimSpace(content.Text)
		if text == "" {
			continue
		}
		textParts = append(textParts, text)
	}

	if len(textParts) == 0 {
		return "", fmt.Errorf("no text content in response")
	}

	return strings.Join(textParts, "\n\n"), nil
}
