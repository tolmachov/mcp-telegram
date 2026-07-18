package internal

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/authsrv"
	"github.com/tolmachov/mcp-telegram/internal/flags"
	"github.com/tolmachov/mcp-telegram/internal/sessionstore"
	"github.com/tolmachov/mcp-telegram/internal/tgid"
)

// runBuildAuthOptions parses args through a cli.Command carrying the auth
// flags and returns whatever buildAuthOptions produced, exercising the real
// flag → config translation.
func runBuildAuthOptions(t *testing.T, args ...string) (*authsrv.Config, sessionstore.Store, error) {
	t.Helper()
	var (
		gotCfg   *authsrv.Config
		gotStore sessionstore.Store
		gotErr   error
	)
	cmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			flags.AuthFlag(), flags.AuthIssuerURLFlag(), flags.AuthAllowedUsersFlag(),
			flags.AuthTokenKeyFlag(), flags.AuthAllowedRedirectsFlag(),
			flags.AuthSessionBucketFlag(), flags.AuthSessionDirFlag(),
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			gotCfg, gotStore, gotErr = buildAuthOptions(ctx, cmd)
			return nil
		},
	}
	// A flag Action (e.g. --auth-allowed-users validation) can reject args at
	// parse time, before the command Action runs. Surface that as the returned
	// error too, so callers see both parse-time and build-time failures.
	if runErr := cmd.Run(t.Context(), append([]string{"test"}, args...)); runErr != nil && gotErr == nil {
		gotErr = runErr
	}
	return gotCfg, gotStore, gotErr
}

