package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/tolmachov/mcp-telegram/internal"
	"github.com/tolmachov/mcp-telegram/internal/config"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.Fatalf("failed to load .env file: %v", err)
	}

	// Merge the secure store into the environment only for the commands that
	// actually consume Telegram credentials. The store's first read triggers a
	// macOS Keychain prompt, so `config …` (which manages the store directly),
	// `--help`, and `--version` must not touch it. This has to run before the
	// CLI parses flags, since credentials arrive via env-sourced flags.
	if commandNeedsSecureConfig(os.Args) {
		store, err := config.NewStore()
		if err != nil {
			log.Fatalf("failed to initialize config store: %v", err)
		}
		if err := config.LoadIntoEnv(store); err != nil {
			log.Fatalf("failed to load config from secure store: %v", err)
		}
	}

	if err := internal.New(os.Stdin, os.Stdout, os.Stderr).Run(ctx, os.Args); err != nil {
		log.Fatalf("failed to run: %v", err)
	}
}

// commandNeedsSecureConfig reports whether the invoked subcommand reads
// credentials from the secure store. Only run/login/logout do; config manages
// the store itself and help/version need nothing. The subcommand is the first
// non-flag argument — which is unambiguous only because the root command
// defines no global flags, so no flag value can appear before the subcommand
// name. If a root-level flag with a value is ever added, switch this to match
// against a known-command set instead of "first non-flag token wins".
func commandNeedsSecureConfig(args []string) bool {
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		switch a {
		case "run", "login", "logout":
			return true
		default:
			return false
		}
	}
	return false
}
