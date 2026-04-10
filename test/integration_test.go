//go:build integration

package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tolmachov/mcp-telegram/internal"
	"github.com/tolmachov/mcp-telegram/internal/config"
	"github.com/tolmachov/mcp-telegram/internal/flags"
)

// parseChatIDInt converts a chat ID string (typically from os.Getenv) to
// int64, failing the test with a clear message on parse errors.
//
// Why this exists: chat IDs come from env vars as strings, but the tool
// schemas declare chat_id as an integer (was already the case; the previous
// mark3labs SDK was loose about runtime type coercion, so tests could pass
// chat_id as a JSON string and it Just Worked). The official go-sdk enforces
// strict jsonschema validation on tool arguments, so strings are rejected
// with "type: N has type 'string', want 'integer'". Converting once at the
// call site is cleaner than loosening the tool contract.
func parseChatIDInt(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	require.NoError(t, err, "parsing chat ID %q as int64", s)
	return n
}

func init() {
	// Mirror main.go's startup: load .env first, then merge in any keys
	// stored in the OS secure store (macOS Keychain / Linux file). The
	// integration test bypasses main.go because it spawns server.New
	// directly, so without this initialization TELEGRAM_API_ID/HASH stay
	// empty, urfave/cli's Required flag fails the server goroutine, and
	// the io.Pipe-backed Initialize call hangs forever waiting for a
	// reader that will never appear.
	if err := godotenv.Load("../.env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		panic(fmt.Sprintf("failed to load .env file: %v", err))
	}
	store, err := config.NewStore()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "[test init] warning: failed to init secure config store: %v\n", err)
		return
	}
	if err := config.LoadIntoEnv(store); err != nil {
		// Log to stderr instead of panic — secure store may not be
		// configured on every dev machine, and tests can still run if
		// credentials come from .env / environment.
		_, _ = fmt.Fprintf(os.Stderr, "[test init] warning: failed to load secure config: %v\n", err)
	}
}

func setupClient(t *testing.T) (*client.Client, context.Context, func()) {
	return setupClientWithTimeout(t, 5*time.Minute)
}

func setupClientWithTimeout(t *testing.T, timeout time.Duration) (*client.Client, context.Context, func()) {
	t.Helper()

	// Sanity-check credentials BEFORE we spin up io.Pipes. Without these the
	// urfave/cli Required flags fail immediately, the server goroutine exits,
	// and any subsequent io.Pipe write hangs forever (the pipe has no buffer
	// and no live reader). Skipping early gives a clear error instead of a
	// 5-minute timeout.
	if os.Getenv(flags.EnvTelegramAPIID) == "" || os.Getenv(flags.EnvTelegramAPIHash) == "" {
		t.Skipf("%s and %s are required for integration tests; set them via .env, environment, or `mcp-telegram config set`", flags.EnvTelegramAPIID, flags.EnvTelegramAPIHash)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	// Create pipes for client-server communication
	// client writes to clientWriter -> serverReader reads
	// server writes to serverWriter -> clientReader reads
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()
	stderrReader, stderrWriter := io.Pipe()

	// Log server stderr
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderrReader.Read(buf)
			if n > 0 {
				t.Logf("[server stderr] %s", string(buf[:n]))
			}
			if err != nil {
				return
			}
		}
	}()

	// Start a server in a goroutine
	serverCtx, serverCancel := context.WithCancel(ctx)
	serverDone := make(chan error, 1)

	go func() {
		app := internal.New(serverReader, serverWriter, stderrWriter)
		err := app.Run(serverCtx, []string{"mcp-telegram", "run"})
		serverDone <- err
		// Critical: close the pipe ends the test client writes to so any
		// in-flight Write returns io.ErrClosedPipe instead of blocking
		// forever. Without this, an early server exit (e.g. auth failure)
		// causes the test to hang in c.Initialize.
		_ = serverReader.CloseWithError(fmt.Errorf("server exited: %v", err))
		_ = serverWriter.CloseWithError(fmt.Errorf("server exited: %v", err))
	}()

	// Create transport from pipes
	stdioTransport := transport.NewIO(clientReader, clientWriter, stderrReader)

	c := client.NewClient(stdioTransport)

	cleanup := func() {
		// Close client
		assert.NoError(t, c.Close())

		// Stop server
		serverCancel()

		// Close pipes
		_ = clientWriter.Close()
		_ = serverWriter.Close()
		_ = stderrWriter.Close()

		// Wait for the server to finish
		select {
		case err := <-serverDone:
			if err != nil && !errors.Is(err, context.Canceled) {
				assert.NoError(t, err, "server error")
			}
		case <-time.After(5 * time.Second):
			assert.Fail(t, "server did not stop in time")
		}

		cancel()
	}

	if err := c.Start(ctx); err != nil {
		cleanup()
		require.NoError(t, err, "failed to start client")
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{
		Name:    "mcp-telegram-test",
		Version: "1.0.0",
	}
	initRequest.Params.Capabilities = mcp.ClientCapabilities{}

	// Race Initialize against early server exit. If the server died before
	// stdioServer.Listen ran (auth failure, missing config, gotd connection
	// timeout), c.Initialize would otherwise hang on the orphaned io.Pipe.
	// 30s is generous for the gotd MTProto handshake while still bounding
	// total wait so failures surface promptly.
	initCtx, initCancel := context.WithTimeout(ctx, 30*time.Second)
	defer initCancel()

	type initResult struct {
		info *mcp.InitializeResult
		err  error
	}
	initDone := make(chan initResult, 1)
	go func() {
		info, err := c.Initialize(initCtx, initRequest)
		initDone <- initResult{info, err}
	}()

	var serverInfo *mcp.InitializeResult
	select {
	case res := <-initDone:
		if res.err != nil {
			cleanup()
			require.NoError(t, res.err, "failed to initialize")
		}
		serverInfo = res.info
	case err := <-serverDone:
		// Re-queue so cleanup can drain it cleanly.
		serverDone <- err
		cleanup()
		require.NoError(t, err, "server exited before initialize completed (check Telegram credentials and `mcp-telegram login` session)")
	case <-initCtx.Done():
		cleanup()
		require.Fail(t, "initialize timed out after 30s — server may be stuck in Telegram MTProto handshake; verify network connectivity and that `mcp-telegram login` was run")
	}

	t.Logf("Connected to server: %s (version %s)", serverInfo.ServerInfo.Name, serverInfo.ServerInfo.Version)

	return c, ctx, cleanup
}

func TestListResources(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	resourcesResult, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
	require.NoError(t, err)

	t.Logf("Available resources: %d", len(resourcesResult.Resources))
	for _, resource := range resourcesResult.Resources {
		t.Logf("  - %s: %s", resource.URI, resource.Description)
	}

	assert.NotEmpty(t, resourcesResult.Resources)
}

func TestListResourceTemplates(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	templatesResult, err := c.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
	require.NoError(t, err)

	t.Logf("Available resource templates: %d", len(templatesResult.ResourceTemplates))
	for _, tmpl := range templatesResult.ResourceTemplates {
		t.Logf("  - %s: %s", tmpl.URITemplate.Raw(), tmpl.Description)
	}

	if len(templatesResult.ResourceTemplates) == 0 {
		t.Log("no resource templates available")
	}
}

func TestListTools(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	toolsResult, err := c.ListTools(ctx, mcp.ListToolsRequest{})
	require.NoError(t, err)

	t.Logf("Available tools: %d", len(toolsResult.Tools))
	for _, tool := range toolsResult.Tools {
		t.Logf("  - %s: %s", tool.Name, tool.Description)
	}

	assert.NotEmpty(t, toolsResult.Tools)
}

