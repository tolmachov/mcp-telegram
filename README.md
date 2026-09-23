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
- **Secure storage**: separate versioned Keychain items on macOS; atomic `0600`
  session/config files in a `0700` state directory on Linux and Windows
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

Store credentials (one versioned Keychain item per value on macOS; one atomic
`0600` file per value in a `0700` state directory on Linux/Windows):

```bash
mcp-telegram config set api-id 123456789
mcp-telegram config set api-hash abcd1234efgh5678
```

Or export the current environment names explicitly:

```bash
export MCP_TELEGRAM_API_ID=123456789
export MCP_TELEGRAM_API_HASH=abcd1234efgh5678
```

### 3. Login to Telegram

```bash
mcp-telegram login --phone +1234567890
```

You'll be prompted for the verification code sent by Telegram and, when the
account has two-step verification enabled, its 2FA password.

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
        "MCP_TELEGRAM_API_ID": "your_api_id",
        "MCP_TELEGRAM_API_HASH": "your_api_hash"
      }
    }
  }
}
```

#### Claude Code

```bash
claude mcp add telegram -- /path/to/mcp-telegram run
```

Set process environment variables or pass them through your MCP host. Local
`.env` files are not loaded automatically.

## Available Tools

The `full` stdio surface exposes 29 tools (`full` over HTTP exposes 28 because
filesystem backup is local-only). Messages are identified by opaque string
handles (`"42"` for regular, `"s:42"` for scheduled) — copy them back
verbatim from tool outputs to follow-up calls, never parse or construct
them manually.

If your client shows a single tool called `TelegramLoginRequired` and none of
the tools below, the server is up but Telegram is not authorized — see
[When Telegram Isn't Authorized](#when-telegram-isnt-authorized).

| Tool | Description |
|------|-------------|
| `GetMe` | Get current user information |
| `GetChats` | List all chats, groups, and channels |
| `SearchChats` | Search local/global chats by title, username, or numeric ID, ranked with one normalized distance score |
| `GetChatInfo` | Get detailed information about a chat |
| `GetMessages` | Get messages from a chat (set `include_scheduled=true` to also list pending scheduled messages in a separate field) |
| `SearchMessages` | Search within one chat by substring, with optional date / sender / media / thread filters |
| `SearchMessagesGlobal` | Search by substring across all chats with opaque cursor-based pagination |
| `GetMessageContext` | Get messages around a specific anchor message in chronological order |
| `GetReplies` | Get the messages of a reply thread / comment section under a root message |
| `GetForumTopics` | List a forum supergroup's topics with opaque cursor-based pagination |
| `SendMessage` | Send, reply, schedule, or draft a message. `mode` = `send` (default) / `schedule` / `draft`; `reply_to_message_id` works with any mode; `schedule_at` is RFC3339; text is limited to 4096 UTF-16 code units |
| `EditMessage` | Edit a message within the same 4096 UTF-16 limit; for scheduled handles, `schedule_at` reschedules delivery in the same call |
| `DeleteMessages` | Delete up to 100 same-kind messages per call with per-message `deleted` / `not_found` / `forbidden` / `unverified` outcomes; `"s:<id>"` handles cancel scheduled messages; requires `confirm: true` |
| `ForwardMessage` | Forward a delivered message; scheduled handles are rejected and `confirm: true` is required |
| `SetReaction` | Set or clear your emoji reactions on a message (empty list clears) |
| `JoinChat` | Join a channel/group/supergroup by @username, numeric ID, or invite link (`t.me/+hash`) |
| `LeaveChat` | Leave a channel/group/supergroup by @username or numeric ID (requires `confirm: true`) |
| `ResolveMessageLink` | Parse `t.me` / `tg://` message links into `chat_id`, `message_id`, and `topic_message_id` for forum links |
| `MarkAsRead` | Mark one or more chats as read |
| `BackupMessages` | Local stdio only: atomically export messages under a server-configured path. Never exposed over HTTP |
| `ResolveUsername` | Resolve @username to user/chat info |
| `SetChatMute` | Mute or unmute chat notifications (`muted` bool + optional `duration_seconds`) |
| `SummarizeChat` | AI-powered summarization via sampling / Gemini / Ollama / Anthropic; processes at most `max_messages` (default 2000, hard maximum 10000) and reports truncation/partial status |
| `GetMedia` | Download photo media from a media resource URI; returns MCP image content |
| `GetFolders` | List chat folders (dialog filters) with their ID, title, flags, and included/excluded/pinned chat IDs |
| `CreateFolder` | Create a folder from a title plus chats and/or category flags (e.g. `include_groups`) |
| `DeleteFolder` | Delete a folder by ID; chats are untouched (requires `confirm: true`) |
| `AddChatsToFolder` | Add chats/groups/channels to a folder by ID (@username or numeric ID) |
| `RemoveChatsFromFolder` | Remove chats/groups/channels from a folder by ID |

