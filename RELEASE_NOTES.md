# Production-hardening release

This release intentionally breaks every previous HTTP authorization, storage,
configuration, and pagination contract.

- HTTP always uses embedded Telegram OAuth and isolated per-authorization
  sessions. There is no `--auth`, `MCP_AUTH`, or unauthenticated HTTP mode.
- OAuth codes are durable and single-use. Refresh tokens rotate by atomic
  generation; replay revokes the complete grant and Telegram session.
- Current HTTP storage prefixes are `sessions-v3/`, `revoked-v3/`, and
  `oauth-v3/grants/`. Earlier objects are never read or migrated.
- The macOS Keychain service and local session/config paths are versioned. The
  previous aggregate Keychain blob and config JSON are ignored.
- Message/forum continuations use versioned opaque cursors. Earlier cursors are
  rejected.
- Destructive/publishing tools require explicit `confirm: true`.
- Client MCP roots no longer affect filesystem access. Backup is stdio-only and
  restricted to server-configured roots.
- Environment names now use `MCP_*`, `MCP_AUTH_*`, and `MCP_SUMMARIZE_*`.
  `.env` is never loaded automatically.
- The `sampling` summarisation provider is removed (MCP deprecated sampling in
  SEP-2577). Summarisation is off until `MCP_SUMMARIZE_PROVIDER` names
  `anthropic`, `gemini` or `ollama`.
- The server sends no MCP logging notifications and no longer advertises the
  `logging` capability; diagnostics go to the server log.
- Clients no longer negotiate server variants (SEP-2053): `--variant` /
  `MCP_VARIANT` picks one, `full` by default.
- Release containers compile with the checksummed Go 1.27.1 toolchain, upgrade
  Alpine 3.24 security packages, and use gRPC 1.84.0 or newer.

All users must reconfigure current environment names and complete a new QR
login after upgrading. Keep the prior image and old storage prefixes through
the rollback window; delete old objects only after the new release passes the
manual Telegram/Cloud Run smoke described in `deploy/README.md`.
