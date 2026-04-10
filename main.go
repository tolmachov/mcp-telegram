package main

import (
	"context"
	"errors"
	"log"
	"os"
	"os/signal"
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

	store, err := config.NewStore()
	if err != nil {
		log.Fatalf("failed to initialize config store: %v", err)
	}
	if err := config.LoadIntoEnv(store); err != nil {
		log.Fatalf("failed to load config from secure store: %v", err)
	}

	if err := internal.New(os.Stdin, os.Stdout, os.Stderr).Run(ctx, os.Args); err != nil {
		log.Fatalf("failed to run: %v", err)
	}
}
