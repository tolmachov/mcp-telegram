# mcp-telegram

MCP (Model Context Protocol) server for Telegram integration. Allows AI assistants like Claude to interact with your Telegram account.

## Features

- **Chat Management**: List, search, mute/unmute chats
- **Messages**: Read, send, draft, schedule, and backup messages
- **AI Summarization**: Summarize chat conversations using multiple LLM providers
- **Secure**: Session stored in macOS Keychain (or encrypted file on Linux/Windows)

## Installation

```bash
go install github.com/tolmachov/mcp-telegram@latest
```

Or build from source:

```bash
git clone https://github.com/tolmachov/mcp-telegram.git
cd mcp-telegram
go build -o mcp-telegram .
```

## Setup

### 1. Get Telegram API Credentials

1. Go to [my.telegram.org/apps](https://my.telegram.org/apps)
2. Create an application
3. Copy `api_id` and `api_hash`

### 2. Configure Environment

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

Add to `~/.claude/settings.json`:

```json
{
  "mcpServers": {
    "tg": {
      "type": "stdio",
      "command": "/path/to/mcp-telegram",
      "args": ["run"],
      "env": {
        "TELEGRAM_API_ID": "your_api_id",
        "TELEGRAM_API_HASH": "your_api_hash"
      }
    }
  }
}
```

## Available Tools

| Tool | Description |
|------|-------------|
| `GetMe` | Get current user information |
| `GetChats` | List all chats, groups, and channels |
| `SearchChats` | Fuzzy search for chats by name |
| `GetChatInfo` | Get detailed information about a chat |
| `GetMessages` | Get messages from a chat |
| `SendMessage` | Send a message |
| `DraftMessage` | Save a draft message |
| `ScheduleMessage` | Schedule a message for later |
| `GetScheduledMessages` | List scheduled messages |
| `DeleteScheduledMessage` | Cancel a scheduled message |
| `BackupMessages` | Export messages to a text file |
| `ResolveUsername` | Resolve @username to user/chat info |
| `MuteChat` | Mute chat notifications |
| `UnmuteChat` | Unmute chat notifications |
| `SummarizeChat` | AI-powered chat summarization |

## Available Resources

| URI | Description |
|-----|-------------|
| `telegram://me` | Current user info |
| `telegram://chats` | All chats list |
| `telegram://chat/{id}/info` | Chat details |
| `telegram://chat/{id}/messages` | Chat messages |

## Chat Summarization

The `SummarizeChat` tool supports multiple LLM providers:

- **sampling** (default): Uses the MCP client's LLM - no additional setup required
- **ollama**: Local LLM via [Ollama](https://ollama.ai)
- **gemini**: Google Gemini API
- **anthropic**: Anthropic Claude API

Configure via environment variables:

```bash
SUMMARIZE_PROVIDER=sampling  # or: ollama, gemini, anthropic
SUMMARIZE_MODEL=             # provider-specific model name
```

## Commands

```bash
# Run MCP server (used by MCP clients)
mcp-telegram run

# Login to Telegram
mcp-telegram login --phone +1234567890

# Logout and delete session
mcp-telegram logout
```

## Configuration Options

| Environment Variable | Description | Default |
|---------------------|-------------|---------|
| `TELEGRAM_API_ID` | Telegram API ID | Required |
| `TELEGRAM_API_HASH` | Telegram API Hash | Required |
| `TELEGRAM_ALLOWED_PATHS` | Allowed directories for backups | OS app data dir |
| `SUMMARIZE_PROVIDER` | LLM provider for summarization | `sampling` |
| `SUMMARIZE_MODEL` | Model name | Provider default |
| `SUMMARIZE_BATCH_TOKENS` | Tokens per summarization batch | `8000` |
| `OLLAMA_URL` | Ollama API URL | `http://localhost:11434` |
| `GEMINI_API_KEY` | Google Gemini API key | - |
| `ANTHROPIC_API_KEY` | Anthropic API key | - |

## Session Storage

- **macOS**: Stored securely in Keychain
- **Linux/Windows**: Stored in `~/.local/state/mcp-telegram/session.json`

## License

[MIT](LICENSE)