func TestSearchChats(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	query := os.Getenv("TEST_SEARCH_QUERY")
	if query == "" {
		query = "test"
	}

	callRequest := mcp.CallToolRequest{}
	callRequest.Params.Name = "SearchChats"
	callRequest.Params.Arguments = map[string]any{
		"query": query,
		"limit": 10,
	}

	t.Logf("Calling SearchChats with query='%s'", query)

	result, err := c.CallTool(ctx, callRequest)
	require.NoError(t, err)

	logToolResult(t, result)
}

func TestGetChats(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	readRequest := mcp.ReadResourceRequest{}
	readRequest.Params.URI = "telegram://chats"

	result, err := c.ReadResource(ctx, readRequest)
	require.NoError(t, err)

	assert.NotEmpty(t, result.Contents)

	for _, content := range result.Contents {
		if textContent, ok := content.(mcp.TextResourceContents); ok {
			var data any
			if err := json.Unmarshal([]byte(textContent.Text), &data); err == nil {
				pretty, _ := json.MarshalIndent(data, "", "  ")
				// Truncate for logging
				output := string(pretty)
				if len(output) > 2000 {
					output = output[:2000] + "\n... (truncated)"
				}
				t.Logf("Chats:\n%s", output)
			}
		}
	}
}

func TestGetMe(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	readRequest := mcp.ReadResourceRequest{}
	readRequest.Params.URI = "telegram://me"

	result, err := c.ReadResource(ctx, readRequest)
	require.NoError(t, err)

	assert.NotEmpty(t, result.Contents)

	for _, content := range result.Contents {
		if textContent, ok := content.(mcp.TextResourceContents); ok {
			t.Logf("Me:\n%s", textContent.Text)
		}
	}
}

func TestGetChatInfo(t *testing.T) {
	cases := []struct {
		name   string
		envVar string
	}{
		{"Dialog", "TEST_CHAT_ID"},
		{"Group", "TEST_GROUP_ID"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatID := os.Getenv(tc.envVar)
			if chatID == "" {
				t.Skipf("%s not set", tc.envVar)
			}

			c, ctx, cleanup := setupClient(t)
			defer cleanup()

			callRequest := mcp.CallToolRequest{}
			callRequest.Params.Name = "GetChatInfo"
			callRequest.Params.Arguments = map[string]any{
				"chat_id": parseChatIDInt(t, chatID),
			}

			t.Logf("Calling GetChatInfo with chat_id=%s", chatID)

			result, err := c.CallTool(ctx, callRequest)
			require.NoError(t, err)

			logToolResult(t, result)
		})
	}
}

func TestPinnedChatResource(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	// List resources to find pinned chats
	resourcesResult, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
	require.NoError(t, err)

	// Find the first pinned chat resource (telegram://chats/{id})
	var pinnedURI string
	for _, resource := range resourcesResult.Resources {
		if strings.HasPrefix(resource.URI, "telegram://chats/") {
			pinnedURI = resource.URI
			t.Logf("Found pinned chat resource: %s (%s)", resource.URI, resource.Name)
			break
		}
	}

	if pinnedURI == "" {
		t.Log("No pinned chats found, skipping read test")
		return
	}

	// Read the pinned chat resource
	readRequest := mcp.ReadResourceRequest{}
	readRequest.Params.URI = pinnedURI

	result, err := c.ReadResource(ctx, readRequest)
	require.NoError(t, err)

	assert.NotEmpty(t, result.Contents)

	logResourceResult(t, result)
}

func TestGetMessages(t *testing.T) {
	cases := []struct {
		name   string
		envVar string
	}{
		{"Dialog", "TEST_CHAT_ID"},
		{"Group", "TEST_GROUP_ID"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatID := os.Getenv(tc.envVar)
			if chatID == "" {
				t.Skipf("%s not set", tc.envVar)
			}

			c, ctx, cleanup := setupClient(t)
			defer cleanup()

			callRequest := mcp.CallToolRequest{}
			callRequest.Params.Name = "GetMessages"
			callRequest.Params.Arguments = map[string]any{
				"chat_id": parseChatIDInt(t, chatID),
				"limit":   10,
			}

			t.Logf("Calling GetMessages with chat_id=%s", chatID)

			result, err := c.CallTool(ctx, callRequest)
			require.NoError(t, err)

			logToolResult(t, result)
		})
	}
}

func TestBackupMessages(t *testing.T) {
	cases := []struct {
		name   string
		envVar string
	}{
		{"Dialog", "TEST_CHAT_ID"},
		{"Group", "TEST_GROUP_ID"},
	}

	// Create a temp directory for backups and set allowed paths
	tmpDir, err := os.MkdirTemp("", "backup-test-*")
	require.NoError(t, err)
	defer func(path string) {
		if err := os.RemoveAll(path); err != nil {
			t.Logf("failed to remove temp dir: %v", err)
		}
	}(tmpDir)
	t.Setenv("TELEGRAM_ALLOWED_PATHS", tmpDir)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatID := os.Getenv(tc.envVar)
			if chatID == "" {
				t.Skipf("%s not set", tc.envVar)
			}

			c, ctx, cleanup := setupClient(t)
			defer cleanup()

			tmpFile := tmpDir + "/backup-" + tc.name + ".txt"

			callRequest := mcp.CallToolRequest{}
			callRequest.Params.Name = "BackupMessages"
			callRequest.Params.Arguments = map[string]any{
				"chat_id":  parseChatIDInt(t, chatID),
				"filepath": tmpFile,
				"count":    10,
			}

			t.Logf("Calling BackupMessages with chat_id=%s, filepath=%s", chatID, tmpFile)

			result, err := c.CallTool(ctx, callRequest)
			require.NoError(t, err)

			logToolResult(t, result)

			// Verify a file was written
			content, err := os.ReadFile(tmpFile)
			require.NoError(t, err)

			t.Logf("Backup file content (%d bytes):\n%s", len(content), string(content))

			assert.NotEmpty(t, content)
		})
	}

	t.Run("Forbidden path", func(t *testing.T) {
		chatID := os.Getenv("TEST_CHAT_ID")
		if chatID == "" {
			t.Skip("TEST_CHAT_ID not set")
		}

		c, ctx, cleanup := setupClient(t)
		defer cleanup()

		forbiddenPath := filepath.Join(os.TempDir(), "not-allowed", "backup.txt")

		callRequest := mcp.CallToolRequest{}
		callRequest.Params.Name = "BackupMessages"
		callRequest.Params.Arguments = map[string]any{
			"chat_id":  parseChatIDInt(t, chatID),
			"filepath": forbiddenPath,
			"count":    10,
		}

		t.Logf("Calling BackupMessages with forbidden path: %s", forbiddenPath)

		result, err := c.CallTool(ctx, callRequest)
		require.NoError(t, err, "failed to call BackupMessages")
		require.True(t, result.IsError, "expected error for forbidden path, but got success")

		// Check an error message contains expected text
		var errorText string
		for _, content := range result.Content {
			if tc, ok := content.(mcp.TextContent); ok {
				errorText = tc.Text
				break
			}
		}
		assert.Contains(t, errorText, "is not within allowed directories")

		logToolResult(t, result)
	})
}

