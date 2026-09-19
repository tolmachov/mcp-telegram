package tools

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tolmachov/mcp-telegram/internal/xdg"
)

// This file holds the filesystem-sandbox for BackupMessages: default backup
// directory resolution, filename sanitisation, and the allow-list check that
// keeps a backup from being written outside the configured roots (including via
// symlink escape). It is separated from the tool handler because it is
// security-critical and warrants review in isolation.

// maxFilenameLength limits the base filename length for portability
// across filesystems (most support 255 bytes, but we keep it conservative).
const maxFilenameBytes = 180

// DefaultBackupDir keeps backups under the same state root as every other
// persistent artifact.
func DefaultBackupDir() (string, error) {
	stateDir, err := xdg.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(stateDir, "backups"), nil
}

// sanitizeFilename removes or replaces characters that are invalid in filenames.
func sanitizeFilename(name string) string {
	invalid := []string{"/", "\\", ":", "*", "?", "\"", "<", ">", "|", "\n", "\r", "\t"}
	result := name
	for _, char := range invalid {
		result = strings.ReplaceAll(result, char, "_")
	}
	result = strings.Trim(result, " .")
	// Filesystems limit names in bytes, not runes. Keep enough room for the
	// timestamp and extension appended by the caller without splitting UTF-8.
	if len(result) > maxFilenameBytes {
		var limited strings.Builder
		for _, r := range result {
			if limited.Len()+len(string(r)) > maxFilenameBytes {
				break
			}
			limited.WriteRune(r)
		}
		result = limited.String()
	}
	if result == "" {
		result = "backup"
	}
	return result
}

// isPathAllowed checks if the given path is within one of the allowed
// directories. Both sides of the comparison are resolved through
// filepath.EvalSymlinks when the target (or parent) already exists on disk,
// so a symlink placed inside an allowed directory cannot be used to escape
// the sandbox. For paths that don't exist yet (the common case for a new
// backup file) we fall back to evaluating the parent directory — callers are
// expected to sanitise the filename separately via sanitizeFilename.
func isPathAllowed(targetPath string, allowedPaths []string) error {
	absTarget, err := filepath.Abs(targetPath)
	if err != nil {
		return fmt.Errorf("resolving path: %w", err)
	}
	resolvedTarget, err := resolveSymlinks(absTarget)
	if err != nil {
		return fmt.Errorf("resolving target path %q: %w", targetPath, err)
	}

	// Track allowlist entries we had to skip because of unresolvable
	// ancestors; if every entry gets skipped we want the user to see the
	// real configuration error instead of a generic "not within allowed
	// directories" message.
	var skipReasons []string

	for _, allowed := range allowedPaths {
		absAllowed, err := filepath.Abs(allowed)
		if err != nil {
			skipReasons = append(skipReasons, fmt.Sprintf("%q: %v", allowed, err))
			continue
		}
		resolvedAllowed, err := resolveSymlinks(absAllowed)
		if err != nil {
			// If the allowlist entry itself can't be resolved (e.g. EACCES
			// on an ancestor), skip it rather than silently treating the
			// unresolved string as the sandbox root. A misconfigured
			// allowlist shouldn't widen the sandbox.
			skipReasons = append(skipReasons, fmt.Sprintf("%q: %v", allowed, err))
			continue
		}

		rel, err := filepath.Rel(resolvedAllowed, resolvedTarget)
		if err != nil {
			continue
		}

		outside := rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
		if !outside && !filepath.IsAbs(rel) {
			return nil
		}
	}

	if len(allowedPaths) > 0 && len(skipReasons) == len(allowedPaths) {
		return fmt.Errorf("path %q is not within allowed directories: all %d allowlist entries are unresolvable (%s). Fix the configuration so the sandbox roots exist and are readable", targetPath, len(skipReasons), strings.Join(skipReasons, "; "))
	}

	return fmt.Errorf("path %q is not within allowed directories. Configure --allowed-paths or MCP_TELEGRAM_ALLOWED_PATHS", targetPath)
}

// resolveSymlinks returns the fully-resolved absolute path of p. When p
// itself does not exist (e.g. a backup filename that hasn't been written
// yet) it walks up to the first existing ancestor, resolves that, and
// re-attaches the unresolved tail. This keeps "target inside allowed-dir
// via symlink escape" attacks impossible while still allowing isPathAllowed
// to be called before the file exists.
//
// Non-ENOENT errors from EvalSymlinks (typically EACCES on an ancestor) are
// propagated as errors so the sandbox check can fail closed — silently
// reattaching the unresolved tail in that case would let an attacker who
// can place a symlink behind a permission-denied directory bypass the
// sandbox.
func resolveSymlinks(p string) (string, error) {
	p = filepath.Clean(p)
	resolved, err := filepath.EvalSymlinks(p)
	if err == nil {
		return filepath.Clean(resolved), nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("evaluating symlinks for %q: %w", p, err)
	}
	// Path does not exist yet — walk up to the closest existing ancestor,
	// resolve that, and join the unresolved tail back on.
	parent := filepath.Dir(p)
	if parent == p {
		return p, nil
	}
	resolvedParent, err := resolveSymlinks(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolvedParent, filepath.Base(p)), nil
}
