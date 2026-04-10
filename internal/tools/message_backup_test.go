package tools

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

// TestIsPathAllowedSymlinkEscapes covers the primary threat model for
// resolveSymlinks: an attacker placing a symlink inside an allowed directory
// that points outside must not bypass the sandbox, whether the symlink is
// at the leaf, mid-path, or the target doesn't exist yet.
func TestIsPathAllowedSymlinkEscapes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on Windows; sandbox enforcement is Unix-first")
	}

	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{allowed, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Case a: normal path inside allowlist — should pass.
	inside := filepath.Join(allowed, "ok.txt")
	if err := isPathAllowed(inside, []string{allowed}); err != nil {
		t.Errorf("path inside allowed dir rejected: %v", err)
	}

	// Case b: symlink at the leaf pointing outside. resolveSymlinks must
	// resolve the leaf and notice it lives outside the sandbox.
	leafLink := filepath.Join(allowed, "escape-leaf")
	leafTarget := filepath.Join(outside, "leaked.txt")
	if err := os.WriteFile(leafTarget, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(leafTarget, leafLink); err != nil {
		t.Fatal(err)
	}
	if err := isPathAllowed(leafLink, []string{allowed}); err == nil {
		t.Error("leaf symlink to outside was accepted — sandbox escape")
	}

	// Case c: symlink mid-path pointing outside. Writing `midLink/child.txt`
	// where midLink is a dir-symlink to `outside` must fail.
	midLink := filepath.Join(allowed, "escape-mid")
	if err := os.Symlink(outside, midLink); err != nil {
		t.Fatal(err)
	}
	if err := isPathAllowed(filepath.Join(midLink, "child.txt"), []string{allowed}); err == nil {
		t.Error("mid-path symlink to outside was accepted — sandbox escape")
	}

	// Case d: target does not exist yet (common: new backup file). The
	// parent exists and is inside the sandbox, so the check should pass.
	nonExistent := filepath.Join(allowed, "not-there-yet", "file.txt")
	if err := isPathAllowed(nonExistent, []string{allowed}); err != nil {
		t.Errorf("non-existent leaf inside allowed dir rejected: %v", err)
	}

	// Case e: target equals the allowlist root (rel == ".").
	if err := isPathAllowed(allowed, []string{allowed}); err != nil {
		t.Errorf("allowlist root itself rejected: %v", err)
	}
}

// TestParseDateTimezones pins the UTC-default semantics introduced with the
// RFC3339 upgrade: bare dates and naked timestamps are UTC, RFC3339 with an
// explicit offset is preserved. Regression guard for silent "backup window
// shifted by hours" bugs when the MCP server runs in a different timezone
// than the user.
func TestParseDateTimezones(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantZone *time.Location
		wantTime time.Time
	}{
		{
			name:     "bare date",
			input:    "2026-04-09",
			wantZone: time.UTC,
			wantTime: time.Date(2026, 4, 9, 0, 0, 0, 0, time.UTC),
		},
		{
			name:     "date with time",
			input:    "2026-04-09 15:30:00",
			wantZone: time.UTC,
			wantTime: time.Date(2026, 4, 9, 15, 30, 0, 0, time.UTC),
		},
		{
			name:     "rfc3339 with positive offset",
			input:    "2026-04-09T15:30:00+03:00",
			wantTime: time.Date(2026, 4, 9, 15, 30, 0, 0, time.FixedZone("", 3*60*60)),
		},
		{
			name:     "rfc3339 zulu",
			input:    "2026-04-09T15:30:00Z",
			wantZone: time.UTC,
			wantTime: time.Date(2026, 4, 9, 15, 30, 0, 0, time.UTC),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseDate(tt.input)
			if err != nil {
				t.Fatalf("parseDate(%q) unexpected error: %v", tt.input, err)
			}
			if !got.Equal(tt.wantTime) {
				t.Errorf("parseDate(%q) = %s, want %s", tt.input, got, tt.wantTime)
			}
			if tt.wantZone != nil && got.Location().String() != tt.wantZone.String() {
				t.Errorf("parseDate(%q) zone = %q, want %q", tt.input, got.Location(), tt.wantZone)
			}
		})
	}

	if _, err := parseDate(""); err != nil {
		t.Errorf("parseDate(empty) should return zero time with nil error, got %v", err)
	}
	if _, err := parseDate("not a date"); err == nil {
		t.Error("parseDate should reject garbage input")
	}
}
