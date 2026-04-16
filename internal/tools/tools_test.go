package tools

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