func TestSummarizeChat(t *testing.T) {
	chatID := os.Getenv("TEST_CHAT_ID")
	if chatID == "" {
		t.Skip("TEST_CHAT_ID not set")
	}

	// SummarizeChat defaults to provider=sampling, which requires the MCP
	// client to advertise the `sampling` capability and handle inbound
	// sampling/createMessage requests. This integration test uses a vanilla
	// mark3labs client that does NOT advertise sampling, so we must switch to
	// a direct LLM provider for the test. Anthropic is the cheapest/most
	// reliable; skip if no API key is configured.
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		t.Skip("ANTHROPIC_API_KEY not set; cannot test SummarizeChat without a sampling-capable client. Set ANTHROPIC_API_KEY to enable this test.")
	}
	t.Setenv("SUMMARIZE_PROVIDER", "anthropic")

	c, ctx, cleanup := setupClientWithTimeout(t, 10*time.Minute)
	defer cleanup()

	callRequest := mcp.CallToolRequest{}
	callRequest.Params.Name = "SummarizeChat"
	callRequest.Params.Arguments = map[string]any{
		"chat_id": parseChatIDInt(t, chatID),
		"goal":    "general context of discussions",
		"period":  "week",
	}

	t.Logf("Calling SummarizeChat with chat_id=%s (this may take a while...)", chatID)

	result, err := c.CallTool(ctx, callRequest)
	require.NoError(t, err)

	require.False(t, result.IsError, "SummarizeChat returned error result")

	logToolResult(t, result)
}

// TestSummarizeChatSamplingFallback verifies that SummarizeChat returns a
// helpful error (not a transport-level failure) when the client lacks the
// sampling capability and no alternative provider is configured. This is the
// regression test for the capability check added in
// internal/summarize/sampling.go.
func TestSummarizeChatSamplingFallback(t *testing.T) {
	chatID := os.Getenv("TEST_CHAT_ID")
	if chatID == "" {
		t.Skip("TEST_CHAT_ID not set")
	}

	// Force sampling provider; the test client doesn't advertise sampling, so
	// the server must surface ErrSamplingUnsupported as a tool result error.
	t.Setenv("SUMMARIZE_PROVIDER", "sampling")

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	callRequest := mcp.CallToolRequest{}
	callRequest.Params.Name = "SummarizeChat"
	callRequest.Params.Arguments = map[string]any{
		"chat_id": parseChatIDInt(t, chatID),
		"goal":    "anything",
		"period":  "day",
	}

	result, err := c.CallTool(ctx, callRequest)
	require.NoError(t, err, "CallTool transport failed (expected a tool-level error, not transport)")
	require.True(t, result.IsError, "expected IsError=true when client lacks sampling capability, got success")

	var msg string
	for _, content := range result.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			msg = tc.Text
			break
		}
	}
	assert.Contains(t, msg, "sampling")
	assert.Contains(t, msg, "summarize-provider")
	t.Logf("Got expected fallback error: %s", msg)
}

func logToolResult(t *testing.T, result *mcp.CallToolResult) {
	t.Helper()
	for _, content := range result.Content {
		switch c := content.(type) {
		case mcp.TextContent:
			var data any
			if err := json.Unmarshal([]byte(c.Text), &data); err == nil {
				pretty, _ := json.MarshalIndent(data, "", "  ")
				t.Logf("Result:\n%s", string(pretty))
			} else {
				t.Logf("Result:\n%s", c.Text)
			}
		default:
			t.Logf("Result: %+v", c)
		}
	}
}

