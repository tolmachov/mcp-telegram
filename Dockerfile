FROM golang:1.27-alpine3.24@sha256:8a5910f31396cd4d89662f56c68b3ae31d374308270a1c3bd96672ee5ed43414 AS builder

# The Docker Official Image can lag Go security patch releases. Install the
# checksummed upstream toolchain explicitly so the final binary's stdlib is not
# inherited from a stale builder image.
#
# TARGETARCH is set only by BuildKit. Cloud Build's classic docker builder --
# what `gcloud run deploy --source` uses -- leaves it empty, so fall back to
# the architecture the builder image itself reports. Do not make this RUN
# depend on TARGETARCH alone: an empty value fails the build outright.
ARG TARGETARCH
ARG GO_PATCH_VERSION=1.27.1
RUN GOARCH_="${TARGETARCH:-$(go env GOARCH)}" \
	&& case "$GOARCH_" in \
		amd64) GO_SHA256=63d339f0da5ab53635a56f2490a7984dfe12dfcff22ad749f63edaf590168445 ;; \
		arm64) GO_SHA256=3450b45a3f9ee8568792736a5c5e70a1f2e9b36c35a8f74958c03e51d7d92bec ;; \
		*) echo "unsupported architecture: $GOARCH_" >&2; exit 1 ;; \
	esac \
	&& wget -q "https://go.dev/dl/go${GO_PATCH_VERSION}.linux-${GOARCH_}.tar.gz" -O /tmp/go.tar.gz \
	&& echo "${GO_SHA256}  /tmp/go.tar.gz" | sha256sum -c - \
	&& rm -rf /usr/local/go \
	&& tar -C /usr/local -xzf /tmp/go.tar.gz \
	&& rm /tmp/go.tar.gz \
	&& go version

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Stamp the version the same way the Makefile does so the container binary
# self-reports a real version instead of "dev".
ARG VERSION=docker
RUN go build -ldflags="-s -w -X github.com/tolmachov/mcp-telegram/internal.Version=${VERSION}" -o mcp-telegram .

FROM alpine:3.24@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6

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
# per-user sessions live in the configured GCS bucket.
ENTRYPOINT ["mcp-telegram"]
CMD ["run"]
