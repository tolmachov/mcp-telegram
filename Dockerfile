FROM golang:1.27.3-alpine3.24@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Stamp the version the same way Taskfile.yml does so the container binary
# self-reports a real version instead of "dev".
ARG VERSION=docker
RUN go build -ldflags="-s -w -X github.com/tolmachov/mcp-telegram/internal.Version=${VERSION}" -o mcp-telegram .

FROM alpine:3.24.4@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

# ca-certificates is required for the HTTPS summarize providers (Anthropic,
# Gemini); without it their TLS handshakes fail inside the container.
RUN apk upgrade --no-cache \
	&& apk add --no-cache ca-certificates \
	&& adduser -D -h /home/app app

USER app
WORKDIR /home/app

COPY --from=builder /app/mcp-telegram /usr/local/bin/mcp-telegram

# The Telegram session and file-backed config live under the home directory.
# Mount a volume at /home/app to persist login across container restarts.
# In HTTP mode (MCP_TRANSPORT=http + MCP_AUTH_* env) no volume is needed:
# per-user sessions and OAuth state live in the configured Firestore database.
ENTRYPOINT ["mcp-telegram"]
CMD ["run"]
