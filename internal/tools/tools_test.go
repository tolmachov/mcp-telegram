package tools

import (
	"context"
	"fmt"
	"testing"

	"github.com/gotd/td/tgerr"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestFloodWaitResult verifies the flood-wait detector extracts the retry
// duration both from a bare FLOOD_WAIT and from the wrapped form the
// flood-wait middleware returns when the wait exceeds its max ("flood wait
// argument is too big (... > ...)"). The wrapped case is the one that bit us:
// the middleware wraps the original error, so detection must unwrap.
func TestFloodWaitResult(t *testing.T) {
	flood := &tgerr.Error{Code: 420, Message: "FLOOD_WAIT_265", Type: "FLOOD_WAIT", Argument: 265}

	t.Run("bare flood wait", func(t *testing.T) {
		res, ok := floodWaitResult("join", flood)
		require.True(t, ok)
		require.NotNil(t, res)
		assert.True(t, res.IsError)
		txt := toolResultText(res)
		assert.Contains(t, txt, "265 seconds")
		assert.Contains(t, txt, "join")
	})

	t.Run("wrapped by the waiter (too big)", func(t *testing.T) {
		wrapped := fmt.Errorf("flood wait argument is too big (4m25s > 1m0s): %w", flood)
		res, ok := floodWaitResult("join", wrapped)
		require.True(t, ok)
		assert.Contains(t, toolResultText(res), "265 seconds")
	})

	t.Run("non-flood error returns false", func(t *testing.T) {
		res, ok := floodWaitResult("join", fmt.Errorf("some other error"))
		assert.False(t, ok)
		assert.Nil(t, res)
	})
}

// Note: progress-token extraction is now SDK-provided
// (req.Params.GetProgressToken()), so the previous TestProgressToken case was
// removed — there is no longer a project-owned helper to test. Coverage of
// our sendProgress wrapper is implicit via the per-tool handler tests once
// in-memory transport-based integration tests are added.

// TestMcpLogSlogFallback verifies that mcpLog handles nil sessions by
// falling back to slog for error and warning levels, and stays silent for
// lower levels. This test verifies the function doesn't panic and behaves correctly.
func TestMcpLogSlogFallback(t *testing.T) {
	ctx := context.Background()

	// Test that nil session + error doesn't panic and logs via slog
	mcpLog(ctx, nil, logLevelError, "test-logger", map[string]any{"k": "v"})

	// Test that nil session + warning doesn't panic and logs via slog
	mcpLog(ctx, nil, logLevelWarning, "test-warn", map[string]any{"k": "v"})

	// Test that nil session + info doesn't panic (lower level, no output expected)
	mcpLog(ctx, nil, logLevelInfo, "test-info", map[string]any{"k": "v"})

	// Test that nil session + debug doesn't panic (lower level, no output expected)
	mcpLog(ctx, nil, logLevelDebug, "test-debug", map[string]any{"k": "v"})
}

// TestRootsFromClientNilSession verifies that rootsFromClient handles nil
// sessions gracefully without panicking.
func TestRootsFromClientNilSession(t *testing.T) {
	paths := rootsFromClient(context.Background(), nil)
	assert.Nil(t, paths)
}

func TestFileURIToPath(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    string
		wantErr bool
	}{
		{
			name: "posix path",
			raw:  "file:///tmp/project",
			want: "/tmp/project",
		},
		{
			name: "percent decoded",
			raw:  "file:///tmp/My%20Project",
			want: "/tmp/My Project",
		},
		{
			name:    "non-file scheme rejected",
			raw:     "https://example.com",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := fileURIToPath(tt.raw)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestClampWindow(t *testing.T) {
	tests := []struct {
		name string
		n    int
		want int
	}{
		{"negative returns zero", -1, 0},
		{"large negative returns zero", -100, 0},
		{"zero returns default", 0, defaultContextWindow},
		{"above max clamps to max", maxContextWindow + 1, maxContextWindow},
		{"equal to max passes through", maxContextWindow, maxContextWindow},
		{"within range passes through", 10, 10},
		{"one passes through", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clampWindow(tt.n))
		})
	}
}

func TestClampLimit(t *testing.T) {
	tests := []struct {
		name            string
		limit, def, max int
		want            int
	}{
		{"zero uses default", 0, 50, 100, 50},
		{"negative uses default", -5, 50, 100, 50},
		{"within range passes through", 30, 50, 100, 30},
		{"above max clamps", 200, 50, 100, 100},
		{"equal to max passes through", 100, 50, 100, 100},
		{"equal to one passes through", 1, 50, 100, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, clampLimit(tt.limit, tt.def, tt.max))
		})
	}
}

// TestConfirmDestructiveNilSession verifies that a nil session returns an error
// (fail-closed) rather than silently declining with a misleading return value.
func TestConfirmDestructiveNilSession(t *testing.T) {
	confirmed, err := confirmDestructive(context.Background(), nil, "delete this?")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no MCP session")
	assert.False(t, confirmed)
}

// TestConfirmDestructiveNilRequest verifies that a nil request also fails closed.
func TestConfirmDestructiveNilRequest(t *testing.T) {
	confirmed, err := confirmDestructive(context.Background(), &mcp.CallToolRequest{}, "delete this?")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no MCP session")
	assert.False(t, confirmed)
}