func testKey(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestBuildAuthOptionsNoneReturnsNil(t *testing.T) {
	cfg, store, err := runBuildAuthOptions(t, "--auth", "none")
	if err != nil || cfg != nil || store != nil {
		t.Errorf("--auth none = (%v, %v, %v), want (nil, nil, nil)", cfg, store, err)
	}
}

// TestBuildAuthOptionsEncryptsSessions is the load-bearing wiring check: it
// proves production actually wraps the backend in the AEAD layer. If a
// refactor returned the raw backend, sessions would hit disk in plaintext and
// every other test would still pass — so this asserts on the bytes on disk.
func TestBuildAuthOptionsEncryptsSessions(t *testing.T) {
	dir := t.TempDir()
	key := testKey(t)
	cfg, store, err := runBuildAuthOptions(t,
		"--auth", "telegram",
		"--auth-issuer-url", "https://mcp.example.com",
		"--auth-allowed-users", "123456789",
		"--auth-token-key", key,
		"--auth-session-dir", dir,
	)
	if err != nil {
		t.Fatalf("buildAuthOptions: %v", err)
	}
	if cfg == nil || store == nil {
		t.Fatal("expected non-nil config and store for --auth telegram")
	}
	if want := (tgid.UserID(123456789)); cfg.Allow.IsWildcard() || len(cfg.Allow.UserIDs()) != 1 || cfg.Allow.UserIDs()[0] != want {
		t.Errorf("Allow = %+v, want single user [%v]", cfg.Allow, want)
	}

	const user = tgid.UserID(123456789)
	// Exercise the split-key path: an independent session id + per-session key.
	sid := "0123456789abcdef0123456789abcdef"
	sessionKey := []byte("0123456789abcdef0123456789abcdef") // 32 bytes
	plaintext := []byte("SUPER-SECRET-SESSION-BYTES")
	if err := store.Session(user, sid, sessionKey).StoreSession(t.Context(), plaintext); err != nil {
		t.Fatalf("StoreSession: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, user.String()+"."+sid+".bin")) //nolint:gosec // path built from a test-controlled temp dir and numeric id
	if err != nil {
		t.Fatalf("reading session file: %v", err)
	}
	if strings.Contains(string(raw), string(plaintext)) {
		t.Fatal("session persisted in plaintext — the Encrypted wrapper is not applied")
	}

	got, err := store.Session(user, sid, sessionKey).LoadSession(t.Context())
	if err != nil || string(got) != string(plaintext) {
		t.Errorf("round trip = (%q, %v), want (%q, nil)", got, err, plaintext)
	}
}

func TestBuildAuthOptionsWildcard(t *testing.T) {
	cfg, store, err := runBuildAuthOptions(t,
		"--auth", "telegram",
		"--auth-issuer-url", "https://mcp.example.com",
		"--auth-allowed-users", "*",
		"--auth-token-key", testKey(t),
		"--auth-session-dir", t.TempDir(),
	)
	if err != nil {
		t.Fatalf("buildAuthOptions: %v", err)
	}
	if cfg == nil || store == nil {
		t.Fatal("expected non-nil config and store for wildcard")
	}
	if !cfg.Allow.IsWildcard() {
		t.Error("Allow.IsWildcard() = false, want true for --auth-allowed-users '*'")
	}
	if len(cfg.Allow.UserIDs()) != 0 {
		t.Errorf("Allow.UserIDs() = %v, want empty with wildcard", cfg.Allow.UserIDs())
	}
}

func TestBuildAuthOptionsValidation(t *testing.T) {
	baseArgs := []string{
		"--auth", "telegram",
		"--auth-issuer-url", "https://mcp.example.com",
		"--auth-allowed-users", "123",
		"--auth-token-key", testKey(t),
	}
	withStorage := func(extra ...string) []string {
		return append(append([]string{}, baseArgs...), extra...)
	}

	t.Run("bad user id", func(t *testing.T) {
		args := []string{
			"--auth", "telegram", "--auth-issuer-url", "https://mcp.example.com",
			"--auth-allowed-users", "notanumber", "--auth-token-key", testKey(t),
			"--auth-session-dir", t.TempDir(),
		}
		if _, _, err := runBuildAuthOptions(t, args...); err == nil {
			t.Error("accepted a non-numeric user id")
		}
	})

	t.Run("non-positive user id", func(t *testing.T) {
		args := []string{
			"--auth", "telegram", "--auth-issuer-url", "https://mcp.example.com",
			"--auth-allowed-users", "0", "--auth-token-key", testKey(t),
			"--auth-session-dir", t.TempDir(),
		}
		if _, _, err := runBuildAuthOptions(t, args...); err == nil {
			t.Error("accepted user id 0")
		}
	})

	t.Run("wildcard mixed with specific id rejected", func(t *testing.T) {
		args := []string{
			"--auth", "telegram", "--auth-issuer-url", "https://mcp.example.com",
			"--auth-allowed-users", "*,123", "--auth-token-key", testKey(t),
			"--auth-session-dir", t.TempDir(),
		}
		_, _, err := runBuildAuthOptions(t, args...)
		if err == nil {
			t.Fatal("accepted '*' combined with a specific id — silently wide open")
		}
		if !strings.Contains(err.Error(), "cannot be combined") {
			t.Errorf("error = %v, want a 'cannot be combined' message", err)
		}
	})

	t.Run("whitespace user id trimmed", func(t *testing.T) {
		args := []string{
			"--auth", "telegram", "--auth-issuer-url", "https://mcp.example.com",
			"--auth-allowed-users", "  123  ", "--auth-token-key", testKey(t),
			"--auth-session-dir", t.TempDir(),
		}
		cfg, _, err := runBuildAuthOptions(t, args...)
		if err != nil || cfg == nil || len(cfg.Allow.UserIDs()) != 1 || cfg.Allow.UserIDs()[0] != tgid.UserID(123) {
			t.Errorf("whitespace id not trimmed: cfg=%v err=%v", cfg, err)
		}
	})

	t.Run("both bucket and dir", func(t *testing.T) {
		args := withStorage("--auth-session-dir", t.TempDir(), "--auth-session-bucket", "b")
		if _, _, err := runBuildAuthOptions(t, args...); err == nil {
			t.Error("accepted both --auth-session-dir and --auth-session-bucket")
		}
	})

	t.Run("neither bucket nor dir", func(t *testing.T) {
		if _, _, err := runBuildAuthOptions(t, baseArgs...); err == nil {
			t.Error("accepted --auth telegram with no session storage")
		}
	})

	t.Run("invalid issuer surfaced", func(t *testing.T) {
		args := []string{
			"--auth", "telegram", "--auth-issuer-url", "http://not-loopback.example.com",
			"--auth-allowed-users", "123", "--auth-token-key", testKey(t),
			"--auth-session-dir", t.TempDir(),
		}
		if _, _, err := runBuildAuthOptions(t, args...); err == nil {
			t.Error("accepted a non-https, non-loopback issuer")
		}
	})
}
