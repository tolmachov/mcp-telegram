package tools

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "normal name",
			input:    "my-chat",
			expected: "my-chat",
		},
		{
			name:     "with slashes",
			input:    "path/to\\file",
			expected: "path_to_file",
		},
		{
			name:     "with special chars",
			input:    "file:name*with?bad<chars>|here",
			expected: "file_name_with_bad_chars__here",
		},
		{
			name:     "with whitespace chars",
			input:    "has\nnew\rlines\tand\ttabs",
			expected: "has_new_lines_and_tabs",
		},
		{
			name:     "leading/trailing spaces and dots",
			input:    "  .file. ",
			expected: "file",
		},
		{
			name:     "empty after sanitize",
			input:    "...",
			expected: "backup",
		},
		{
			name:     "cyrillic name",
			input:    "Привет Мир",
			expected: "Привет Мир",
		},
		{
			name:     "long cyrillic name preserved as runes",
			input:    strings.Repeat("ш", 120),
			expected: strings.Repeat("ш", maxFilenameLength),
		},
		{
			name:     "long ascii name truncated",
			input:    strings.Repeat("a", 120),
			expected: strings.Repeat("a", maxFilenameLength),
		},
		{
			name:     "emoji name",
			input:    "Chat 🌍🌎🌏",
			expected: "Chat 🌍🌎🌏",
		},
		{
			name:     "quotes in name",
			input:    `He said "hello"`,
			expected: "He said _hello_",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := sanitizeFilename(tt.input)
			if result != tt.expected {
				t.Errorf("sanitizeFilename(%q) = %q, want %q", tt.input, result, tt.expected)
			}
		})
	}
}

func TestIsPathAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	if err := os.MkdirAll(allowedDir, 0o755); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name         string
		targetPath   string
		allowedPaths []string
		wantErr      bool
	}{
		{
			name:         "path within allowed dir",
			targetPath:   filepath.Join(allowedDir, "file.txt"),
			allowedPaths: []string{allowedDir},
			wantErr:      false,
		},
		{
			name:         "path in subdirectory",
			targetPath:   filepath.Join(allowedDir, "sub", "file.txt"),
			allowedPaths: []string{allowedDir},
			wantErr:      false,
		},
		{
			name:         "path outside allowed dir",
			targetPath:   filepath.Join(tmpDir, "other", "file.txt"),
			allowedPaths: []string{allowedDir},
			wantErr:      true,
		},
		{
			name:         "directory traversal attempt",
			targetPath:   filepath.Join(allowedDir, "..", "other", "file.txt"),
			allowedPaths: []string{allowedDir},
			wantErr:      true,
		},
		{
			name:         "exact allowed dir path",
			targetPath:   filepath.Join(allowedDir, "backup.txt"),
			allowedPaths: []string{allowedDir},
			wantErr:      false,
		},
		{
			name:         "no allowed paths",
			targetPath:   filepath.Join(allowedDir, "file.txt"),
			allowedPaths: []string{},
			wantErr:      true,
		},
		{
			name:         "multiple allowed paths - second matches",
			targetPath:   filepath.Join(tmpDir, "file.txt"),
			allowedPaths: []string{allowedDir, tmpDir},
			wantErr:      false,
		},
		{
			name:         "double dot in traversal",
			targetPath:   filepath.Join(allowedDir, "..", "..", "etc", "passwd"),
			allowedPaths: []string{allowedDir},
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := isPathAllowed(tt.targetPath, tt.allowedPaths)
			if (err != nil) != tt.wantErr {
				t.Errorf("isPathAllowed(%q, %v) error = %v, wantErr %v", tt.targetPath, tt.allowedPaths, err, tt.wantErr)
			}
		})
	}
}