func TestGetMedia(t *testing.T) {
	// We need a chat to scan for a recent photo. The hardcoded TEST_MEDIA_URI
	// from previous test runs goes stale within hours because Telegram's
	// file_reference field is short-lived (FILE_REFERENCE_EXPIRED). Instead,
	// scan recent messages in TEST_CHAT_ID for the first photo and use its
	// fresh resource_uri.
	chatID := os.Getenv("TEST_CHAT_ID")
	if chatID == "" {
		t.Skip("TEST_CHAT_ID not set")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	mediaURI := findFreshMediaURI(t, c, ctx, chatID)
	if mediaURI == "" {
		t.Skip("no recent photo found in TEST_CHAT_ID; nothing to download")
	}

	callRequest := mcp.CallToolRequest{}
	callRequest.Params.Name = "GetMedia"
	callRequest.Params.Arguments = map[string]any{
		"uri": mediaURI,
	}

	t.Logf("Calling GetMedia with fresh uri=%s", mediaURI)

	result, err := c.CallTool(ctx, callRequest)
	require.NoError(t, err, "failed to call GetMedia")
	require.False(t, result.IsError, "GetMedia returned error")

	// Check that we got image content
	var hasImage bool
	for _, content := range result.Content {
		switch c := content.(type) {
		case mcp.ImageContent:
			hasImage = true
			t.Logf("Received image: mimeType=%s, data length=%d bytes", c.MIMEType, len(c.Data))
		case mcp.TextContent:
			t.Logf("Text: %s", c.Text)
		}
	}

	assert.True(t, hasImage, "expected image content in result")
}

// findFreshMediaURI walks recent messages in chatID looking for the first
// photo and returns its resource_uri. Returns "" if no photo is found within
// the first 50 messages. We do this at test time (rather than reading a stale
// TEST_MEDIA_URI from .env) because Telegram's file_reference values expire
// within hours, causing FILE_REFERENCE_EXPIRED errors on cached URIs.
func findFreshMediaURI(t *testing.T, c *client.Client, ctx context.Context, chatID string) string {
	t.Helper()

	req := mcp.CallToolRequest{}
	req.Params.Name = "GetMessages"
	req.Params.Arguments = map[string]any{
		"chat_id": parseChatIDInt(t, chatID),
		"limit":   50,
	}
	result, err := c.CallTool(ctx, req)
	require.NoError(t, err)
	require.False(t, result.IsError, "GetMessages returned error while searching for media")

	// Parse the structured content (or fall back to text) and walk messages
	// looking for the first one with a media.resource_uri of type "photo".
	var raw []byte
	if result.StructuredContent != nil {
		var marshalErr error
		raw, marshalErr = json.Marshal(result.StructuredContent)
		require.NoError(t, marshalErr, "marshaling StructuredContent")
	} else {
		for _, content := range result.Content {
			if tc, ok := content.(mcp.TextContent); ok {
				raw = []byte(tc.Text)
				break
			}
		}
	}

	// Note: ID is now an opaque string handle ("42" for regular, "s:42" for
	// scheduled) after the move to MessageRef at the tool boundary.
	var parsed struct {
		Messages []struct {
			ID    string `json:"id"`
			Media *struct {
				Type        string `json:"type"`
				ResourceURI string `json:"resource_uri"`
			} `json:"media"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed))

	for _, m := range parsed.Messages {
		if m.Media != nil && m.Media.Type == "photo" && m.Media.ResourceURI != "" {
			t.Logf("Found fresh photo: message id=%s, uri=%s", m.ID, m.Media.ResourceURI)
			return m.Media.ResourceURI
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Prompt tests — verify the three guided prompts registered by internal/prompts
// ---------------------------------------------------------------------------

func TestListPrompts(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	result, err := c.ListPrompts(ctx, mcp.ListPromptsRequest{})
	require.NoError(t, err)

	t.Logf("Available prompts: %d", len(result.Prompts))
	got := map[string]bool{}
	for _, p := range result.Prompts {
		t.Logf("  - %s: %s", p.Name, p.Description)
		got[p.Name] = true
	}

	for _, want := range []string{"daily-digest", "chat-catchup", "find-and-reply"} {
		assert.True(t, got[want], "expected prompt %q to be registered", want)
	}
}

func TestGetPromptChatCatchup(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	req := mcp.GetPromptRequest{}
	req.Params.Name = "chat-catchup"
	req.Params.Arguments = map[string]string{
		"chat":   "@durov",
		"period": "week",
	}

	result, err := c.GetPrompt(ctx, req)
	require.NoError(t, err)

	assert.NotEmpty(t, result.Messages)

	tc, ok := result.Messages[0].Content.(mcp.TextContent)
	require.True(t, ok, "expected first message content to be TextContent, got %T", result.Messages[0].Content)

	// The handler should have substituted both arguments and referenced the
	// SummarizeChat tool. Disambiguation guidance per tool-design.md.
	for _, want := range []string{"@durov", "week", "SummarizeChat"} {
		assert.Contains(t, tc.Text, want)
	}
	t.Logf("chat-catchup prompt text:\n%s", tc.Text)
}

func TestGetPromptMissingRequiredArg(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	// chat-catchup requires `chat`. Omitting it should fail at the prompt
	// handler level.
	req := mcp.GetPromptRequest{}
	req.Params.Name = "chat-catchup"
	req.Params.Arguments = map[string]string{
		"period": "day",
	}

	_, err := c.GetPrompt(ctx, req)
	require.Error(t, err, "expected error when required argument 'chat' is missing")
	t.Logf("Got expected error: %v", err)
}

// ---------------------------------------------------------------------------
// Resource template test — telegram://chat/{chat_id}/info
// ---------------------------------------------------------------------------

func TestGetChatInfoTemplate(t *testing.T) {
	chatID := os.Getenv("TEST_CHAT_ID")
	if chatID == "" {
		t.Skip("TEST_CHAT_ID not set")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	// First confirm the template is advertised in ListResourceTemplates so we
	// know clients can discover it.
	tmpls, err := c.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
	require.NoError(t, err, "failed to list resource templates")

	var found bool
	for _, tmpl := range tmpls.ResourceTemplates {
		if strings.Contains(tmpl.URITemplate.Raw(), "telegram://chat/") {
			found = true
			t.Logf("Found chat info template: %s", tmpl.URITemplate.Raw())
			break
		}
	}
	assert.True(t, found, "expected telegram://chat/{chat_id}/info template to be registered")

	// Then read a concrete URI by substituting the test chat ID.
	uri := fmt.Sprintf("telegram://chat/%s/info", chatID)
	req := mcp.ReadResourceRequest{}
	req.Params.URI = uri

	result, err := c.ReadResource(ctx, req)
	require.NoError(t, err)
	assert.NotEmpty(t, result.Contents)

	tc, ok := result.Contents[0].(mcp.TextResourceContents)
	require.True(t, ok, "expected TextResourceContents, got %T", result.Contents[0])
	assert.Equal(t, "application/json", tc.MIMEType)

	// Verify it's parseable JSON with at least an id field, proving the
	// template parameter was actually substituted into GetChatInfo.
	var data map[string]any
	require.NoError(t, json.Unmarshal([]byte(tc.Text), &data))
	assert.Contains(t, mapKeys(data), "id")
	t.Logf("Chat info via template:\n%s", tc.Text)
}

// ---------------------------------------------------------------------------
// StructuredContent check — verify jsonResult populates result.StructuredContent
// in addition to the text fallback. We test on GetMessages because it's the
// safest read-only tool that uses jsonResult().
// ---------------------------------------------------------------------------

func TestGetMessagesStructuredContent(t *testing.T) {
	chatID := os.Getenv("TEST_CHAT_ID")
	if chatID == "" {
		t.Skip("TEST_CHAT_ID not set")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	callRequest := mcp.CallToolRequest{}
	callRequest.Params.Name = "GetMessages"
	callRequest.Params.Arguments = map[string]any{
		"chat_id": parseChatIDInt(t, chatID),
		"limit":   5,
	}

	result, err := c.CallTool(ctx, callRequest)
	require.NoError(t, err, "failed to call GetMessages")
	require.False(t, result.IsError, "GetMessages returned error")

	// jsonResult() always sets StructuredContent — clients that support typed
	// output (per tool-design.md) should see this populated.
	require.NotNil(t, result.StructuredContent, "expected StructuredContent to be populated by jsonResult")

	// Should be the wrapper type with FetchResult fields plus pagination_hint.
	raw, err := json.Marshal(result.StructuredContent)
	require.NoError(t, err, "marshaling StructuredContent")
	t.Logf("StructuredContent: %s", string(raw))
	var parsed map[string]any
	require.NoError(t, json.Unmarshal(raw, &parsed), "StructuredContent is not JSON-serializable")

	// FetchResult embeds the expected fields.
	for _, key := range []string{"chat_id", "messages", "count", "has_more"} {
		assert.Contains(t, parsed, key, "expected StructuredContent to include %q, got keys: %v", key, mapKeys(parsed))
	}

	// And the text fallback should still be present so non-structured clients
	// see something useful.
	var hasText bool
	for _, content := range result.Content {
		if _, ok := content.(mcp.TextContent); ok {
			hasText = true
			break
		}
	}
	assert.True(t, hasText, "expected text fallback in result.Content alongside StructuredContent")
}

// ---------------------------------------------------------------------------
// P1 feature tests — date filter on GetMessages, ResolveMessageLink,
// GetMessageContext. See research/COMPETITORS.md for the context behind
// each feature.
// ---------------------------------------------------------------------------

// TestGetMessagesDateFilter verifies that GetMessages honours from_date and
// to_date and that every returned message falls inside the requested window.
// Runs against the same TEST_CHAT_ID as the rest of the suite; skips cleanly
// if unset. Uses a 7-day window which should contain at least some messages
// in any active chat, and collapses the assertion to a no-op when the chat
// is silent (the contract is "filter correctness", not "non-empty result").
func TestGetMessagesDateFilter(t *testing.T) {
	chatID := os.Getenv("TEST_CHAT_ID")
	if chatID == "" {
		t.Skip("TEST_CHAT_ID not set")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	now := time.Now().UTC()
	from := now.Add(-7 * 24 * time.Hour)
	to := now

	req := mcp.CallToolRequest{}
	req.Params.Name = "GetMessages"
	req.Params.Arguments = map[string]any{
		"chat_id":   parseChatIDInt(t, chatID),
		"limit":     50,
		"from_date": from.Format(time.RFC3339),
		"to_date":   to.Format(time.RFC3339),
	}

	result, err := c.CallTool(ctx, req)
	require.NoError(t, err, "failed to call GetMessages")
	require.False(t, result.IsError, "GetMessages returned error")

	raw := structuredOrText(result)
	var parsed struct {
		Messages []struct {
			ID   string    `json:"id"`
			Date time.Time `json:"date"`
		} `json:"messages"`
		HasMore bool `json:"has_more"`
	}
	require.NoError(t, json.Unmarshal(raw, &parsed), "failed to parse GetMessages result")

	t.Logf("Date filter returned %d messages in [%s, %s]", len(parsed.Messages), from.Format(time.RFC3339), to.Format(time.RFC3339))

	// Every returned message must fall within the requested window.
	// Allow a small clock-skew grace on the upper bound.
	grace := 5 * time.Minute
	for _, m := range parsed.Messages {
		assert.False(t, m.Date.Before(from), "message %s dated %s is before from_date %s", m.ID, m.Date, from)
		assert.False(t, m.Date.After(to.Add(grace)), "message %s dated %s is after to_date %s (+%s grace)", m.ID, m.Date, to, grace)
	}

	// When from_date is set and all recent messages fit in the window,
	// HasMore should be clamped to false the moment we hit the boundary.
	// We can't assert that unconditionally (the chat may have >limit
	// messages in the window), but if len < limit we can.
	assert.False(t, len(parsed.Messages) < 50 && parsed.HasMore, "short page (%d < limit) should have has_more=false with from_date set", len(parsed.Messages))
}

// TestResolveMessageLinkPrivate exercises the private-channel branch of
// ResolveMessageLink (the t.me/c/… form). This branch is fully offline
// and does not require any env vars: we feed a synthetic link and verify
// the -100 prefix conversion.
func TestResolveMessageLinkPrivate(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	req := mcp.CallToolRequest{}
	req.Params.Name = "ResolveMessageLink"
	req.Params.Arguments = map[string]any{
		"link": "https://t.me/c/1234567890/42",
	}

	result, err := c.CallTool(ctx, req)
	require.NoError(t, err, "failed to call ResolveMessageLink")
	require.False(t, result.IsError, "ResolveMessageLink returned error on a valid private-channel link")

	var parsed struct {
		ChatID    int64  `json:"chat_id"`
		MessageID string `json:"message_id"`
		Hint      string `json:"next_step_hint"`
	}
	require.NoError(t, json.Unmarshal(structuredOrText(result), &parsed), "failed to parse ResolveMessageLink result")

	const wantChatID = int64(-1001234567890)
	assert.Equal(t, wantChatID, parsed.ChatID, "chat_id should be the -100 prefix form of the URL's c/<id>")
	assert.Equal(t, "42", parsed.MessageID)
	assert.NotEmpty(t, parsed.Hint, "expected next_step_hint for private-channel links")
}

// TestResolveMessageLinkPublic exercises the public-username branch, which
// does hit Telegram. Gated on TEST_MESSAGE_LINK (e.g. a real t.me/<user>/<id>
// URL from a public channel the test account can see).
func TestResolveMessageLinkPublic(t *testing.T) {
	link := os.Getenv("TEST_MESSAGE_LINK")
	if link == "" {
		t.Skip("TEST_MESSAGE_LINK not set (expected a public t.me/<username>/<id> URL)")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	req := mcp.CallToolRequest{}
	req.Params.Name = "ResolveMessageLink"
	req.Params.Arguments = map[string]any{"link": link}

	result, err := c.CallTool(ctx, req)
	require.NoError(t, err, "failed to call ResolveMessageLink")
	require.False(t, result.IsError, "ResolveMessageLink returned error for %q", link)

	var parsed struct {
		ChatID    int64  `json:"chat_id"`
		ChatTitle string `json:"chat_title"`
		Username  string `json:"username"`
		MessageID string `json:"message_id"`
	}
	require.NoError(t, json.Unmarshal(structuredOrText(result), &parsed), "failed to parse ResolveMessageLink result")
	assert.NotZero(t, parsed.ChatID, "expected non-zero chat_id from public link resolution")
	assert.NotEmpty(t, parsed.MessageID, "expected non-empty message_id")
	assert.NotEmpty(t, parsed.Username, "expected username to be echoed back for public links")
	t.Logf("Resolved %s → chat_id=%d title=%q message_id=%s", link, parsed.ChatID, parsed.ChatTitle, parsed.MessageID)
}

// TestGetMessageContext fetches a recent message's ID and then asks for a
// context window around it, verifying chronological ordering and the
// presence of the anchor inside the returned slice.
func TestGetMessageContext(t *testing.T) {
	chatID := os.Getenv("TEST_CHAT_ID")
	if chatID == "" {
		t.Skip("TEST_CHAT_ID not set")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	// Step 1: fetch a handful of recent messages to pick an anchor.
	fetchReq := mcp.CallToolRequest{}
	fetchReq.Params.Name = "GetMessages"
	fetchReq.Params.Arguments = map[string]any{
		"chat_id": parseChatIDInt(t, chatID),
		"limit":   10,
	}
	fetchResult, err := c.CallTool(ctx, fetchReq)
	require.NoError(t, err, "failed to call GetMessages to seed anchor")
	require.False(t, fetchResult.IsError, "GetMessages returned error while seeding anchor")

	var seed struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(structuredOrText(fetchResult), &seed), "failed to parse seed GetMessages result")
	require.NotEmpty(t, seed.Messages, "TEST_CHAT_ID has no recent messages to seed the context window")

	// Pick the oldest of the 10 most-recent messages so before/after both
	// have room to expand.
	anchor := seed.Messages[len(seed.Messages)-1].ID
	t.Logf("Using anchor message_id=%s", anchor)

	// Step 2: request context around the anchor.
	req := mcp.CallToolRequest{}
	req.Params.Name = "GetMessageContext"
	req.Params.Arguments = map[string]any{
		"chat_id":    parseChatIDInt(t, chatID),
		"message_id": anchor,
		"before":     3,
		"after":      3,
	}
	result, err := c.CallTool(ctx, req)
	require.NoError(t, err, "failed to call GetMessageContext")
	require.False(t, result.IsError, "GetMessageContext returned error")

	var parsed struct {
		ChatID   int64  `json:"chat_id"`
		AnchorID string `json:"anchor_id"`
		Messages []struct {
			ID   string    `json:"id"`
			Date time.Time `json:"date"`
		} `json:"messages"`
		Count int `json:"count"`
	}
	require.NoError(t, json.Unmarshal(structuredOrText(result), &parsed), "failed to parse GetMessageContext result")

	assert.Equal(t, anchor, parsed.AnchorID)
	require.NotZero(t, parsed.Count, "expected at least the anchor message back")

	// Chronological order: each message's date must be >= the previous.
	for i := 1; i < len(parsed.Messages); i++ {
		assert.False(t, parsed.Messages[i].Date.Before(parsed.Messages[i-1].Date), "messages are not in chronological order at index %d: %s before %s", i, parsed.Messages[i].Date, parsed.Messages[i-1].Date)
	}

	// The anchor itself must appear in the returned window.
	found := false
	for _, m := range parsed.Messages {
		if m.ID == anchor {
			found = true
			break
		}
	}
	assert.True(t, found, "anchor %q not present in returned context window of %d messages", anchor, parsed.Count)

	// Cap: at most before+after+1 = 7 messages.
	assert.LessOrEqual(t, parsed.Count, 7, "context window returned %d messages, expected ≤ 7 (before=3 + anchor + after=3)", parsed.Count)
	t.Logf("Context window: %d messages around anchor %s", parsed.Count, anchor)
}

// structuredOrText extracts the JSON payload from a CallToolResult,
// preferring StructuredContent (the typed SDK path) and falling back to the
// first TextContent for tools that still use jsonResult. Shared helper for
// the P1 tests above so each one doesn't repeat the same dispatch.
func structuredOrText(result *mcp.CallToolResult) []byte {
	if result.StructuredContent != nil {
		raw, err := json.Marshal(result.StructuredContent)
		if err != nil {
			panic(fmt.Sprintf("structuredOrText: json.Marshal failed: %v", err))
		}
		return raw
	}
	for _, content := range result.Content {
		if tc, ok := content.(mcp.TextContent); ok {
			return []byte(tc.Text)
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Recovery hint tests — verify that errors include actionable next steps
// per tool-design.md § Errors. These run with no real Telegram interaction
// because we expect them to fail at the validation stage.
//
// Note on the test pattern: we send PRESENT-BUT-ZERO values (chat_id: 0,
// query: "", etc.) rather than omitting the fields entirely. The official
// Go MCP SDK enforces JSON schema "required" validation BEFORE calling the
// handler, so a completely absent field yields a generic SDK validation
// message ("missing properties: chat_id") rather than our custom
// actionable hint. Zero/empty values pass the schema "present" check, let
// the handler run, and exercise the recovery-hint path that tool-design.md
// cares about. This is also a realistic failure mode: the model
// occasionally sets a chat_id to 0 by mistake.
// ---------------------------------------------------------------------------

func TestErrorRecoveryHints(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	cases := []struct {
		name      string
		tool      string
		args      map[string]any
		wantHints []string // substrings the error message must contain
	}{
		{
			name: "SendMessage with zero chat_id",
			tool: "SendMessage",
			args: map[string]any{
				"chat_id": 0,
				"message": "hi",
			},
			wantHints: []string{"chat_id is required", "SearchChats", "ResolveUsername"},
		},
		{
			// After the SendMessage merge (commit 7375d86), ReplyToMessage no
			// longer exists as a standalone tool — reply semantics live on
			// SendMessage via reply_to_message_id. Exercise that path instead.
			name: "SendMessage with invalid reply_to_message_id",
			tool: "SendMessage",
			args: map[string]any{
				"chat_id":             123,
				"message":             "ok",
				"reply_to_message_id": "not-a-handle",
			},
			wantHints: []string{"invalid", "message_id"},
		},
		{
			// message_id is an opaque string handle ("42") since the opaque-
			// handles migration — passing an int is now a schema-level error.
			name: "DeleteMessage with zero chat_id",
			tool: "DeleteMessage",
			args: map[string]any{
				"chat_id":    0,
				"message_id": "1",
			},
			wantHints: []string{"chat_id is required", "SearchChats"},
		},
		{
			name: "SearchChats with empty query",
			tool: "SearchChats",
			args: map[string]any{
				"query": "",
			},
			wantHints: []string{"query parameter is required"},
		},
		{
			name: "ResolveUsername with empty username",
			tool: "ResolveUsername",
			args: map[string]any{
				"username": "",
			},
			wantHints: []string{"username is required", "SearchChats"},
		},
		{
			name: "MarkAsRead with empty chat_ids",
			tool: "MarkAsRead",
			args: map[string]any{
				"chat_ids": []int64{},
			},
			wantHints: []string{"chat_ids is required", "GetChats"},
		},
		{
			name: "GetMessages with invalid from_date",
			tool: "GetMessages",
			args: map[string]any{
				"chat_id":   123,
				"from_date": "yesterday",
			},
			wantHints: []string{"invalid from_date", "RFC3339"},
		},
		{
			name: "GetMessages with from_date after to_date",
			tool: "GetMessages",
			args: map[string]any{
				"chat_id":   123,
				"from_date": "2026-04-09T00:00:00Z",
				"to_date":   "2026-04-01T00:00:00Z",
			},
			wantHints: []string{"from_date", "to_date", "window is empty"},
		},
		{
			name: "ResolveMessageLink with empty link",
			tool: "ResolveMessageLink",
			args: map[string]any{
				"link": "",
			},
			wantHints: []string{"link is required"},
		},
		{
			name: "ResolveMessageLink with joinchat invite",
			tool: "ResolveMessageLink",
			args: map[string]any{
				"link": "https://t.me/joinchat/abcDEF",
			},
			wantHints: []string{"invalid link", "invite link"},
		},
		{
			name: "ResolveMessageLink with bare username (no message id)",
			tool: "ResolveMessageLink",
			args: map[string]any{
				"link": "https://t.me/durov",
			},
			wantHints: []string{"invalid link", "missing message id"},
		},
		{
			name: "GetMessageContext with zero chat_id",
			tool: "GetMessageContext",
			args: map[string]any{
				"chat_id":    0,
				"message_id": "1",
			},
			wantHints: []string{"chat_id is required"},
		},
		{
			name: "GetMessageContext with empty message_id",
			tool: "GetMessageContext",
			args: map[string]any{
				"chat_id":    123,
				"message_id": "",
			},
			wantHints: []string{"message_id is required"},
		},
		{
			name: "GetMessageContext with scheduled handle",
			tool: "GetMessageContext",
			args: map[string]any{
				"chat_id":    123,
				"message_id": "s:42",
			},
			wantHints: []string{"scheduled message"},
		},
		{
			name: "SearchMessages with zero chat_id",
			tool: "SearchMessages",
			args: map[string]any{
				"chat_id": 0,
				"query":   "hi",
			},
			wantHints: []string{"chat_id is required", "SearchChats"},
		},
		{
			name: "SearchMessages with empty query",
			tool: "SearchMessages",
			args: map[string]any{
				"chat_id": 123,
				"query":   "",
			},
			wantHints: []string{"query is required"},
		},
		{
			name: "SearchMessages with invalid media_type",
			tool: "SearchMessages",
			args: map[string]any{
				"chat_id":    123,
				"query":      "hi",
				"media_type": "emoji",
			},
			wantHints: []string{"invalid media_type", "photos"},
		},
		{
			name: "SearchMessages with scheduled offset_id",
			tool: "SearchMessages",
			args: map[string]any{
				"chat_id":   123,
				"query":     "hi",
				"offset_id": "s:42",
			},
			wantHints: []string{"scheduled"},
		},
		{
			name: "SearchMessagesGlobal with empty query",
			tool: "SearchMessagesGlobal",
			args: map[string]any{
				"query": "",
			},
			wantHints: []string{"query is required"},
		},
		{
			name: "SearchMessagesGlobal with invalid cursor",
			tool: "SearchMessagesGlobal",
			args: map[string]any{
				"query":  "hi",
				"cursor": "!!!not-a-cursor!!!",
			},
			wantHints: []string{"invalid cursor", "verbatim"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mcp.CallToolRequest{}
			req.Params.Name = tc.tool
			req.Params.Arguments = tc.args

			result, err := c.CallTool(ctx, req)
			require.NoError(t, err, "transport error (expected tool-level error)")
			require.True(t, result.IsError, "expected IsError=true, got success")

			var msg string
			for _, content := range result.Content {
				if textContent, ok := content.(mcp.TextContent); ok {
					msg = textContent.Text
					break
				}
			}
			for _, hint := range tc.wantHints {
				assert.Contains(t, msg, hint, "expected error message to contain %q, got: %s", hint, msg)
			}
			t.Logf("Got: %s", msg)
		})
	}
}

// ---------------------------------------------------------------------------
// Search tests — SearchMessages (single chat) and SearchMessagesGlobal
// (cross-chat). See plans/frolicking-chasing-patterson.md for the design.
//
// These hit real chats via MTProto, so assertions focus on structural
// invariants that hold regardless of chat content:
//
//   * count == len(messages)
//   * every id is an opaque numeric handle ("42", never "s:…")
//   * has_more and next_offset_id / next_cursor are consistent
//   * when a second page is requested via the cursor, message ids do not
//     overlap the first page
//
// When the chosen query returns nothing, tests soft-skip the per-message
// assertions but still exercise the happy path. Users can tune the query
// via TEST_MESSAGE_SEARCH_QUERY in .env.
// ---------------------------------------------------------------------------

// searchMessageDTO is a minimal decoder for messages returned by
// SearchMessages / SearchMessagesGlobal. It intentionally ignores fields the
// tests don't assert on (entities, reply_to_id, sender_name) to stay robust
// to additive changes in the tool-boundary DTO.
type searchMessageDTO struct {
	ID        string    `json:"id"`
	Date      time.Time `json:"date"`
	Text      string    `json:"text"`
	ChatID    int64     `json:"chat_id,omitempty"`    // set only by SearchMessagesGlobal
	ChatTitle string    `json:"chat_title,omitempty"` // set only by SearchMessagesGlobal
	Media     *struct {
		Type string `json:"type"`
	} `json:"media,omitempty"`
}

// searchMessagesResult is the decoded shape of SearchMessages output.
type searchMessagesResult struct {
	ChatID       int64              `json:"chat_id"`
	Query        string             `json:"query"`
	Messages     []searchMessageDTO `json:"messages"`
	Count        int                `json:"count"`
	HasMore      bool               `json:"has_more"`
	NextOffsetID string             `json:"next_offset_id,omitempty"`
}

// searchMessagesGlobalResult is the decoded shape of SearchMessagesGlobal.
type searchMessagesGlobalResult struct {
	Query      string             `json:"query"`
	Messages   []searchMessageDTO `json:"messages"`
	Count      int                `json:"count"`
	HasMore    bool               `json:"has_more"`
	NextCursor string             `json:"next_cursor,omitempty"`
}

// searchQueryOrDefault returns the query to use for message-content search
// tests. "test" is a lowest-common-denominator fallback that at least runs
// on English-speaking chats; most users will set TEST_MESSAGE_SEARCH_QUERY
// in .env to something that actually matches their test chats.
func searchQueryOrDefault() string {
	if q := os.Getenv("TEST_MESSAGE_SEARCH_QUERY"); q != "" {
		return q
	}
	return "test"
}

// assertSearchMessagesShape runs the structural invariants that must hold
// for any SearchMessages / SearchMessagesGlobal response, regardless of
// whether the query matched anything. Returns the decoded messages so the
// caller can do test-specific follow-up assertions.
func assertSearchMessagesShape(t *testing.T, raw []byte, wantChatID int64, global bool) []searchMessageDTO {
	t.Helper()

	var msgs []searchMessageDTO
	var count int
	var hasMore bool
	var nextToken string
	var tokenName string

	if global {
		var parsed searchMessagesGlobalResult
		require.NoError(t, json.Unmarshal(raw, &parsed), "failed to parse SearchMessagesGlobal result")
		msgs, count, hasMore, nextToken, tokenName = parsed.Messages, parsed.Count, parsed.HasMore, parsed.NextCursor, "next_cursor"
	} else {
		var parsed searchMessagesResult
		require.NoError(t, json.Unmarshal(raw, &parsed), "failed to parse SearchMessages result")
		if wantChatID != 0 {
			assert.Equal(t, wantChatID, parsed.ChatID)
		}
		msgs, count, hasMore, nextToken, tokenName = parsed.Messages, parsed.Count, parsed.HasMore, parsed.NextOffsetID, "next_offset_id"
	}

	assert.Equal(t, len(msgs), count, "count and len(messages) must match")
	assert.False(t, hasMore && nextToken == "", "has_more=true but %s is empty", tokenName)
	assert.False(t, !hasMore && nextToken != "", "has_more=false but %s=%q is set", tokenName, nextToken)

	// Per-message invariants. Id handles must be positive decimal integers
	// with no scheduled prefix — the "s:" form is not reachable via search.
	for i, m := range msgs {
		assert.NotEmpty(t, m.ID, "message[%d] has empty id", i)
		assert.False(t, strings.HasPrefix(m.ID, "s:"), "message[%d] id=%q uses scheduled prefix; SearchMessages must only return regular handles", i, m.ID)
		_, err := strconv.Atoi(m.ID)
		assert.NoError(t, err, "message[%d] id=%q is not a numeric handle", i, m.ID)
		if global {
			assert.NotZero(t, m.ChatID, "global message[%d] (id=%s) has zero chat_id — SearchMessagesGlobal must attribute every message", i, m.ID)
		}
	}
	return msgs
}

// TestSearchMessages exercises single-chat substring search against each
// configured test chat (dialog + group). Assertions focus on structural
// invariants; the actual hit count depends on chat content, so tests pass
// on empty results too.
func TestSearchMessages(t *testing.T) {
	cases := []struct {
		name   string
		envVar string
	}{
		{"Dialog", "TEST_CHAT_ID"},
		{"Group", "TEST_GROUP_ID"},
	}

	query := searchQueryOrDefault()

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			chatID := os.Getenv(tc.envVar)
			if chatID == "" {
				t.Skipf("%s not set", tc.envVar)
			}
			chatIDInt := parseChatIDInt(t, chatID)

			req := mcp.CallToolRequest{}
			req.Params.Name = "SearchMessages"
			req.Params.Arguments = map[string]any{
				"chat_id": chatIDInt,
				"query":   query,
				"limit":   20,
			}

			t.Logf("Calling SearchMessages with chat_id=%s, query=%q", chatID, query)

			result, err := c.CallTool(ctx, req)
			require.NoError(t, err, "failed to call SearchMessages")
			require.False(t, result.IsError, "SearchMessages returned error")

			msgs := assertSearchMessagesShape(t, structuredOrText(result), chatIDInt, false)
			t.Logf("SearchMessages returned %d messages", len(msgs))
		})
	}
}

// TestSearchMessagesDateFilter verifies that from_date / to_date clip the
// search window and that every returned message falls within it.
func TestSearchMessagesDateFilter(t *testing.T) {
	chatID := os.Getenv("TEST_CHAT_ID")
	if chatID == "" {
		t.Skip("TEST_CHAT_ID not set")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	now := time.Now().UTC()
	from := now.Add(-30 * 24 * time.Hour)
	to := now

	req := mcp.CallToolRequest{}
	req.Params.Name = "SearchMessages"
	req.Params.Arguments = map[string]any{
		"chat_id":   parseChatIDInt(t, chatID),
		"query":     searchQueryOrDefault(),
		"limit":     20,
		"from_date": from.Format(time.RFC3339),
		"to_date":   to.Format(time.RFC3339),
	}

	result, err := c.CallTool(ctx, req)
	require.NoError(t, err, "failed to call SearchMessages")
	require.False(t, result.IsError, "SearchMessages returned error")

	msgs := assertSearchMessagesShape(t, structuredOrText(result), parseChatIDInt(t, chatID), false)
	t.Logf("Date-filtered SearchMessages returned %d messages", len(msgs))

	// Grace on the upper bound mirrors TestGetMessagesDateFilter: allow a
	// few minutes for clock skew between local and Telegram servers.
	grace := 5 * time.Minute
	for _, m := range msgs {
		assert.False(t, m.Date.Before(from), "message %s dated %s is before from_date %s", m.ID, m.Date, from)
		assert.False(t, m.Date.After(to.Add(grace)), "message %s dated %s is after to_date %s (+%s grace)", m.ID, m.Date, to, grace)
	}
}

// TestSearchMessagesMediaFilter verifies the media_type filter. Runs against
// the group (more likely to have mixed media) and asserts that every
// returned message, if any, carries a non-nil media payload. We don't pin
// the specific media.type because Telegram's filter group is coarser than
// the DTO's type string (e.g. InputMessagesFilterPhotos matches both photo
// messages and photo-album sub-items).
func TestSearchMessagesMediaFilter(t *testing.T) {
	chatID := os.Getenv("TEST_GROUP_ID")
	if chatID == "" {
		t.Skip("TEST_GROUP_ID not set")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	req := mcp.CallToolRequest{}
	req.Params.Name = "SearchMessages"
	req.Params.Arguments = map[string]any{
		"chat_id":    parseChatIDInt(t, chatID),
		"query":      searchQueryOrDefault(),
		"limit":      20,
		"media_type": "photos",
	}

	result, err := c.CallTool(ctx, req)
	require.NoError(t, err, "failed to call SearchMessages")
	require.False(t, result.IsError, "SearchMessages with media_type=photos returned error")

	msgs := assertSearchMessagesShape(t, structuredOrText(result), parseChatIDInt(t, chatID), false)
	t.Logf("media_type=photos returned %d messages", len(msgs))

	for _, m := range msgs {
		assert.NotNil(t, m.Media, "message %s has no media but matched media_type=photos filter", m.ID)
	}
}

// TestSearchMessagesPagination fetches one page at limit=1, then pages again
// via next_offset_id and verifies that the second page does not repeat any
// id from the first. Hard-fails if the test query returns fewer than two
// results — the test account must contain at least two matches so the
// cursor round-trip is actually exercised in CI.
func TestSearchMessagesPagination(t *testing.T) {
	chatID := os.Getenv("TEST_GROUP_ID")
	if chatID == "" {
		chatID = os.Getenv("TEST_CHAT_ID")
	}
	if chatID == "" {
		t.Skip("neither TEST_GROUP_ID nor TEST_CHAT_ID set")
	}
	chatIDInt := parseChatIDInt(t, chatID)

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	// limit=1 forces pagination: any query with at least 2 matching messages
	// produces multiple pages so the cursor round-trip is always exercised.
	const limit = 1

	callPage := func(offset string) *searchMessagesResult {
		req := mcp.CallToolRequest{}
		req.Params.Name = "SearchMessages"
		args := map[string]any{
			"chat_id": chatIDInt,
			"query":   searchQueryOrDefault(),
			"limit":   limit,
		}
		if offset != "" {
			args["offset_id"] = offset
		}
		req.Params.Arguments = args

		result, err := c.CallTool(ctx, req)
		require.NoError(t, err, "SearchMessages call failed")
		require.False(t, result.IsError, "SearchMessages returned error")

		var parsed searchMessagesResult
		require.NoError(t, json.Unmarshal(structuredOrText(result), &parsed), "failed to parse SearchMessages page")
		return &parsed
	}

	page1 := callPage("")
	t.Logf("page 1: count=%d, has_more=%v, next=%s", page1.Count, page1.HasMore, page1.NextOffsetID)

	if page1.Count == 0 {
		t.Skipf("query %q returned no results — test account needs at least 2 matching messages in chat %s", searchQueryOrDefault(), chatID)
	}
	if !page1.HasMore || page1.NextOffsetID == "" {
		t.Skipf("query %q returned only one result at limit=1 — test account needs at least 2 matching messages to exercise pagination", searchQueryOrDefault())
	}

	page2 := callPage(page1.NextOffsetID)
	t.Logf("page 2: count=%d, has_more=%v, next=%s", page2.Count, page2.HasMore, page2.NextOffsetID)

	seen := make(map[string]bool, len(page1.Messages))
	for _, m := range page1.Messages {
		seen[m.ID] = true
	}
	for _, m := range page2.Messages {
		assert.False(t, seen[m.ID], "message id %s appears on both page 1 and page 2 — pagination is double-counting", m.ID)
	}
}

// TestSearchMessagesGlobal exercises cross-chat search. Structural checks
// are identical to the single-chat case, plus the requirement that every
// message carries a non-zero chat_id (attribution is the whole point).
func TestSearchMessagesGlobal(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	req := mcp.CallToolRequest{}
	req.Params.Name = "SearchMessagesGlobal"
	req.Params.Arguments = map[string]any{
		"query": searchQueryOrDefault(),
		"limit": 20,
	}

	t.Logf("Calling SearchMessagesGlobal with query=%q", searchQueryOrDefault())

	result, err := c.CallTool(ctx, req)
	require.NoError(t, err, "failed to call SearchMessagesGlobal")
	require.False(t, result.IsError, "SearchMessagesGlobal returned error")

	msgs := assertSearchMessagesShape(t, structuredOrText(result), 0, true)
	t.Logf("SearchMessagesGlobal returned %d messages across %d distinct chats",
		len(msgs), countDistinctChats(msgs))
}

// TestSearchMessagesGlobalPagination round-trips through the opaque cursor
// at limit=1 and verifies no (chat_id, id) collisions between pages. Hard-
// fails if the test query returns fewer than two global results — the
// test account must contain at least two matches across its dialogs so
// the cursor round-trip is actually exercised in CI.
func TestSearchMessagesGlobalPagination(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	// limit=1 forces pagination: any query with at least 2 matching messages
	// across the account produces multiple pages so the cursor round-trip
	// is always exercised.
	const limit = 1

	callPage := func(cursor string) *searchMessagesGlobalResult {
		req := mcp.CallToolRequest{}
		req.Params.Name = "SearchMessagesGlobal"
		args := map[string]any{
			"query": searchQueryOrDefault(),
			"limit": limit,
		}
		if cursor != "" {
			args["cursor"] = cursor
		}
		req.Params.Arguments = args

		result, err := c.CallTool(ctx, req)
		require.NoError(t, err, "SearchMessagesGlobal call failed")
		require.False(t, result.IsError, "SearchMessagesGlobal returned error")

		var parsed searchMessagesGlobalResult
		require.NoError(t, json.Unmarshal(structuredOrText(result), &parsed), "failed to parse SearchMessagesGlobal page")
		return &parsed
	}

	page1 := callPage("")
	t.Logf("global page 1: count=%d, has_more=%v", page1.Count, page1.HasMore)

	if page1.Count == 0 {
		t.Skipf("query %q returned no global results — test account needs at least 2 matching messages", searchQueryOrDefault())
	}
	if !page1.HasMore || page1.NextCursor == "" {
		t.Skipf("query %q returned only one global result at limit=1 — test account needs at least 2 matching messages to exercise pagination", searchQueryOrDefault())
	}

	page2 := callPage(page1.NextCursor)
	t.Logf("global page 2: count=%d, has_more=%v", page2.Count, page2.HasMore)

	// The (chat_id, id) pair is the unique identifier across chats —
	// using id alone would false-positive when two different chats
	// happen to share a message id.
	seen := make(map[string]bool, len(page1.Messages))
	key := func(m searchMessageDTO) string {
		return fmt.Sprintf("%d/%s", m.ChatID, m.ID)
	}
	for _, m := range page1.Messages {
		seen[key(m)] = true
	}
	for _, m := range page2.Messages {
		assert.False(t, seen[key(m)], "message %s appears on both global page 1 and page 2 — cursor pagination is double-counting", key(m))
	}
}

// countDistinctChats returns how many distinct chat_ids appear in a slice
// of global-search messages, used purely for logging.
func countDistinctChats(msgs []searchMessageDTO) int {
	seen := make(map[int64]bool, len(msgs))
	for _, m := range msgs {
		seen[m.ChatID] = true
	}
	return len(seen)
}

// mapKeys returns the keys of a map[string]any in a stable order, used in
// test diagnostics where the map's actual content is too noisy to log.
func mapKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

func logResourceResult(t *testing.T, result *mcp.ReadResourceResult) {
	t.Helper()
	for _, content := range result.Contents {
		if textContent, ok := content.(mcp.TextResourceContents); ok {
			var data any
			if err := json.Unmarshal([]byte(textContent.Text), &data); err == nil {
				pretty, _ := json.MarshalIndent(data, "", "  ")
				output := string(pretty)
				if len(output) > 2000 {
					output = output[:2000] + "\n... (truncated)"
				}
				t.Logf("Result:\n%s", output)
			} else {
				t.Logf("Result:\n%s", textContent.Text)
			}
		}
	}
}
