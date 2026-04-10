package summarize

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// geminiAPIURLFormat is the REST endpoint for generateContent, parameterised
// by model name. The API key is sent via the x-goog-api-key header instead
// of the ?key= query parameter so it never ends up in URLs that leak into
// *url.Error messages, HTTP trace logs, or upstream proxy access logs.
const geminiAPIURLFormat = "https://generativelanguage.googleapis.com/v1beta/models/%s:generateContent"

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

func (e *geminiError) Error() string {
	return fmt.Sprintf("%s (code: %d)", e.Message, e.Code)
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

	url := fmt.Sprintf(geminiAPIURLFormat, p.model)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-goog-api-key", p.apiKey)

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("sending request: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			slog.Debug("gemini: response body close failed", "err", err)
		}
	}()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("gemini returned status %d: %s", resp.StatusCode, errBodySnippet(respBody))
	}

	var geminiResp geminiResponse
	if err := json.Unmarshal(respBody, &geminiResp); err != nil {
		return "", fmt.Errorf("unmarshaling response: %w", err)
	}

	if geminiResp.Error != nil {
		return "", fmt.Errorf("gemini: %w", geminiResp.Error)
	}

	if len(geminiResp.Candidates) == 0 {
		return "", fmt.Errorf("no candidates in response")
	}

	if len(geminiResp.Candidates[0].Content.Parts) == 0 {
		return "", fmt.Errorf("no parts in response content")
	}

	var textParts []string
	for _, part := range geminiResp.Candidates[0].Content.Parts {
		text := strings.TrimSpace(part.Text)
		if text == "" {
			continue
		}
		textParts = append(textParts, text)
	}
	if len(textParts) == 0 {
		return "", fmt.Errorf("no text parts in response content")
	}

	return strings.Join(textParts, "\n\n"), nil
}
