FROM golang:1.26-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Stamp the version the same way the Makefile does so the container binary
# self-reports a real version instead of "dev".
ARG VERSION=docker
RUN go build -ldflags="-s -w -X github.com/tolmachov/mcp-telegram/internal.Version=${VERSION}" -o mcp-telegram .

FROM alpine:3.20

# ca-certificates is required for the HTTPS summarize providers (Anthropic,
# Gemini); without it their TLS handshakes fail inside the container.
RUN apk add --no-cache ca-certificates \
	&& adduser -D -h /home/app app

USER app
WORKDIR /home/app

COPY --from=builder /app/mcp-telegram /usr/local/bin/mcp-telegram

# The Telegram session and file-backed config live under the home directory.
# Mount a volume at /home/app to persist login across container restarts.
# In the HTTP mode (MCP_TRANSPORT=http + AUTH_* env) no volume is needed:
# per-user sessions live in the configured GCS bucket.
ENTRYPOINT ["mcp-telegram"]
CMD ["run"]
