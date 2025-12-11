package summarize

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gotd/td/tg"

	"github.com/tolmachov/mcp-telegram/internal/tgdata"
)

const batchSize = 50

const promptTemplate = `You are summarizing a Telegram chat conversation.

User's goal for this summary:
%s

Current summary so far:
%s

New messages to incorporate:
%s

Instructions:
- Focus on information relevant to the user's goal
- Identify key topics and themes discussed
- Note important decisions or conclusions
- Highlight action items if any
- Keep the summary concise but comprehensive
- Write in the same language as the messages
- Output as plain text (markdown allowed)

Updated summary:`

// ProgressCallback is called with the current batch number, total batches, and a message.
type ProgressCallback func(current, total int, message string)

// Summarizer handles chat summarization using a Provider.
type Summarizer struct {
	provider    Provider
	client      *tg.Client
	batchTokens int
}

// NewSummarizer creates a new Summarizer.
func NewSummarizer(provider Provider, client *tg.Client, batchTokens int) *Summarizer {
	if batchTokens <= 0 {
		batchTokens = DefaultBatchTokens
	}
	return &Summarizer{
		provider:    provider,
		client:      client,
		batchTokens: batchTokens,
	}
}

// Summarize performs rolling summarization of a chat.
func (s *Summarizer) Summarize(ctx context.Context, chatID int64, goal string, since time.Time, onProgress ProgressCallback) (string, error) {
	// Fetch all messages since the given time
	messages, err := s.fetchMessagesSince(ctx, chatID, since)
	if err != nil {
		return "", fmt.Errorf("fetching messages: %w", err)
	}

	if len(messages) == 0 {
		return "No messages found in the specified period.", nil
	}

	// Filter text-only messages (ignore media-only)
	textMessages := filterTextMessages(messages)
	if len(textMessages) == 0 {
		return "No text messages found in the specified period.", nil
	}

	// Split into batches by token count
	batches := splitIntoBatchesByTokens(textMessages, s.batchTokens)
	totalBatches := len(batches)

	var runningSummary string

	for i, batch := range batches {
		if onProgress != nil {
			onProgress(i+1, totalBatches, fmt.Sprintf("Processing batch %d/%d", i+1, totalBatches))
		}

		formattedMessages := formatMessages(batch)
		prompt := fmt.Sprintf(promptTemplate, goal, runningSummary, formattedMessages)

		summary, err := s.summarizeWithProgress(ctx, prompt, i+1, totalBatches, onProgress)
		if err != nil {
			return "", fmt.Errorf("summarizing batch %d: %w", i+1, err)
		}

		runningSummary = strings.TrimSpace(summary)
	}

	return runningSummary, nil
}

func (s *Summarizer) fetchMessagesSince(ctx context.Context, chatID int64, since time.Time) ([]tgdata.MessageInfo, error) {
	var allMessages []tgdata.MessageInfo
	offsetID := 0

	for {
		opts := tgdata.MessagesOptions{
			Limit:    batchSize,
			OffsetID: offsetID,
		}

		result, err := tgdata.GetMessages(ctx, s.client, chatID, opts)
		if err != nil {
			return nil, err
		}

		if len(result.Messages) == 0 {
			break
		}

		// Filter messages by date and collect
		for _, msg := range result.Messages {
			if msg.Date.Before(since) {
				// Messages are in reverse chronological order, so we can stop here
				goto done
			}
			allMessages = append(allMessages, msg)
		}

		if !result.HasMore {
			break
		}

		offsetID = result.NextID
	}

done:
	// Reverse to get chronological order
	for i, j := 0, len(allMessages)-1; i < j; i, j = i+1, j-1 {
		allMessages[i], allMessages[j] = allMessages[j], allMessages[i]
	}

	return allMessages, nil
}

func filterTextMessages(messages []tgdata.MessageInfo) []tgdata.MessageInfo {
	var result []tgdata.MessageInfo
	for _, msg := range messages {
		if msg.Text != "" {
			result = append(result, msg)
		}
	}
	return result
}

// estimateTokens provides a rough token estimate for text.
// Uses the common approximation of ~4 characters per token for English
// but adjusts for other languages that may have different ratios.
func estimateTokens(text string) int {
	// Rough approximation: ~4 chars per token for English
	// For non-ASCII text (like Cyrillic, CJK), tokens can be ~1-2 chars
	charCount := len(text)
	runeCount := utf8.RuneCountInString(text)

	// If there are many multi-byte characters, use a lower ratio
	if charCount > runeCount*2 {
		return runeCount / 2
	}
	return charCount / 4
}

// splitIntoBatchesByTokens splits messages into batches where each batch
// contains approximately maxTokens tokens.
func splitIntoBatchesByTokens(messages []tgdata.MessageInfo, maxTokens int) [][]tgdata.MessageInfo {
	if len(messages) == 0 {
		return nil
	}

	var batches [][]tgdata.MessageInfo
	var currentBatch []tgdata.MessageInfo
	currentTokens := 0

	for _, msg := range messages {
		// Estimate tokens for this message including formatting overhead
		// Format: "[2006-01-02 15:04] <sender_id>: <text>\n"
		msgText := fmt.Sprintf("[%s] %d: %s\n", msg.Date.Format("2006-01-02 15:04"), msg.SenderID, msg.Text)
		msgTokens := estimateTokens(msgText)

		// If adding this message exceeds the limit, start a new batch
		// But always include at least one message per batch
		if currentTokens+msgTokens > maxTokens && len(currentBatch) > 0 {
			batches = append(batches, currentBatch)
			currentBatch = nil
			currentTokens = 0
		}

		currentBatch = append(currentBatch, msg)
		currentTokens += msgTokens
	}

	// Remember the last batch
	if len(currentBatch) > 0 {
		batches = append(batches, currentBatch)
	}

	return batches
}

func formatMessages(messages []tgdata.MessageInfo) string {
	var sb strings.Builder
	for _, msg := range messages {
		timestamp := msg.Date.Format("2006-01-02 15:04")
		sb.WriteString(fmt.Sprintf("[%s] %d: %s\n", timestamp, msg.SenderID, msg.Text))
	}
	return sb.String()
}

const progressInterval = 5 * time.Second

// summarizeWithProgress calls the provider and sends periodic progress updates
// to prevent client timeout during long LLM calls.
func (s *Summarizer) summarizeWithProgress(ctx context.Context, prompt string, currentBatch, totalBatches int, onProgress ProgressCallback) (string, error) {
	type result struct {
		summary string
		err     error
	}

	resultCh := make(chan result, 1)

	go func() {
		summary, err := s.provider.Summarize(ctx, prompt)
		resultCh <- result{summary: summary, err: err}
	}()

	ticker := time.NewTicker(progressInterval)
	defer ticker.Stop()

	elapsed := 0
	for {
		select {
		case res := <-resultCh:
			return res.summary, res.err
		case <-ticker.C:
			elapsed += int(progressInterval.Seconds())
			if onProgress != nil {
				onProgress(currentBatch, totalBatches, fmt.Sprintf("Processing batch %d/%d (%ds elapsed)", currentBatch, totalBatches, elapsed))
			}
		case <-ctx.Done():
			return "", fmt.Errorf("summarization canceled: %w", ctx.Err())
		}
	}
}
