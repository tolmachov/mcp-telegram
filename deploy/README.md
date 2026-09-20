# Cloud Run deployment

HTTP mode is always protected by the embedded OAuth 2.1 server. Each OAuth
authorization creates an independent Telegram MTProto session and a durable
refresh-token family. There is no unauthenticated HTTP mode.

## One-time setup

```bash
PROJECT=my-project
REGION=europe-west1
BUCKET=my-mcp-telegram-sessions
SA=mcp-telegram@${PROJECT}.iam.gserviceaccount.com

gcloud storage buckets create gs://${BUCKET} --location=${REGION} \
  --uniform-bucket-level-access
gcloud iam service-accounts create mcp-telegram
gcloud storage buckets add-iam-policy-binding gs://${BUCKET} \
  --member=serviceAccount:${SA} --role=roles/storage.objectAdmin

printf '%s' "$MCP_TELEGRAM_API_HASH" | \
  gcloud secrets create mcp-telegram-api-hash --data-file=-
head -c 32 /dev/urandom | base64 | tr -d '\n' | \
  gcloud secrets create mcp-telegram-token-key --data-file=-
for s in mcp-telegram-api-hash mcp-telegram-token-key; do
  gcloud secrets add-iam-policy-binding "$s" \
    --member=serviceAccount:${SA} --role=roles/secretmanager.secretAccessor
done
```

Copy `deploy/cloudrun.env.example` to the untracked
`deploy/cloudrun.env`, fill in the bucket, allowlist, API ID, and issuer URL.
Do not add `PORT`: Cloud Run injects it into the ingress container and the
application binds to `:$PORT`. Cloud Run does not expand `$PORT` inside another
environment variable, so `MCP_HTTP_ADDR=$PORT` is not a valid substitute.
`MCP_HTTP_ADDR` remains available as an explicit override outside Cloud Run;
the ordinary local default stays `127.0.0.1:8080`.

## Deploy

```bash
gcloud run deploy mcp-telegram \
  --source . \
  --region=${REGION} \
  --service-account=${SA} \
  --max-instances=1 \
  --env-vars-file=deploy/cloudrun.env \
  --set-secrets=MCP_TELEGRAM_API_HASH=mcp-telegram-api-hash:latest,MCP_AUTH_TOKEN_KEYS=mcp-telegram-token-key:latest \
  --allow-unauthenticated
```

`--allow-unauthenticated` exposes the OAuth protocol and login page; the MCP
endpoint itself rejects requests without a valid bearer token. Keep
`--max-instances=1`: two instances must never connect the same MTProto session
at once because Telegram can revoke the duplicated auth key.

Do not point an uptime check at `/healthz`. Cloud Run's frontend reserves that
exact path and answers it with its own 404 before the request reaches the
container, and the server exposes no other unauthenticated health route:
`/health` and `/healthz/` fall through to the bearer-gated `/` and return 401.
Probe `/.well-known/oauth-protected-resource` instead, which the application
serves and which reflects its configuration.

The service enforces a 1 MiB MCP request body, 32 KiB headers, 128 concurrent
HTTP requests, a ten-minute MCP session timeout, and a per-user limit of ten
new sessionless MCP sessions per minute (burst three). `MCP_AUTH_TRUSTED_PROXY_HOPS=0`
ignores all forwarding headers. Increase it only after verifying every address
in the actual proxy chain.

## Token and storage model

- Authorization codes have a random durable `jti` and can be redeemed once.
- Refresh tokens carry a family id and generation. Rotation is an atomic CAS.
  Reuse or concurrent replay of an old generation revokes the whole family,
  its Telegram session, and the live pooled client.
- A storage failure returns `503`; it does not consume or rotate the grant.
- Session encryption requires both the server master key and the random
  per-authorization key carried by the OAuth token.
- Current objects use `sessions-v3/`, `revoked-v3/`, and `oauth-v3/grants/`.
  Older prefixes are never read.

## Breaking upgrade

This release intentionally has no migration path. Before deployment, record a
rollback image digest and preserve the old storage long enough to roll back.
After committing to the new release:

1. Remove old objects under `sessions/`, `revoked/`, and `oauth-v2/` only after
   the rollback window closes.
2. Every user reconnects the MCP client and completes QR login (and 2FA when
   enabled).
3. Verify code exchange, refresh rotation, revoke, two parallel clients, MCP
   reconnect, and offline logout against a real Telegram account.

Rollback means deploying the recorded previous image with its previous env and
storage prefixes. Do not point an older binary at the v3 prefixes.

## Release gate

Do not promote unless Linux/macOS tests, race tests, vet, lint, `govulncheck`,
module verification, Windows cross-build, container build/scan, and the manual
Telegram smoke all pass. Also verify that the v3 prefixes contain only current
objects and that rollback instructions/image digest are recorded.
