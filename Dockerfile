FROM golang:1.26-alpine3.22@sha256:727cfc3c40be55cd1bc9a4a059406b28a059857e3be752aa9d09531e12c20c56 AS builder

# The Docker Official Image can lag Go security patch releases. Install the
# checksummed upstream toolchain explicitly so the final binary's stdlib is not
# inherited from a stale builder image.
#
# TARGETARCH is set only by BuildKit. Cloud Build's classic docker builder --
# what `gcloud run deploy --source` uses -- leaves it empty, so fall back to
# the architecture the builder image itself reports. Do not make this RUN
# depend on TARGETARCH alone: an empty value fails the build outright.
ARG TARGETARCH
ARG GO_PATCH_VERSION=1.26.8
RUN GOARCH_="${TARGETARCH:-$(go env GOARCH)}" \
	&& case "$GOARCH_" in \
		amd64) GO_SHA256=d0f743b33e8d8945e6b1f432edd15785c70507121d6e2a723b21285eddf8b57b ;; \
		arm64) GO_SHA256=211ffced9dcb9633a55eac6364816ec0ddd951389a740e88fa8b3337971bdda0 ;; \
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

FROM alpine:3.22@sha256:14358309a308569c32bdc37e2e0e9694be33a9d99e68afb0f5ff33cc1f695dce

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
