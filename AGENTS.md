# AGENTS.md

Guidance for coding agents working in this repository. User-facing docs live in
[README.md](README.md); this file only covers what isn't obvious from the code.

## What this is

A Go MCP server for Telegram, built on [gotd/td](https://github.com/gotd/td). It is
primarily a **read-heavy research/summarisation assistant** (search, extraction,
context loading); admin and posting features are secondary. Two transports:

- **stdio** — single local account;
- **streamable HTTP** — embedded OAuth 2.1 server, per-user QR login, multi-user,
  deployable to Cloud Run (`deploy/`).

## Layout

- `main.go` → `internal/app.go` — CLI entry (`run`, `login`, `logout`, `config`).
- `internal/tools` — MCP tools, one file per tool (`Handler` with `Register`).
- `internal/tgdata`, `internal/messages` — Telegram data access and message
  formatting shared by tools.
- `internal/server`, `internal/authsrv` — HTTP transport and OAuth.
- `internal/tgclient`, `internal/sessionstore`, `internal/secret`, `internal/config` —
  Telegram client and credential/session storage: macOS Keychain on darwin,
  file store in `*_other.go` (`//go:build !darwin`).
- `internal/summarize`, `internal/prompts`, `internal/resources`, `internal/completion`.
- `test/` — integration tests (build tag `integration`).

## Adding a tool

1. New file in `internal/tools`: an input struct (descriptions go in `jsonschema`
   tags; closed value sets via `inputSchemaWithEnums`) and a `Register` method.
2. Register with `tools.AddTool`, not `mcp.AddTool` — it keeps error results from
   reaching the client as an empty structured output. Use plain `mcp.AddTool`
   only for tools with no typed output (e.g. `GetMedia`).
3. Add the handler to `buildHandlers` in `internal/server/server.go`: `research`
   for read-only tools, `mutating` for anything that changes state. The split
   drives the server variants (see README "Server Variants").
4. Make the first sentence of `Description` self-contained: the `compact` and
   `research` variants keep only that sentence.
5. Update the tool list and tool counts in README.md.

## Commands

```bash
make build             # binary with version ldflags
make test              # go test ./... (unit only)
make test-integration  # real Telegram account; needs TEST_* vars from .env.example
make lint              # golangci-lint run
make fmt               # golangci-lint fmt
```

## Gotchas

- **Keychain prompts on macOS.** Secret/session-store tests touch the Keychain and
  may prompt. Between steps run targeted tests (`go test ./internal/<pkg>/...`);
  run the full suite once at the end.
- **`!darwin` files aren't linted locally on macOS.** Before pushing, run
  `GOOS=linux golangci-lint run ./...` to match CI (ubuntu, Go 1.26,
  golangci-lint v2.12.2).
- **Manual `run` can hang silently.** A zero-log hang usually means another
  process holds the same Telegram session — kill strays before blaming the change.

## Conventions

- British English in code and comments; `.golangci.yml` whitelists the spellings
  for `misspell`.
- Wrap errors (`wrapcheck`, `errorlint` are enabled).
- Destructive tools take a `confirm` input and go through `confirmDestructive`
  (`internal/tools/tools.go`).
- Single-user project: no migrations or backwards-compat shims. Remove old paths,
  aliases and fallbacks together with their replacement.