### Pagination, dates, and identifiers

- `GetMessages`, `SearchMessages`, and `GetReplies` accept filters only on the
  first call. For the next page, copy `next_cursor` into `cursor` and omit every
  other field. `GetForumTopics` follows the same cursor-only continuation rule.
- `before_message_id` is an optional first-page anchor, distinct from the
  continuation cursor. Scheduled handles cannot be pagination anchors.
- Cursors are opaque and versioned. Do not decode, modify, or construct them;
  cursors from older or newer incompatible schemas are rejected.
- Message date windows are consistently `[from_date, to_date)`:
  `from_date` is inclusive and `to_date` is exclusive. Both are RFC3339; an
  empty or reversed window is a validation error. To include a calendar day,
  use midnight of the following day as `to_date`.
- IDs returned in message DTOs and pinned resources use the same opaque string
  handle format. Pass them back exactly as returned.

## Server Variants

The server implements [experimental server variants](https://github.com/modelcontextprotocol/experimental-ext-variants)
(SEP-2053): one server that offers several selectable capability sets. A client
sends hints during `initialize`, the server ranks the variants, and the client
picks one. Clients that don't understand the extension transparently get the
`full` variant, so nothing changes for them.

| Variant | Status | Tools | For |
|---------|--------|-------|-----|
| `full` | stable (default) | all available tools (29 over stdio, 28 over HTTP), full descriptions | interactive research + administration with a human |
| `compact` | stable | all available tools (29 over stdio, 28 over HTTP), descriptions trimmed to the first sentence (~50% smaller) | autonomous agents on a tight context budget |
| `research` | experimental | Telegram read-only tools, descriptions trimmed like `compact` (search, fetch, summarize — no filesystem backup or Telegram mutations) | read-heavy context-loading agents; summarization may send chat data to the configured external LLM |

Pin a single variant with `--variant` (or `MCP_VARIANT`) for clients that
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
| `telegram://chats/{chat_id}/info` | Detailed info for any chat ID via resource template |
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

These examples apply only to local stdio `full`/`compact` mode; HTTP and the
`research` variant never expose filesystem backup:

- "Backup my conversation with [contact] to a file"
- "Export the last week of messages from [group]"
- "Backup media-only updates too so nothing is silently skipped"

## Chat Summarization

The `SummarizeChat` tool supports multiple LLM providers:

- **sampling** (experimental): Uses the MCP client's LLM via [MCP Sampling](https://modelcontextprotocol.io/docs/concepts/sampling). Only works with clients that support sampling: [VS Code](https://code.visualstudio.com/docs/copilot/chat/mcp-servers), [fast-agent](https://github.com/evalstate/fast-agent), [Continue](https://www.continue.dev). Does NOT work with Claude Desktop or Claude Code.
- **ollama**: Local LLM via [Ollama](https://ollama.ai) - no API key required
- **gemini**: Google Gemini API
- **anthropic**: Anthropic Claude API

`max_messages` defaults to 2000 and cannot exceed 10000. Fetching is bounded
and paged in batches of 100; if a later Telegram page fails, the tool summarizes
the messages already fetched and marks the result as degraded. Every response
includes `messages_processed` and `truncated`, with `partial` and `warning` when
applicable.

Chat messages are serialized as untrusted JSON data, separate from the system
instructions, goal, and previous rolling summary. The server explicitly marks
Telegram content as untrusted and tells the provider not to execute instructions
found inside it. Sampling sends selected text to the MCP client's LLM; Gemini
and Anthropic send it to their APIs; Ollama sends it to the configured URL,
which may itself be remote. Providers may log, retain, or bill for content under
their own policies. Retryable `429`/`5xx` responses are attempted at most three
times with backoff and `Retry-After`, without exceeding the MCP request
deadline.

Configure via environment variables:

```bash
MCP_SUMMARIZE_PROVIDER=ollama  # or: sampling, gemini, anthropic
MCP_SUMMARIZE_MODEL=           # provider-specific model name
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

`logout` always closes the local client and removes the local session. If
Telegram is offline, the command reports the remote logout failure together
with any local cleanup failure instead of leaving the local credential behind.

For storable credentials, configuration resolves in this priority order: CLI
flags → process environment → secure store → defaults. Other runtime settings
resolve as CLI flags → process environment → defaults. The binary never reads
`.env` files and never mutates the process environment.

## Configuration Options

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `MCP_TELEGRAM_API_ID` | Telegram API ID | Required for login/HTTP; missing stdio credentials expose login-required mode |
| `MCP_TELEGRAM_API_HASH` | Telegram API Hash | Required for login/HTTP; missing stdio credentials expose login-required mode |
| `MCP_TELEGRAM_ALLOWED_PATHS` | Server-owned backup roots; client MCP roots are ignored | OS state backup dir in stdio `full`/`compact`; unused in HTTP/research |
| `MCP_SUMMARIZE_PROVIDER` | LLM provider for summarization | `sampling` |
| `MCP_SUMMARIZE_MODEL` | Model name | Provider default |
| `MCP_SUMMARIZE_BATCH_TOKENS` | Tokens per summarization batch (must be positive) | `8000` |
| `MCP_SUMMARIZE_OLLAMA_URL` | Ollama API URL | `http://localhost:11434` |
| `MCP_SUMMARIZE_GEMINI_API_KEY` | Google Gemini API key | - |
| `MCP_SUMMARIZE_ANTHROPIC_API_KEY` | Anthropic API key | - |
| `MCP_TELEGRAM_MEDIA_MAX_BYTES` | Max bytes `GetMedia` downloads; `0` or less removes the cap | `52428800` |
| `MCP_TELEGRAM_RATE_LIMIT_RPS` | Telegram history RPS ceiling (must be positive) | `1` |
| `MCP_TELEGRAM_PINNED_REFRESH_SECONDS` | Pinned resource polling interval; `0` disables the watcher | `30` |
| `MCP_TELEGRAM_FLOOD_WAIT_MAX_SECONDS` | Maximum handled `FLOOD_WAIT` (must be positive) | `60` |
| `MCP_VARIANT` | Pin `full`, `compact`, or `research`; empty enables negotiation | empty |
| `MCP_TRANSPORT` | MCP transport: `stdio` or `http` (streamable HTTP) | `stdio` |
| `MCP_HTTP_ADDR` | Explicit listen address for HTTP; when unset on Cloud Run, the server binds to injected `:$PORT` | `127.0.0.1:8080` |
| `MCP_LOG_FORMAT` | `text` or GCP-compatible structured `json` | `text` for stdio, `json` for HTTP |
| `MCP_LOG_LEVEL` | `debug`, `info`, `warn`, or `error` | `info` |
| `MCP_AUTH_ISSUER_URL` | Public OAuth issuer/resource URL, without a trailing slash; required for HTTP | - |
| `MCP_AUTH_ALLOWED_USERS` | Allowed Telegram user IDs, or `*` alone | - |
| `MCP_AUTH_TOKEN_KEYS` | Base64 32-byte master keys, first seals and all verify | - |
| `MCP_AUTH_ALLOWED_REDIRECTS` | Extra exact HTTPS redirect URIs | - |
| `MCP_AUTH_TRUSTED_PROXY_HOPS` | Trusted rightmost proxy hops; zero ignores forwarding headers | `0` |
| `MCP_AUTH_SESSION_BUCKET` / `MCP_AUTH_SESSION_DIR` | Exactly one HTTP session backend | - |

`.env.example` is documentation only. Export values in the actual server
process, pass flags, or use `config set` for supported credentials. On Cloud
Run, `K_SERVICE` causes the application to validate and consume the platform's
injected `PORT`; do not put `$PORT` inside `MCP_HTTP_ADDR` because Cloud Run does
not expand environment-variable references there.

## Remote (HTTP) Mode

By default the server speaks the **stdio** transport: one local process, one
Telegram account, driven by a desktop MCP client. It can instead run as a
long-lived remote server over the MCP **streamable HTTP** transport, so a single
hosted deployment serves many users — each authenticating with their own
Telegram account — over the network.

| | stdio (default) | streamable HTTP |
|---|---|---|
| Transport | stdio pipe | HTTP on one endpoint (`POST` for requests, `GET` for the SSE stream) |
| Users | single account on the host | many, isolated per OAuth authorization |
| Auth | local process boundary; no OAuth | mandatory embedded OAuth 2.1 + Telegram QR login |
| Session storage | Keychain / local file | encrypted per-user blobs in a GCS bucket or a directory |
| Use for | local desktop clients | a shared or hosted server |

### How authorization works

Authorization is pure Telegram — no bots, no external identity provider. The
server embeds a small OAuth 2.1 authorization server (Dynamic Client
Registration + PKCE); when a client connects it opens the authorization page,
which shows a **QR code**. You scan it with the Telegram app (Settings → Devices
→ Link Desktop Device), and the resulting MTProto session both proves who you
are and becomes your working session (a 2FA-password prompt appears if your
account has one). Only Telegram user ids listed in `MCP_AUTH_ALLOWED_USERS` may
complete the login — everyone else is rejected after the scan and their session
is discarded.

Each authorization is an **independent session**: it gets its own encrypted
record in the selected backend and its own MCP server assembly, so one account
can be logged in from several clients at once without them contending. The
server keeps up to four assemblies per account and 25 globally. An account
actively switching among more than four authorizations is not rejected, but its
least-recently-used assembly is evicted and rebuilt on demand. Sessions are
**encrypted at rest** with AES-256-GCM under a **split key**: the key is derived
from *both* `MCP_AUTH_TOKEN_KEYS` *and* a random per-session key that lives only
inside the client's OAuth access/refresh token (never stored server-side). As a
result, an at-rest dump of the bucket **plus** the secret manager cannot, on its
own, decrypt a session — a live token is also required. (Trade-offs, stated
plainly: this does not protect against a compromise of the running server's
memory. And because the per-session key share travels inside the token, an
attacker who holds the bucket **and** the master key **and** captures **one**
access token can decrypt that single session — and thereby extract its
persistent MTProto auth key, which outlives the ≤55-minute token and keeps
working until the session is revoked or logged out. That exposure is confined to
the one captured session; other sessions stay protected. TLS,
`Cache-Control: no-store` on token/revoke responses, and never logging tokens
narrow the capture window.) Old session formats are intentionally unreadable.

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

Authorization codes are durable single-use grants. Refresh tokens rotate by
atomic family generation; reuse of an older generation revokes the entire
family, Telegram session, and live pooled client. Backend failures return `503`
without consuming or rotating a grant.

The HTTP boundary limits MCP bodies to 1 MiB, headers to 32 KiB, and concurrent
requests (including SSE streams) to 128. Header/read/idle timeouts are
10s/30s/60s; MCP sessions expire after 10 minutes. Creating sessionless MCP
sessions is limited per Telegram user to 10 per minute with burst 3. By default
`MCP_AUTH_TRUSTED_PROXY_HOPS=0`, so forwarded-address headers are ignored;
configure a positive value only when the complete trusted proxy chain is known.
Origin protection remains enabled around the authenticated MCP endpoint to
block cross-origin and DNS-rebinding requests.

### Try it locally

```bash
# Issuer on loopback may use plain http:
mcp-telegram run --transport http --http-addr 127.0.0.1:8080 \
  --auth-issuer-url http://localhost:8080 \
  --auth-allowed-users 123456789 \
  --auth-token-key "$(head -c 32 /dev/urandom | base64)" \
  --auth-session-dir ~/.local/state/mcp-telegram/http-sessions
```

Then point an MCP client at `http://localhost:8080/` and complete the browser
flow. HTTP cannot start without OAuth configuration and exactly one durable
session backend. Use `*` as the sole entry in
`--auth-allowed-users` to allow **any** Telegram account (the deployment is then
only as private as its URL; `*` cannot be combined with specific ids).

### Deploy to Google Cloud Run

The production target is Cloud Run: one container, scale-to-zero, secrets from
Secret Manager, sessions in a GCS bucket. In outline:

1. **Create a bucket** for the encrypted per-user sessions and a dedicated
   service account with `roles/storage.objectAdmin` on just that bucket.
2. **Store two secrets** in Secret Manager — the Telegram `api_hash` and a
   freshly generated 32-byte `MCP_AUTH_TOKEN_KEYS`
   (`head -c 32 /dev/urandom | base64`) — and grant the service account
   `roles/secretmanager.secretAccessor` on them. The token key never leaves
   Secret Manager in plaintext. Losing it logs everyone out (sessions become
   undecryptable). Leaking it alone no longer exposes split-key sessions — those
   also need a live client token — but treat it as highly sensitive regardless:
   it can mint tokens and decrypt current sessions when combined with their
   per-authorization token key.
3. **Deploy** with `gcloud run deploy --source .`, wiring the non-secret env
   from [`deploy/cloudrun.yaml.example`](deploy/cloudrun.yaml.example), the two
   secrets via `--set-secrets`, and **`--max-instances=1`** (mandatory: two
   instances loading the same MTProto session trip Telegram's
   `AUTH_KEY_DUPLICATED` and forcibly log every user out).
4. **Set `MCP_AUTH_ISSUER_URL`** to the service URL and redeploy — tokens and
   session encryption are bound to the issuer value.

Do not set `MCP_HTTP_ADDR=:8080` for Cloud Run. The platform injects `PORT`, and
the application binds to `:$PORT` when `K_SERVICE` is present. An explicitly set
`MCP_HTTP_ADDR` remains an override for non-Cloud-Run environments.

Copy-paste commands, the exact IAM bindings, the token/revocation model, and how
to connect claude.ai / Claude Desktop / Claude Code are in
**[deploy/README.md](deploy/README.md)**.

## Destructive Actions

`DeleteMessages`, `DeleteFolder`, `LeaveChat`, and `ForwardMessage` require an
explicit `confirm: true`. Missing confirmation always fails closed, regardless
of the client's elicitation capabilities.

## When Telegram Isn't Authorized

Telegram sessions expire and can be revoked from **Settings → Devices → Active sessions** on any of your devices. When that happens at startup — or when `api-id`/`api-hash` are missing, or the summarisation settings are invalid — the server does **not** fail its MCP connection. Over stdio it comes up in **login-required mode**:

- the host shows the server as connected, exposing a single tool named **`TelegramLoginRequired`** and no Telegram tools at all;
- the server instructions tell the model Telegram is unavailable and what the fix is, so the first Telegram request you make gets answered with the real reason instead of a generic failure;
- calling `TelegramLoginRequired` re-checks the live state and reports `not_configured`, `login_required`, `check_failed`, or `authorized_pending_reconnect` — the last one meaning you logged in elsewhere and only need to reconnect the server.

The fix is the usual one. Use the path your host actually launches — the binary is typically wired in by absolute path and not on `$PATH`, which is why the tool's `fix_command` field hands back a paste-ready command with the real path already filled in:

```bash
/path/to/mcp-telegram login --phone +1234567890
# then reconnect the MCP server (in Claude Code: /mcp → select this server → Reconnect)
```

This is deliberate. A stdio server that refuses its MCP connection is rendered by hosts as a bare "failed" entry with the reason buried in a log file, indistinguishable from a bad binary path — so the diagnosis is delivered as server content instead, which is the only channel a stdio server has. Two paths keep fail-fast behaviour, because they have no MCP peer to tell:

- the **HTTP transport** — the process exits non-zero so Cloud Run sees an unhealthy start rather than a listener it can never serve;
- **stdin attached to a TTY**, i.e. you ran `mcp-telegram run` yourself — the message goes to stderr and the process exits, which is what makes that command usable as a manual smoke test.

Note the boundary is the TTY, not "a human started it": with stdin piped or redirected from a file, `mcp-telegram run` serves login-required mode and exits **0** on EOF. Scripted health checks should call `mcp-telegram login`/`config list`, or assert on the tool list, rather than on `run`'s exit status.

If Telegram revokes the session while the server is running, the first call it refuses stops the client, and from then on every tool call answers with the same login-required reason and fix instead of reaching Telegram; the host stays connected, and after `mcp-telegram login` a reconnect loads the tools again.

In **remote (HTTP) mode** none of this applies: a dead per-user session — whether Telegram refuses it at connect time or in reply to any call — is deleted and the next request is answered with `401` plus a `WWW-Authenticate` challenge, which sends the MCP client back through OAuth and its QR login to mint a fresh session — no restart, no CLI. A client that stops for any other reason, such as a dropped connection, is reconnected on the same session at the next request.

## Session, Config, and Backup Storage

Local stdio storage is intentionally versioned and does not read the previous
formats:

- **macOS:** Telegram session data and every stored config key are independent
  generic-password items under the `mcp-telegram.v2` Keychain service. Reads go
  directly to Keychain; there is no process cache or aggregate JSON blob.
- **Linux/Windows:** the state root is
  `$XDG_STATE_HOME/mcp-telegram`, or
  `$HOME/.local/state/mcp-telegram` when `XDG_STATE_HOME` is unset. The Telegram
  session is `session-v2.bin`; config values are separate files under
  `config-v2/`. The directory is forced to `0700`, files are `0600`, and writes
  use fsync plus atomic replacement so concurrent updates to different keys do
  not overwrite one another.
- **Backups:** the default stdio backup root is `<state-root>/backups`. An
  operator may replace it with `--allowed-paths` or
  `MCP_TELEGRAM_ALLOWED_PATHS`. Client-provided MCP roots never expand the
  filesystem allowlist. Targets are checked after symlink resolution and files
  are atomically replaced. HTTP and `research` do not initialize or expose this
  facility.

The Linux/Windows local session and config files are **plaintext** despite their
restrictive permissions. A copied `session-v2.bin` can grant access to the
Telegram account: keep the OS account trusted, use an encrypted filesystem
(LUKS/BitLocker), and do not sync the state directory to an unencrypted cloud
backup.

HTTP mode does not use the local stdio session. It requires exactly one backend
(`MCP_AUTH_SESSION_BUCKET` or `MCP_AUTH_SESSION_DIR`) and stores encrypted,
per-authorization objects under `sessions-v3/`, durable revocation tombstones
under `revoked-v3/`, and OAuth state under `oauth-v3/`. Session decryption
requires both the server token key and the per-authorization key carried inside
the sealed client token.

## Breaking Upgrade

This release deliberately provides no migration or compatibility layer:

- previous env names and the removed `--auth`/unauthenticated HTTP modes are not
  accepted;
- the old aggregate Keychain blob, config JSON, local session path, HTTP tokens,
  HTTP storage prefixes, and old opaque cursors are not read;
- every HTTP authorization and affected local installation must be configured
  with the current `MCP_*` names and complete a new Telegram login/QR login.

Before deployment, record the previous image digest and retain the previous
storage long enough to roll back. Delete old HTTP objects only after the new
release passes the real-Telegram and Cloud Run smoke described in
[deploy/README.md](deploy/README.md). A rollback must restore the previous
binary, environment, and storage prefixes together.

## Development

```bash
make build            # build the binary with version metadata
make lint             # golangci-lint
make test             # unit tests
make test-integration # end-to-end tests (need a real account + TEST_* vars)

# Production acceptance checks used by CI
go test -race ./...
./scripts/check-coverage.sh
go vet ./...
govulncheck ./...
go mod verify
```

Unit tests run without credentials. The integration suite in `test/` is behind
the `integration` build tag and drives the server over a real stdio pipe using a
second MCP implementation (mark3labs/mcp-go) as the client, to catch wire-level
interop issues. It reads the `TEST_*` variables documented in `.env.example`
(e.g. `TEST_CHAT_ID`, `TEST_GROUP_ID`) and skips any test whose variable or
Telegram credentials are unset.

CI runs ordinary tests on Linux and macOS, the race detector, package coverage
gates, vet, golangci-lint, govulncheck, a Windows cross-build, and a pinned
Docker build followed by a HIGH/CRITICAL vulnerability scan. The release image
uses a checksummed Go 1.26.8 toolchain and Alpine 3.22 with security updates.

## License

[MIT](LICENSE)
