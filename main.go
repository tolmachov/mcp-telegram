package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/tolmachov/mcp-telegram/internal"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := internal.New(os.Stdin, os.Stdout, os.Stderr).Run(ctx, os.Args); err != nil {
		log.Fatalf("failed to run: %v", err)
	}
}
