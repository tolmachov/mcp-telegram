<div align="center">

# mcp-telegram

**MCP server for Telegram — let AI assistants interact with your Telegram account**

[![MCP Server](https://badge.mcpx.dev?type=server 'MCP Server')](https://github.com/punkpeye/awesome-mcp-servers)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tolmachov/mcp-telegram)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Report Card](https://goreportcard.com/badge/github.com/tolmachov/mcp-telegram)](https://goreportcard.com/report/github.com/tolmachov/mcp-telegram)
[![mcp-telegram MCP server](https://glama.ai/mcp/servers/tolmachov/mcp-telegram/badges/score.svg)](https://glama.ai/mcp/servers/tolmachov/mcp-telegram)

[![mcp-telegram MCP server](https://glama.ai/mcp/servers/tolmachov/mcp-telegram/badges/card.svg)](https://glama.ai/mcp/servers/tolmachov/mcp-telegram)

</div>

---

## Features

- **Chat Management**: List, search, mute/unmute chats, organize into folders
- **Messages**: Read, search, inspect context, send, draft, schedule, link-resolve, and backup messages
- **AI Summarization**: Summarize chat conversations using multiple LLM providers
- **Secure**: Session stored in macOS Keychain (file-based storage on Linux/Windows)
- **Two transports**: local **stdio** (single account) or remote **streamable HTTP** with an embedded OAuth 2.1 server and per-user Telegram QR login — multi-user and deployable to Cloud Run (see [Remote (HTTP) Mode](#remote-http-mode))

## Installation

```bash
go install github.com/tolmachov/mcp-telegram@latest
```

Or build from source:

```bash
git clone https://github.com/tolmachov/mcp-telegram.git
cd mcp-telegram
make
```

## Setup

### 1. Get Telegram API Credentials

1. Go to [my.telegram.org/apps](https://my.telegram.org/apps)
2. Create an application
3. Copy `api_id` and `api_hash`

### 2. Configure Environment

Store credentials (macOS Keychain; plaintext JSON at `~/.local/state/mcp-telegram/config.json` with `0600` perms on Linux/Windows):

```bash
mcp-telegram config set api-id 123456789
mcp-telegram config set api-hash abcd1234efgh5678
```

Or use a `.env` file:

```bash
cp .env.example .env
# Edit .env with your credentials
```

### 3. Login to Telegram

```bash
mcp-telegram login --phone +1234567890
```

You'll be prompted for a verification code sent to your Telegram.

### 4. Configure MCP Client

#### Claude Desktop

Add to `~/Library/Application Support/Claude/claude_desktop_config.json` (macOS) or `%APPDATA%\Claude\claude_desktop_config.json` (Windows):

```json
{
  "mcpServers": {
    "telegram": {
      "command": "mcp-telegram",
      "args": ["run"],
      "env": {
        "TELEGRAM_API_ID": "your_api_id",
        "TELEGRAM_API_HASH": "your_api_hash"
      }
    }
  }
}
```

#### Claude Code

```bash
claude mcp add telegram -- /path/to/mcp-telegram run
```

Set environment variables in your `.env` file or pass them via `--env`.

## Available Tools

29 tools exposed to MCP clients. Messages are identified by opaque string
handles (`"42"` for regular, `"s:42"` for scheduled) — copy them back
verbatim from tool outputs to follow-up calls, never parse or construct
them manually.

| Tool | Description |
|------|-------------|
| `GetMe` | Get current user information |
| `GetChats` | List all chats, groups, and channels |
| `SearchChats` | Fuzzy search for chats by name |
| `GetChatInfo` | Get detailed information about a chat |
| `GetMessages` | Get messages from a chat (set `include_scheduled=true` to also list pending scheduled messages in a separate field) |
| `SearchMessages` | Search within one chat by substring, with optional date / sender / media / thread filters |
| `SearchMessagesGlobal` | Search by substring across all chats with opaque cursor-based pagination |
| `GetMessageContext` | Get messages around a specific anchor message in chronological order |
| `GetReplies` | Get the messages of a reply thread / comment section under a root message |
| `GetForumTopics` | List a forum supergroup's topics with opaque cursor-based pagination |
| `SendMessage` | Send, reply, schedule, or draft a message. `mode` = `send` (default) / `schedule` / `draft`; `reply_to_message_id` works with any mode; `schedule_at` is RFC3339 |
| `EditMessage` | Edit a message; for scheduled handles, `schedule_at` reschedules delivery in the same call |
| `DeleteMessage` | Delete a message; `"s:<id>"` handles cancel pending scheduled messages |
| `ForwardMessage` | Forward a delivered message (scheduled handles are rejected) |
| `SetReaction` | Set or clear your emoji reactions on a message (empty list clears) |
| `JoinChat` | Join a channel/group/supergroup by @username, numeric ID, or invite link (`t.me/+hash`) |
| `LeaveChat` | Leave a channel/group/supergroup by @username or numeric ID (asks for confirmation) |
| `ResolveMessageLink` | Parse `t.me` / `tg://` message links into `chat_id`, `message_id`, and `topic_message_id` for forum links |
| `MarkAsRead` | Mark one or more chats as read |
| `BackupMessages` | Export messages to a text file. Filters mirror the read tools: `from_date` (inclusive) / `to_date` (exclusive) / `limit` |
| `ResolveUsername` | Resolve @username to user/chat info |
| `SetChatMute` | Mute or unmute chat notifications (`muted` bool + optional `duration_seconds`) |
| `SummarizeChat` | AI-powered chat summarization via sampling / Gemini / Ollama / Anthropic |
| `GetMedia` | Download photo media from a media resource URI; returns MCP image content |
| `GetFolders` | List chat folders (dialog filters) with their ID, title, flags, and included/excluded/pinned chat IDs |
| `CreateFolder` | Create a folder from a title plus chats and/or category flags (e.g. `include_groups`) |
| `DeleteFolder` | Delete a folder by ID; chats are untouched (asks for confirmation) |
| `AddChatsToFolder` | Add chats/groups/channels to a folder by ID (@username or numeric ID) |
| `RemoveChatsFromFolder` | Remove chats/groups/channels from a folder by ID |

## Server Variants

The server implements [experimental server variants](https://github.com/modelcontextprotocol/experimental-ext-variants)
(SEP-2053): one server that offers several selectable capability sets. A client
sends hints during `initialize`, the server ranks the variants, and the client
picks one. Clients that don't understand the extension transparently get the
`full` variant, so nothing changes for them.

| Variant | Status | Tools | For |
|---------|--------|-------|-----|
| `full` | stable (default) | all 29, full descriptions | interactive research + administration with a human |
| `compact` | stable | all 29, descriptions trimmed to the first sentence (~50% smaller) | autonomous agents on a tight context budget |
| `research` | experimental | 16 Telegram read-only tools, descriptions trimmed like `compact` (search, fetch, summarize, export to a local file — no send/edit/delete/forward/react, mark-as-read, join/leave, mute, or folder edits) | read-heavy context-loading agents |

Pin a single variant with `--variant` (or `TELEGRAM_VARIANT`) for clients that
can't negotiate — e.g. `--variant research` exposes only the read-only subset:

```bash
mcp-telegram run --variant research
```

Leave it unset to expose all three and let the client choose.

> **Note on pinned-chat resources.** In multi-variant mode (no `--variant`),
> pinned-chat resources are exposed on every variant and kept fresh by a single
> poller, but proactive `resources/list_changed` notifications are **not**
> delivered — the variants proxy can't forward the background watcher's
> notifications (an upstream library limitation). Clients pick up pin changes on
> their next `resources/list`. Pin a single `--variant` to restore live
> notifications.

## Available Resources

| URI | Description |
|-----|-------------|
| `telegram://me` | Current user info |
| `telegram://chats` | All chats list |
| `telegram://chats/{id}/info` | Detailed info for any chat ID via resource template |
| `telegram://chats/{id}/messages` | Last 100 messages from a pinned chat (dynamic resource, only for currently pinned chats) |

Pinned chat resources are created dynamically for each pinned chat and refreshed in the background; clients receive `resources/list_changed` when the set changes (except in multi-variant mode — see the note under [Server Variants](#server-variants)).

## Available Prompts

3 parameterized prompts that MCP clients expose as slash-commands or quick actions.

| Prompt | Arguments | Description |
|--------|-----------|-------------|
| `daily-digest` | `period` — `day` (default) / `week` / `month` | Walks active chats and produces a per-chat digest of key updates and action items. Read-only. |
| `chat-catchup` | `chat` (required) — ID / @username / title; `period` — `day` / `week` (default) / `month` | Summarizes a specific chat and lists messages that look like they need a reply. Read-only. |
| `find-and-reply` | `chat` (required), `query` (required) — what to search for, `reply` (required) — reply text or instruction | Searches for a message, shows a draft reply, and sends **only after explicit user confirmation**. |

## Prompt Examples

Here are some example prompts you can use with AI assistants:

### Message Management
- "Check for any unread important messages in my Telegram"
- "Summarize all my unread Telegram messages"
- "Read and analyze my unread messages, prepare draft responses where needed"
- "Check non-critical unread messages and give me a brief overview"
- "Find messages mentioning 'invoice' in my work chat from last week"
- "Open the context around this Telegram link: https://t.me/example/123"

### Organization
- "Analyze my Telegram dialogs and suggest a folder structure"
- "Help me categorize my Telegram chats by importance"
- "Find all work-related conversations and suggest how to organize them"

### Communication
- "Monitor specific chat for updates about [topic]"
- "Draft a polite response to the last message in [chat]"
- "Check if there are any unanswered questions in my chats"
- "Resolve this Telegram message link and show me the thread context"

### Backup & Export
- "Backup my conversation with [contact] to a file"
- "Export the last week of messages from [group]"
- "Backup media-only updates too so nothing is silently skipped"

## Chat Summarization

The `SummarizeChat` tool supports multiple LLM providers:

- **sampling** (experimental): Uses the MCP client's LLM via [MCP Sampling](https://modelcontextprotocol.io/docs/concepts/sampling). Only works with clients that support sampling: [VS Code](https://code.visualstudio.com/docs/copilot/chat/mcp-servers), [fast-agent](https://github.com/evalstate/fast-agent), [Continue](https://www.continue.dev). Does NOT work with Claude Desktop or Claude Code.
- **ollama**: Local LLM via [Ollama](https://ollama.ai) - no API key required
- **gemini**: Google Gemini API
- **anthropic**: Anthropic Claude API

Configure via environment variables:

```bash
SUMMARIZE_PROVIDER=ollama  # or: sampling, gemini, anthropic
SUMMARIZE_MODEL=           # provider-specific model name
```

## Commands

```bash
# Run MCP server (used by MCP clients)
mcp-telegram run

# Login to Telegram
mcp-telegram login --phone +1234567890

# Logout and delete session
mcp-telegram logout

# Securely store config values (macOS Keychain / file on Linux)
mcp-telegram config set api-id 123456789
mcp-telegram config set api-hash abcd1234

# List stored keys
mcp-telegram config list

# Delete a stored value
mcp-telegram config delete api-id
```

Allowed keys: `api-id`, `api-hash`, `anthropic`, `gemini`.

Credentials resolve in this priority order (higher wins): CLI flags (`--api-id`, `--api-hash`) → environment variables (including `.env`) → secure store values set via `config set`. This lets you keep stable values in the keychain and override per-run from the command line without editing the store.

## Configuration Options

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `TELEGRAM_API_ID` | Telegram API ID | Required |
| `TELEGRAM_API_HASH` | Telegram API Hash | Required |
| `TELEGRAM_ALLOWED_PATHS` | Allowed directories for backups | OS app data dir |
| `SUMMARIZE_PROVIDER` | LLM provider for summarization | `sampling` (experimental) |
| `SUMMARIZE_MODEL` | Model name | Provider default |
| `SUMMARIZE_BATCH_TOKENS` | Tokens per summarization batch | `8000` |
| `OLLAMA_URL` | Ollama API URL | `http://localhost:11434` |
| `GEMINI_API_KEY` | Google Gemini API key | - |
| `ANTHROPIC_API_KEY` | Anthropic API key | - |
| `TELEGRAM_MEDIA_MAX_BYTES` | Max bytes `GetMedia` will download per call (cap to avoid OOM on large attachments) | `52428800` (50 MiB) |
| `TELEGRAM_RATE_LIMIT_RPS` | RPS ceiling for history-fetching calls to Telegram. Exceeding Telegram's FLOOD_WAIT thresholds pauses all tools. | `0` (safe built-in default) |
| `TELEGRAM_PINNED_REFRESH_SECONDS` | Polling interval (seconds) for the pinned-chat resource watcher. `0` disables the watcher. | `30` |
| `TELEGRAM_FLOOD_WAIT_MAX_SECONDS` | Max seconds to wait out a Telegram `FLOOD_WAIT` before failing fast with a retry-after hint. Keep below your MCP client's tool-call timeout (Claude Desktop ≈ 240s). | `60` |
| `MCP_TRANSPORT` | MCP transport: `stdio` or `http` (streamable HTTP) | `stdio` |
| `MCP_HTTP_ADDR` | Listen address for the HTTP transport | `:8080` |
| `MCP_AUTH` | HTTP authorization mode: `none` or `telegram` | `none` |
| `AUTH_ISSUER_URL` | Public base URL (OAuth issuer); required with `MCP_AUTH=telegram` | - |
| `AUTH_ALLOWED_USERS` | Comma-separated Telegram user ids allowed to log in, or `*` alone to allow any account (only as private as the URL; cannot be mixed with ids); required with `MCP_AUTH=telegram` | - |
| `AUTH_TOKEN_KEY` | Base64 32-byte master key(s) for tokens + session encryption (comma-separated for rotation); required with `MCP_AUTH=telegram` | - |
| `AUTH_ALLOWED_REDIRECTS` | Extra exact-match HTTPS OAuth redirect URIs (loopback and claude.ai/claude.com are always allowed) | - |
| `AUTH_SESSION_BUCKET` / `AUTH_SESSION_DIR` | Where per-user Telegram sessions live (GCS bucket or local dir; exactly one with `MCP_AUTH=telegram`) | - |

## Remote (HTTP) Mode

By default the server speaks the **stdio** transport: one local process, one
Telegram account, driven by a desktop MCP client. It can instead run as a
long-lived remote server over the MCP **streamable HTTP** transport, so a single
hosted deployment serves many users — each authenticating with their own
Telegram account — over the network.

| | stdio (default) | streamable HTTP |
|---|---|---|
| Transport | stdio pipe | HTTP on one endpoint (`POST` for requests, `GET` for the SSE stream) |
| Users | single account on the host | many, isolated per Telegram user |
| Auth | none (local trust) | embedded OAuth 2.1 + Telegram QR login |
| Session storage | Keychain / local file | encrypted per-user blobs in a GCS bucket or a directory |
| Use for | local desktop clients | a shared or hosted server |

### How authorization works

Authorization is pure Telegram — no bots, no external identity provider. The
server embeds a small OAuth 2.1 authorization server (Dynamic Client
Registration + PKCE); when a client connects it opens the authorization page,
which shows a **QR code**. You scan it with the Telegram app (Settings → Devices
→ Link Desktop Device), and the resulting MTProto session both proves who you
are and becomes your working session (a 2FA-password prompt appears if your
account has one). Only Telegram user ids listed in `AUTH_ALLOWED_USERS` may
complete the login — everyone else is rejected after the scan and their session
is discarded.

Each authorization is an **independent session**: it gets its own encrypted
object in the bucket and its own MCP server assembly, so one account can be
logged in from several clients at once without them contending. Sessions are
**encrypted at rest** with AES-256-GCM under a **split key**: the key is derived
from *both* `AUTH_TOKEN_KEY` *and* a random per-session key that lives only
inside the client's OAuth access/refresh token (never stored server-side). As a
result, an at-rest dump of the bucket **plus** the secret manager cannot, on its
own, decrypt a session — a live token is also required. (Trade-offs, stated
plainly: this does not protect against a compromise of the running server's
memory, and a leaked *live* access token combined with bucket read access
exposes that one session, since the token now also carries a decryption key
share. TLS, `Cache-Control: no-store`, and never logging tokens mitigate the
latter.) Pre-existing sessions from older versions stay readable with the master
key alone and are transparently upgraded to a split-key session on their next
token refresh.

Session lifecycle is managed automatically: `POST /revoke` (RFC 7009) with an
access or refresh token durably marks that authorization revoked (a tombstone
stored where the Telegram client never writes) and deletes its session. The
refresh grant dies immediately and stays dead — even if a still-live client
re-stores the session object, the refresh check consults the tombstone, so
revocation cannot be undone by a resurrected blob. An already-issued access
token remains valid until it expires (≤55 minutes): revocation reliably stops
renewal, matching the standard short-lived-access / revocable-refresh model.
Revocation does not terminate the Telegram-side device authorization (it stays
in Settings → Devices until Telegram expires it). Sessions abandoned without
revocation — and old tombstones — are reclaimed by a background sweep once older
than the refresh-token TTL plus a day, past which no refresh token for the
session can still be valid.

Legacy (pre-split-key) sessions are upgraded on their next token refresh by
*moving* the session to a fresh split-key object and revoking the old legacy
session. Consequently, if one account was logged in from several clients before
upgrading, the first client to refresh wins the upgrade and the others re-run
the QR login on their next refresh — this avoids ever running two Telegram
clients on one auth key (which would trip `AUTH_KEY_DUPLICATED`).

### Try it locally

```bash
# Issuer on loopback may use plain http:
mcp-telegram run --transport http --http-addr :8080 \
  --auth telegram \
  --auth-issuer-url http://localhost:8080 \
  --auth-allowed-users 123456789 \
  --auth-token-key "$(head -c 32 /dev/urandom | base64)" \
  --auth-session-dir ~/.local/state/mcp-telegram/http-sessions
```

Then point an MCP client at `http://localhost:8080/` and complete the browser
flow. `--auth none` (the default for HTTP) serves plain streamable HTTP with no
authentication — only safe behind a trusted proxy. Use `*` as the sole entry in
`--auth-allowed-users` to allow **any** Telegram account (the deployment is then
only as private as its URL; `*` cannot be combined with specific ids).

### Deploy to Google Cloud Run

The production target is Cloud Run: one container, scale-to-zero, secrets from
Secret Manager, sessions in a GCS bucket. In outline:

1. **Create a bucket** for the encrypted per-user sessions and a dedicated
   service account with `roles/storage.objectAdmin` on just that bucket.
2. **Store two secrets** in Secret Manager — the Telegram `api_hash` and a
   freshly generated 32-byte `AUTH_TOKEN_KEY`
   (`head -c 32 /dev/urandom | base64`) — and grant the service account
   `roles/secretmanager.secretAccessor` on them. The token key never leaves
   Secret Manager in plaintext. Losing it logs everyone out (sessions become
   undecryptable). Leaking it alone no longer exposes split-key sessions — those
   also need a live client token — but treat it as highly sensitive regardless:
   it still mints tokens and decrypts any not-yet-upgraded legacy sessions.
3. **Deploy** with `gcloud run deploy --source .`, wiring the non-secret env
   from [`deploy/cloudrun.env.example`](deploy/cloudrun.env.example), the two
   secrets via `--set-secrets`, and **`--max-instances=1`** (mandatory: two
   instances loading the same MTProto session trip Telegram's
   `AUTH_KEY_DUPLICATED` and forcibly log every user out).
4. **Set `AUTH_ISSUER_URL`** to the service URL and redeploy — tokens and
   session encryption are bound to the issuer value.

Copy-paste commands, the exact IAM bindings, the token/revocation model, and how
to connect claude.ai / Claude Desktop / Claude Code are in
**[deploy/README.md](deploy/README.md)**.

## Destructive Actions

Tools like `DeleteMessage` request user confirmation via [MCP elicitation](https://modelcontextprotocol.io/docs/concepts/elicitation) before proceeding. If your MCP client does not support elicitation, the server proceeds automatically without a confirmation dialog.

## Session Storage

- **macOS**: Stored securely in Keychain.
- **Linux/Windows**: Stored in `~/.local/state/mcp-telegram/session.json` with `0600` file permissions. The file is **plaintext** — keep the containing user account trusted, and prefer running on macOS when handling sensitive accounts.

Config values set via `mcp-telegram config set` (API keys, Telegram credentials) follow the same backend: Keychain on macOS, plaintext JSON on Linux/Windows.

> **Note on the plaintext store (Linux/Windows):** the session file grants full access to your Telegram account. Place it on an encrypted filesystem (LUKS/BitLocker) and do **not** sync `~/.local/state/mcp-telegram` (or `~/.config`) to an unencrypted cloud backup — a leaked `session.json` is equivalent to a leaked login.

## Development

```bash
make build            # build the binary with version metadata
make lint             # golangci-lint
make test             # unit tests
make test-integration # end-to-end tests (need a real account + TEST_* vars)
```

Unit tests run without credentials. The integration suite in `test/` is behind
the `integration` build tag and drives the server over a real stdio pipe using a
second MCP implementation (mark3labs/mcp-go) as the client, to catch wire-level
interop issues. It reads the `TEST_*` variables documented in `.env.example`
(e.g. `TEST_CHAT_ID`, `TEST_GROUP_ID`) and skips any test whose variable or
Telegram credentials are unset.

## License

[MIT](LICENSE)
