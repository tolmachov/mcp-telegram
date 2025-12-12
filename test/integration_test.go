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
	"strings"
	"testing"
	"time"

	"github.com/joho/godotenv"
	"github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/tolmachov/mcp-telegram/internal"
)

func init() {
	if err := godotenv.Load("../.env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		panic(fmt.Sprintf("failed to load .env file: %v", err))
	}
}

func setupClient(t *testing.T) (*client.Client, context.Context, func()) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

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
	}()

	// Create transport from pipes
	stdioTransport := transport.NewIO(clientReader, clientWriter, stderrReader)

	c := client.NewClient(stdioTransport)

	cleanup := func() {
		// Close client
		if err := c.Close(); err != nil {
			t.Errorf("failed to close client: %v", err)
		}

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
				t.Errorf("server error: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("server did not stop in time")
		}

		cancel()
	}

	if err := c.Start(ctx); err != nil {
		cleanup()
		t.Fatalf("failed to start client: %v", err)
	}

	initRequest := mcp.InitializeRequest{}
	initRequest.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	initRequest.Params.ClientInfo = mcp.Implementation{
		Name:    "mcp-telegram-test",
		Version: "1.0.0",
	}
	initRequest.Params.Capabilities = mcp.ClientCapabilities{}

	serverInfo, err := c.Initialize(ctx, initRequest)
	if err != nil {
		cleanup()
		t.Fatalf("failed to initialize: %v", err)
	}

	t.Logf("Connected to server: %s (version %s)", serverInfo.ServerInfo.Name, serverInfo.ServerInfo.Version)

	return c, ctx, cleanup
}

func TestListResources(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	resourcesResult, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		t.Fatalf("failed to list resources: %v", err)
	}

	t.Logf("Available resources: %d", len(resourcesResult.Resources))
	for _, resource := range resourcesResult.Resources {
		t.Logf("  - %s: %s", resource.URI, resource.Description)
	}

	if len(resourcesResult.Resources) == 0 {
		t.Error("expected at least one resource")
	}
}

func TestListResourceTemplates(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	templatesResult, err := c.ListResourceTemplates(ctx, mcp.ListResourceTemplatesRequest{})
	if err != nil {
		t.Fatalf("failed to list resource templates: %v", err)
	}

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
	if err != nil {
		t.Fatalf("failed to list tools: %v", err)
	}

	t.Logf("Available tools: %d", len(toolsResult.Tools))
	for _, tool := range toolsResult.Tools {
		t.Logf("  - %s: %s", tool.Name, tool.Description)
	}

	if len(toolsResult.Tools) == 0 {
		t.Error("expected at least one tool")
	}
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
	if err != nil {
		t.Fatalf("failed to call SearchChats: %v", err)
	}

	logToolResult(t, result)
}

func TestGetChats(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	readRequest := mcp.ReadResourceRequest{}
	readRequest.Params.URI = "telegram://chats"

	result, err := c.ReadResource(ctx, readRequest)
	if err != nil {
		t.Fatalf("failed to read chats: %v", err)
	}

	if len(result.Contents) == 0 {
		t.Error("expected at least one content item")
	}

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
	if err != nil {
		t.Fatalf("failed to read me: %v", err)
	}

	if len(result.Contents) == 0 {
		t.Error("expected at least one content item")
	}

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
				"chat_id": chatID,
			}

			t.Logf("Calling GetChatInfo with chat_id=%s", chatID)

			result, err := c.CallTool(ctx, callRequest)
			if err != nil {
				t.Fatalf("failed to call GetChatInfo: %v", err)
			}

			logToolResult(t, result)
		})
	}
}

func TestPinnedChatResource(t *testing.T) {
	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	// List resources to find pinned chats
	resourcesResult, err := c.ListResources(ctx, mcp.ListResourcesRequest{})
	if err != nil {
		t.Fatalf("failed to list resources: %v", err)
	}

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
	if err != nil {
		t.Fatalf("failed to read pinned chat resource: %v", err)
	}

	if len(result.Contents) == 0 {
		t.Error("expected at least one content item")
	}

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
				"chat_id": chatID,
				"limit":   10,
			}

			t.Logf("Calling GetMessages with chat_id=%s", chatID)

			result, err := c.CallTool(ctx, callRequest)
			if err != nil {
				t.Fatalf("failed to call GetMessages: %v", err)
			}

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
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
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
				"chat_id":  chatID,
				"filepath": tmpFile,
				"count":    10,
			}

			t.Logf("Calling BackupMessages with chat_id=%s, filepath=%s", chatID, tmpFile)

			result, err := c.CallTool(ctx, callRequest)
			if err != nil {
				t.Fatalf("failed to call BackupMessages: %v", err)
			}

			logToolResult(t, result)

			// Verify a file was written
			content, err := os.ReadFile(tmpFile)
			if err != nil {
				t.Fatalf("failed to read backup file: %v", err)
			}

			t.Logf("Backup file content (%d bytes):\n%s", len(content), string(content))

			if len(content) == 0 {
				t.Error("backup file is empty")
			}
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
			"chat_id":  chatID,
			"filepath": forbiddenPath,
			"count":    10,
		}

		t.Logf("Calling BackupMessages with forbidden path: %s", forbiddenPath)

		result, err := c.CallTool(ctx, callRequest)
		if err != nil {
			t.Fatalf("failed to call BackupMessages: %v", err)
		}

		if !result.IsError {
			t.Error("expected error for forbidden path, but got success")
		}

		// Check an error message contains expected text
		var errorText string
		for _, content := range result.Content {
			if tc, ok := content.(mcp.TextContent); ok {
				errorText = tc.Text
				break
			}
		}
		if !strings.Contains(errorText, "is not within allowed directories") {
			t.Errorf("expected error message to contain 'is not within allowed directories', got: %s", errorText)
		}

		logToolResult(t, result)
	})
}

func TestSummarizeChat(t *testing.T) {
	chatID := os.Getenv("TEST_CHAT_ID")
	if chatID == "" {
		t.Skip("TEST_CHAT_ID not set")
	}

	c, ctx, cleanup := setupClient(t)
	defer cleanup()

	// Extend timeout for summarization
	ctx, extCancel := context.WithTimeout(ctx, 10*time.Minute)
	defer extCancel()

	callRequest := mcp.CallToolRequest{}
	callRequest.Params.Name = "SummarizeChat"
	callRequest.Params.Arguments = map[string]any{
		"chat_id": chatID,
		"goal":    "general context of discussions",
		"period":  "week",
	}

	t.Logf("Calling SummarizeChat with chat_id=%s (this may take a while...)", chatID)

	result, err := c.CallTool(ctx, callRequest)
	if err != nil {
		t.Fatalf("failed to call SummarizeChat: %v", err)
	}

	logToolResult(t, result)
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
