<div align="center">

# mcp-telegram

**MCP server for Telegram — let AI assistants interact with your Telegram account**

[![MCP Server](https://badge.mcpx.dev?type=server 'MCP Server')](https://github.com/punkpeye/awesome-mcp-servers)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tolmachov/mcp-telegram)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Report Card](https://goreportcard.com/badge/github.com/tolmachov/mcp-telegram)](https://goreportcard.com/report/github.com/tolmachov/mcp-telegram)
[![mcp-telegram MCP server](https://glama.ai/mcp/servers/tolmachov/mcp-telegram/badges/card.svg)](https://glama.ai/mcp/servers/tolmachov/mcp-telegram)
[![mcp-telegram MCP server](https://glama.ai/mcp/servers/tolmachov/mcp-telegram/badges/score.svg)](https://glama.ai/mcp/servers/tolmachov/mcp-telegram)

</div>

---

## Features

- **Chat Management**: List, search, mute/unmute chats
- **Messages**: Read, search, inspect context, send, draft, schedule, link-resolve, and backup messages
- **AI Summarization**: Summarize chat conversations using multiple LLM providers
- **Secure**: Session stored in macOS Keychain (file-based storage on Linux/Windows)

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
mcp-telegram config set TELEGRAM_API_ID 123456789
mcp-telegram config set TELEGRAM_API_HASH abcd1234efgh5678
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

19 tools exposed to MCP clients. Messages are identified by opaque string
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
| `SendMessage` | Send, reply, schedule, or draft a message. `mode` = `send` (default) / `schedule` / `draft`; `reply_to_message_id` works with any mode; `schedule_at` is RFC3339 |
| `EditMessage` | Edit a message; for scheduled handles, `schedule_at` reschedules delivery in the same call |
| `DeleteMessage` | Delete a message; `"s:<id>"` handles cancel pending scheduled messages |
| `ForwardMessage` | Forward a delivered message (scheduled handles are rejected) |
| `ResolveMessageLink` | Parse `t.me` / `tg://` message links into `chat_id`, `message_id`, and `topic_message_id` for forum links |
| `MarkAsRead` | Mark one or more chats as read |
| `BackupMessages` | Export messages to a text file (idempotent; overwrites target) |
| `ResolveUsername` | Resolve @username to user/chat info |
| `SetChatMute` | Mute or unmute chat notifications (`muted` bool + optional `duration_seconds`) |
| `SummarizeChat` | AI-powered chat summarization via sampling / Gemini / Ollama / Anthropic |
| `GetMedia` | Download photo media from a message resource URI; returns MCP image content |

## Available Resources

| URI | Description |
|-----|-------------|
| `telegram://me` | Current user info |
| `telegram://chats` | All chats list |
| `telegram://chat/{id}/info` | Detailed info for any chat ID via resource template |
| `telegram://chats/{id}` | Last 100 messages from a pinned chat (dynamic resource, only for currently pinned chats) |

Pinned chat resources are created dynamically for each pinned chat and refreshed in the background; clients will receive `resources/list_changed` when the set changes.

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
mcp-telegram config set TELEGRAM_API_ID 123456789
mcp-telegram config set TELEGRAM_API_HASH abcd1234

# List stored keys
mcp-telegram config list

# Delete a stored value
mcp-telegram config delete TELEGRAM_API_ID
```

Allowed keys: `TELEGRAM_API_ID`, `TELEGRAM_API_HASH`, `ANTHROPIC_API_KEY`, `GEMINI_API_KEY`.

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

## Destructive Actions

Tools like `DeleteMessage` request user confirmation via [MCP elicitation](https://modelcontextprotocol.io/docs/concepts/elicitation) before proceeding. If your MCP client does not support elicitation, the server relies on the LLM's instructions to confirm verbally before executing destructive operations.

## Session Storage

- **macOS**: Stored securely in Keychain.
- **Linux/Windows**: Stored in `~/.local/state/mcp-telegram/session.json` with `0600` file permissions. The file is **plaintext** — keep the containing user account trusted, and prefer running on macOS when handling sensitive accounts.

Config values set via `mcp-telegram config set` (API keys, Telegram credentials) follow the same backend: Keychain on macOS, plaintext JSON on Linux/Windows.

## License

[MIT](LICENSE)
