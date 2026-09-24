package tools

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPartialBackupFailure pins that a timed-out backup still tells the model
// how to shrink the request: a timeout is systemic, so failureText drops the
// hint, and the retry advice must travel in the note.
func TestPartialBackupFailure(t *testing.T) {
	timedOut := failureText("BackupMessages", partialBackupFailure("back up chat 1",
		fmt.Errorf("fetching batch: %w", context.DeadlineExceeded), 42, "/backups/chat.txt"))
	assert.Contains(t, timedOut, "The backup timed out; a partial file with 42 messages was saved to /backups/chat.txt.")
	assert.Contains(t, timedOut, "narrower date window or a smaller limit")
	assert.NotContains(t, timedOut, "mid-stream")

	broken := failureText("BackupMessages", partialBackupFailure("back up chat 1",
		errors.New("connection reset"), 42, "/backups/chat.txt"))
	assert.Contains(t, broken, "The backup stopped mid-stream; a partial file with 42 messages was saved to /backups/chat.txt.")
	assert.Contains(t, broken, "Retry with a narrower date window or resume from the last saved message.")
}

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
			expected: strings.Repeat("ш", maxFilenameBytes/len("ш")),
		},
		{
			name:     "long ascii name truncated",
			input:    strings.Repeat("a", maxFilenameBytes+40),
			expected: strings.Repeat("a", maxFilenameBytes),
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
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestIsPathAllowed(t *testing.T) {
	tmpDir := t.TempDir()
	allowedDir := filepath.Join(tmpDir, "allowed")
	err := os.MkdirAll(allowedDir, 0o750)
	require.NoError(t, err)

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
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
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
		err := os.MkdirAll(d, 0o750)
		require.NoError(t, err)
	}

	// Case a: normal path inside allowlist — should pass.
	inside := filepath.Join(allowed, "ok.txt")
	err := isPathAllowed(inside, []string{allowed})
	assert.NoError(t, err)

	// Case b: symlink at the leaf pointing outside. resolveSymlinks must
	// resolve the leaf and notice it lives outside the sandbox.
	leafLink := filepath.Join(allowed, "escape-leaf")
	leafTarget := filepath.Join(outside, "leaked.txt")
	err = os.WriteFile(leafTarget, []byte("x"), 0o600)
	require.NoError(t, err)
	err = os.Symlink(leafTarget, leafLink)
	require.NoError(t, err)
	err = isPathAllowed(leafLink, []string{allowed})
	assert.Error(t, err, "leaf symlink to outside was accepted — sandbox escape")

	// Case c: symlink mid-path pointing outside. Writing `midLink/child.txt`
	// where midLink is a dir-symlink to `outside` must fail.
	midLink := filepath.Join(allowed, "escape-mid")
	err = os.Symlink(outside, midLink)
	require.NoError(t, err)
	err = isPathAllowed(filepath.Join(midLink, "child.txt"), []string{allowed})
	assert.Error(t, err, "mid-path symlink to outside was accepted — sandbox escape")

	// Case d: target does not exist yet (common: new backup file). The
	// parent exists and is inside the sandbox, so the check should pass.
	nonExistent := filepath.Join(allowed, "not-there-yet", "file.txt")
	err = isPathAllowed(nonExistent, []string{allowed})
	assert.NoError(t, err)

	// Case e: target equals the allowlist root (rel == ".").
	err = isPathAllowed(allowed, []string{allowed})
	assert.NoError(t, err)
}

// TestIsPathAllowedFailsClosedOnUnreadableAncestor pins the documented security
// decision in resolveSymlinks: an EACCES while resolving an ancestor must
// propagate as an error (fail closed), never silently reattach the unresolved
// tail. A regression that "fixed" the spurious permission error by falling
// through would reopen the symlink-behind-a-locked-dir bypass, and this test —
// on both the target side and the allowlist side — would catch it.
func TestIsPathAllowedFailsClosedOnUnreadableAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permission-bit semantics differ on Windows; sandbox is Unix-first")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root bypasses permission bits, so EACCES can't be provoked")
	}

	root := t.TempDir()
	allowed := filepath.Join(root, "allowed")
	locked := filepath.Join(allowed, "locked")
	require.NoError(t, os.MkdirAll(locked, 0o750))
	// Drop all permissions on `locked` so traversing into it yields EACCES.
	require.NoError(t, os.Chmod(locked, 0o000))
	// Restore search/exec bits so t.TempDir's recursive cleanup can descend.
	// 0o750 (>0600) is required for a directory; safe here in test scaffolding.
	t.Cleanup(func() { _ = os.Chmod(locked, 0o750) }) //nolint:gosec // G302: directory needs exec bit

	// Target side: the target lives behind the unreadable directory, so
	// resolving it must error rather than pass the sandbox check.
	target := filepath.Join(locked, "sub", "file.txt")
	err := isPathAllowed(target, []string{allowed})
	require.Error(t, err, "target behind an unreadable ancestor must not be allowed")
	assert.Contains(t, err.Error(), "resolving target path")

	// Allowlist side: an allowlist entry behind the unreadable directory must
	// be skipped (not treated as a sandbox root), and with every entry skipped
	// the caller gets the real "unresolvable" configuration error.
	validTarget := filepath.Join(allowed, "ok.txt")
	err = isPathAllowed(validTarget, []string{filepath.Join(locked, "subroot")})
	require.Error(t, err, "unresolvable allowlist entry must not widen the sandbox")
	assert.Contains(t, err.Error(), "unresolvable")
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
			require.NoError(t, err)
			assert.True(t, got.Equal(tt.wantTime))
			if tt.wantZone != nil {
				assert.Equal(t, tt.wantZone.String(), got.Location().String())
			}
		})
	}

	_, err := parseDate("")
	assert.NoError(t, err)

	_, err = parseDate("not a date")
	assert.Error(t, err)
}

func TestDefaultBackupDir(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	dir, err := DefaultBackupDir()
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(stateHome, "mcp-telegram", "backups"), dir)
}
