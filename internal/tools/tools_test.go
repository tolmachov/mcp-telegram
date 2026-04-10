package tools

import (
	"testing"
)

// Note: progress-token extraction is now SDK-provided
// (req.Params.GetProgressToken()), so the previous TestProgressToken case was
// removed — there is no longer a project-owned helper to test. Coverage of
// our sendProgress wrapper is implicit via the per-tool handler tests once
// in-memory transport-based integration tests are added.

func TestClampLimit(t *testing.T) {
	tests := []struct {
		name              string
		limit, def, max   int
		want              int
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
			if got := clampLimit(tt.limit, tt.def, tt.max); got != tt.want {
				t.Errorf("clampLimit(%d, %d, %d) = %d, want %d", tt.limit, tt.def, tt.max, got, tt.want)
			}
		})
	}
}

func TestTruncateRunes(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		n        int
		expected string
	}{
		{
			name:     "empty string",
			input:    "",
			n:        10,
			expected: "",
		},
		{
			name:     "shorter than limit",
			input:    "hello",
			n:        10,
			expected: "hello",
		},
		{
			name:     "exact length",
			input:    "hello",
			n:        5,
			expected: "hello",
		},
		{
			name:     "longer than limit",
			input:    "hello world",
			n:        5,
			expected: "hello...",
		},
		{
			name:     "cyrillic shorter",
			input:    "привет",
			n:        10,
			expected: "привет",
		},
		{
			name:     "cyrillic exact",
			input:    "привет",
			n:        6,
			expected: "привет",
		},
		{
			name:     "cyrillic truncate",
			input:    "привет мир",
			n:        6,
			expected: "привет...",
		},
		{
			name:     "emoji truncate",
			input:    "hello 🌍🌎🌏 world",
			n:        8,
			expected: "hello 🌍🌎...",
		},
		{
			name:     "mixed unicode",
			input:    "hello привет 世界",
			n:        10,
			expected: "hello прив...",
		},
		{
			name:     "zero limit",
			input:    "hello",
			n:        0,
			expected: "...",
		},
		{
			name:     "limit one",
			input:    "hello",
			n:        1,
			expected: "h...",
		},
		{
			name:     "chinese characters",
			input:    "你好世界",
			n:        2,
			expected: "你好...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := truncateRunes(tt.input, tt.n)
			if result != tt.expected {
				t.Errorf("truncateRunes(%q, %d) = %q, want %q", tt.input, tt.n, result, tt.expected)
			}
		})
	}
}
