# Deploying the remote (HTTP) mode to Cloud Run

The HTTP mode serves MCP over streamable HTTP behind an embedded OAuth 2.1
authorization server. Users authenticate by scanning a Telegram QR code on
the authorization page; each allowed user gets their own MTProto session
(stored encrypted in GCS) and their own MCP assembly.

## One-time setup

```bash
PROJECT=my-project
REGION=europe-west1
BUCKET=my-mcp-telegram-sessions
SA=mcp-telegram@${PROJECT}.iam.gserviceaccount.com

# Bucket for encrypted per-user sessions.
gcloud storage buckets create gs://${BUCKET} --location=${REGION} \
  --uniform-bucket-level-access

# Dedicated service account with access to that bucket only.
gcloud iam service-accounts create mcp-telegram
gcloud storage buckets add-iam-policy-binding gs://${BUCKET} \
  --member=serviceAccount:${SA} --role=roles/storage.objectAdmin

# Secrets: the Telegram API hash and the 32-byte token master key.
printf '%s' "$TELEGRAM_API_HASH" | gcloud secrets create telegram-api-hash --data-file=-
head -c 32 /dev/urandom | base64 | tr -d '\n' | gcloud secrets create mcp-auth-token-key --data-file=-
for s in telegram-api-hash mcp-auth-token-key; do
  gcloud secrets add-iam-policy-binding $s \
    --member=serviceAccount:${SA} --role=roles/secretmanager.secretAccessor
done
```

## Deploy

```bash
gcloud run deploy mcp-telegram \
  --source . \
  --region=${REGION} \
  --service-account=${SA} \
  --max-instances=1 \
  --env-vars-file=deploy/cloudrun.env \
  --set-secrets=TELEGRAM_API_HASH=telegram-api-hash:latest,AUTH_TOKEN_KEY=mcp-auth-token-key:latest \
  --allow-unauthenticated
```

After the first deploy, put the service URL into `AUTH_ISSUER_URL` in
`deploy/cloudrun.env` and deploy again — sealed tokens and session encryption
are bound to the issuer value.

### Why `--max-instances=1` is required

Every user has exactly one MTProto session (one auth key). If two instances
load the same session and connect concurrently from different IPs, Telegram
kills the key with `AUTH_KEY_DUPLICATED` and every user is forcibly logged
out. A single instance caps nothing else: scale-to-zero still works
(`min-instances` stays 0), sessions persist in GCS, and per-user clients are
re-connected lazily on the next request.

If idle connections misbehave after long CPU-throttled pauses (tools failing
right after a wake-up), add `--no-cpu-throttling` — it keeps background
MTProto connections alive between requests at the cost of always-on billing
while an instance exists.

## Token model and revocation

Access and refresh tokens are stateless sealed blobs — the server keeps no
token database, so individual tokens cannot be revoked by id, and a rotated
refresh token stays valid until its absolute 30-day expiry. What the design
gives you instead are two coarse levers, both re-checked on every refresh:

- **Remove a user from `AUTH_ALLOWED_USERS`** and redeploy — their refresh
  grants stop working immediately (the allowlist is re-checked at refresh).
- **Delete their session** from the bucket (`gs://$BUCKET/sessions/<id>.bin`)
  — refresh re-checks that the session still exists, so this forces a fresh
  QR login.

A leaked refresh token therefore grants at most 30 days of access and cannot
be individually revoked; if that matters, rotate `AUTH_TOKEN_KEY` (invalidates
*all* tokens at once) or shorten the deployment's exposure by keeping the
allowlist tight. `POST /revoke` exists for spec compliance but is a no-op.

## Connecting a client

- **claude.ai / Claude Desktop**: add a custom connector with the service
  URL. The OAuth flow opens the authorization page; scan the QR with the
  Telegram app (Settings → Devices → Link Desktop Device), enter the 2FA
  password if prompted.
- **Claude Code**: `claude mcp add --transport http telegram https://<service-url>/`
  and complete the same browser flow with `/mcp`.

Only Telegram accounts listed in `AUTH_ALLOWED_USERS` can complete the login;
everyone else is rejected after the scan and their session is discarded.
