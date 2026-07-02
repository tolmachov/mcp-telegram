package xdg

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// StateDir returns the mcp-telegram state directory, creating it if absent.
// Follows XDG Base Directory spec: $XDG_STATE_HOME/mcp-telegram or
// $HOME/.local/state/mcp-telegram.
func StateDir() (string, error) {
	stateHome := os.Getenv("XDG_STATE_HOME")
	if stateHome == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locating home directory: %w", err)
		}
		if homeDir == "" {
			return "", fmt.Errorf("locating home directory: UserHomeDir returned empty path")
		}
		stateHome = filepath.Join(homeDir, ".local", "state")
	}
	if !filepath.IsAbs(stateHome) {
		return "", fmt.Errorf("state home %q is not absolute", stateHome)
	}

	stateDir := filepath.Join(stateHome, "mcp-telegram")
	//nolint:gosec // G703: stateDir derives from XDG_STATE_HOME/HOME (trusted env), not external request input.
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("creating state directory %s: %w", stateDir, err)
	}
	// MkdirAll honours 0o700 only on directories it creates. If the mcp-telegram
	// dir was restored from a backup or migrated with looser perms, tighten it
	// now — config.json and session.json are 0o600 so contents are unreadable,
	// but a listable dir still leaks which keys exist.
	//nolint:gosec // G302: 0o700 is the correct restrictive mode for a directory (needs the execute bit to be traversable).
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("tightening state directory %s to 0700: %w", stateDir, err)
	}

	return stateDir, nil
}

// WriteFileAtomic writes data to path via a temp file in the same directory
// followed by an atomic rename, so a crash (or SIGKILL) mid-write leaves either
// the old file fully intact or the new one fully written — never a truncated
// mix. The temp file is created with perm and named from tmpPattern (an
// os.CreateTemp pattern, e.g. ".config.*.json.tmp"); it is cleaned up on any
// failure. The parent directory of path must already exist. This is the shared
// implementation behind the file-backed config store and session storage on
// non-darwin platforms.
func WriteFileAtomic(path string, data []byte, perm os.FileMode, tmpPattern string) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, tmpPattern)
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()
	// Best-effort cleanup if any step below fails. On the success path the
	// rename consumes the temp file, so os.ErrNotExist here is expected.
	defer func() {
		if err := os.Remove(tmpPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Debug("cleanup of temp file failed", "path", tmpPath, "err", err)
		}
	}()

	if err := tmp.Chmod(perm); err != nil {
		closeTempFile(tmp, "chmod")
		return fmt.Errorf("setting temp file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		closeTempFile(tmp, "write")
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		closeTempFile(tmp, "sync")
		return fmt.Errorf("syncing temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("renaming temp file into place: %w", err)
	}
	return nil
}

func closeTempFile(f *os.File, stage string) {
	if err := f.Close(); err != nil {
		slog.Debug("closing temp file failed", "stage", stage, "err", err)
	}
}
