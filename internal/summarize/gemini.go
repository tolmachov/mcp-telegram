package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

const geminiAPIURL = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent?key=%s"

// GeminiProvider implements Provider using Google Gemini API.
type GeminiProvider struct {
	apiKey string
	model  string
	client *http.Client
}

// NewGeminiProvider creates a new GeminiProvider.
func NewGeminiProvider(apiKey, model string) *GeminiProvider {
	if model == "" {
		model = "gemini-2.0-flash"
	}
	return &GeminiProvider{
		apiKey: apiKey,
		model:  model,
		client: &http.Client{
			Timeout: 10 * time.Minute,
		},
	}
}

type geminiRequest struct {
	Contents []geminiContent `json:"contents"`
}

type geminiContent struct {
	Parts []geminiPart `json:"parts"`
}

type geminiPart struct {
	Text string `json:"text"`
}

type geminiResponse struct {
	Candidates []geminiCandidate `json:"candidates"`
	Error      *geminiError      `json:"error,omitempty"`
}

type geminiCandidate struct {
	Content geminiContent `json:"content"`
}

type geminiError struct {
	Message string `json:"message"`
	Code    int    `json:"code"`
}

// Summarize sends a prompt to Gemini and returns the response.
func (p *GeminiProvider) Summarize(ctx context.Context, prompt string) (string, error) {
	reqBody := geminiRequest{
		Contents: []geminiContent{
			{
				Parts: []geminiPart{
					{Text: prompt},
				},
			},
		},
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	url := fmt.Sprintf(geminiAPIURL, p.model, p.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var geminiResp geminiResponse
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		return "", fmt.Errorf("unmarshaling response: %w", err)
	}

	if geminiResp.Error != nil {
		return "", fmt.Errorf("gemini error: %s (code: %d)", geminiResp.Error.Message, geminiResp.Error.Code)
	}

	if len(geminiResp.Candidates) == 0 {
		return "", fmt.Errorf("no candidates in response")
	}

	if len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("no parts in response content")
	}

	return geminiResp.Candidates[0].Content.Parts[0].Text, nil
}
