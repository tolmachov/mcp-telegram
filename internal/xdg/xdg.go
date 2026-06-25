package xdg

import (
	"fmt"
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
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("creating state directory %s: %w", stateDir, err)
	}
	// MkdirAll honours 0o700 only on directories it creates. If the mcp-telegram
	// dir was restored from a backup or migrated with looser perms, tighten it
	// now — config.json and session.json are 0o600 so contents are unreadable,
	// but a listable dir still leaks which keys exist.
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return "", fmt.Errorf("tightening state directory %s to 0700: %w", stateDir, err)
	}

	return stateDir, nil
}
