package internal

import (
	"context"
	"fmt"
	"strings"

	"github.com/urfave/cli/v3"

	"github.com/tolmachov/mcp-telegram/internal/config"
)

func configSetAction(_ context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() != 2 {
		return fmt.Errorf("usage: config set <key> <value>")
	}

	key := strings.ToUpper(args.Get(0))

	if err := config.ValidateKey(key); err != nil {
		return err
	}

	store, err := config.NewStore()
	if err != nil {
		return fmt.Errorf("initializing config store: %w", err)
	}
	if err := store.Set(key, args.Get(1)); err != nil {
		return fmt.Errorf("storing config value: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.Root().ErrWriter, "Stored %s securely\n", key)

	return nil
}

func configListAction(_ context.Context, cmd *cli.Command) error {
	store, err := config.NewStore()
	if err != nil {
		return fmt.Errorf("initializing config store: %w", err)
	}

	keys, err := store.List()
	if err != nil {
		return fmt.Errorf("listing config: %w", err)
	}

	if len(keys) == 0 {
		_, _ = fmt.Fprintln(cmd.Root().ErrWriter, "No config values stored")
		return nil
	}

	for _, k := range keys {
		_, _ = fmt.Fprintf(cmd.Root().Writer, "%s = ****\n", k)
	}

	return nil
}

func configDeleteAction(_ context.Context, cmd *cli.Command) error {
	args := cmd.Args()
	if args.Len() != 1 {
		return fmt.Errorf("usage: config delete <key>")
	}

	key := strings.ToUpper(args.Get(0))

	if err := config.ValidateKey(key); err != nil {
		return err
	}

	store, err := config.NewStore()
	if err != nil {
		return fmt.Errorf("initializing config store: %w", err)
	}
	if err := store.Delete(key); err != nil {
		return fmt.Errorf("deleting config value: %w", err)
	}

	_, _ = fmt.Fprintf(cmd.Root().ErrWriter, "Deleted %s\n", key)

	return nil
}
