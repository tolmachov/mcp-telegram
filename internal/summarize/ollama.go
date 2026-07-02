package summarize

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

// OllamaProvider implements Provider using Ollama API.
type OllamaProvider struct {
	baseURL string
	model   string
	client  *http.Client
}

// NewOllamaProvider creates a new OllamaProvider.
func NewOllamaProvider(baseURL, model string) *OllamaProvider {
	return &OllamaProvider{
		baseURL: baseURL,
		model:   model,
		client: &http.Client{
			Timeout: 10 * time.Minute, // LLM inference can take a long time
		},
	}
}

type ollamaRequest struct {
	Model  string `json:"model"`
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type ollamaResponse struct {
	Response string `json:"response"`
	Done     bool   `json:"done"`
	Error    string `json:"error,omitempty"`
}

// Summarize sends a prompt to Ollama and returns the response.
func (p *OllamaProvider) Summarize(ctx context.Context, prompt string) (string, error) {
	reqBody := ollamaRequest{
		Model:  p.model,
		Prompt: prompt,
		Stream: false,
	}

	var ollamaResp ollamaResponse
	if err := postJSON(ctx, p.client, "ollama", p.baseURL+"/api/generate", nil, reqBody, &ollamaResp); err != nil {
		return "", err
	}

	if ollamaResp.Error != "" {
		return "", fmt.Errorf("ollama error: %s", ollamaResp.Error)
	}

	return ollamaResp.Response, nil
}
