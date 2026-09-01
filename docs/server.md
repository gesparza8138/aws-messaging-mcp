# The server: one Lambda, every tool

How the single binary works and what each of the 14 MCP tools does.
Companion documents: [architecture.md](architecture.md) (the high-level
map), [cicd.md](cicd.md) (how code gets here), and
[infrastructure.md](infrastructure.md) (what it runs on). The
[tool reference](tools/README.md) carries the generated, always-current
schemas; this page explains behaviour.

## One binary, three entry paths

The whole server is a single static Go binary (`cmd/server`, built as
`bootstrap` for `provided.al2023`/arm64 — no layers, no web adapter,
~200 ms cold start). It starts in one of three ways:

1. **Lambda Function URL** (production path): CloudFront forwards the HTTP
   request to the Function URL; `internal/lambdaadapter` converts the
   buffered Function URL event into a standard `http.Request` and back.
2. **Scheduler task**: the daily cleanup schedule invokes the same function
   with `{"task":"files-cleanup"}`. `lambdaadapter.Mux` probes every payload
   for a `task` key and dispatches to a registered job instead of the HTTP
   stack — Function URL events never carry that key, so the probe is
   unambiguous.
3. **Local development** (`make dev`): the same handler serves plain HTTP on
   `:8000` with ambient AWS credentials.

Configuration is entirely environment variables (`internal/settings`),
set by CloudFormation; the origin secret and files signing key are resolved
from SSM at cold start. **Tool families register only when configured**:
SES tools always; SMS tools once `ORIGINATION_IDENTITY` is set (the eum
stack exists); files tools once `FILES_BUCKET` and `FILES_KEY_PAIR_ID` are
both set. A stage without a subsystem simply doesn't advertise its tools.

## The request pipeline

Every `/mcp` call passes, in order:

1. **Edge allow-list** (CloudFront Function): default-deny IPv4 CIDR list;
   `/files/*`, `/`, and `/opt-in` are exempt.
2. **Origin secret**: CloudFront injects `X-Origin-Secret`; the Lambda
   compares in constant time, so direct Function-URL calls die with 403.
3. **Bearer auth** (`internal/auth`): break-glass SHA-256 static token
   first (if enabled — scopes limited by `BREAK_GLASS_SCOPES`), then Cognito
   JWT: RS256 against a cached JWKS, issuer, expiry, `token_use=access`, and
   a client-id allow-list. Failures return the RFC 9728 401 challenge.
4. **Scope check**: each tool demands its scope (`msg/read`,
   `msg/email:send`, `msg/sms:send`, `msg/files:write`) from the principal;
   a missing scope is a *tool error*, never a 401 — the model can read it.
5. **Guardrails** (`internal/guardrails`, 100 % coverage, send tools only):
   every decision is returned in `ServerMetadata.guardrails` so a refusal is
   explainable; the first blocking decision stops the call.
6. **The AWS call** — through narrow interfaces (`internal/awsclients`) with
   API errors mapped to `Code: Message` tool errors.

**`DryRun` contract** (all three send tools): guardrails run, nothing is
sent, and the response carries `WouldCall` — the *exact* SDK input the real
call would use, server-injected fields included. The EUM API's own `DryRun`
field is server-controlled and never exposed.

## The tools

### Email (SES)

| Tool | Scope | Behaviour |
| --- | --- | --- |
| `ses_send_email` | `msg/email:send` | Mirrors `sesv2 SendEmail` (`Simple` or `Raw`, exactly one). Guardrails: sender allow-list (or the `From` inside raw MIME), recipient allow-list, max recipients, the raw ladder (`raw_base64` → `raw_size` → `raw_mime` → `sender_allow_list`, each stage its own decision), attachment decoding and combined size (`attachment_base64`, `attachment_size`, same `EMAIL_MAX_RAW_BYTES` budget), rate limits. Attachments may be `INLINE` with a `ContentId` for `cid:` images — SES assembles the MIME. Injected: `ConfigurationSetName` (event trail), default `ReplyToAddresses` |
| `ses_list_email_identities` | `msg/read` | Verified sender identities |
| `ses_get_account` | `msg/read` | Sandbox/production flag and quotas |

### SMS / MMS (End User Messaging)

| Tool | Scope | Behaviour |
| --- | --- | --- |
| `sms_send_text_message` | `msg/sms:send` | Mirrors `SendTextMessage`. Guardrails: US-only destination, recipient allow-list, origination pinned to the toll-free number, `MaxPrice` capped at the server ceiling, rate limits. Injected: configuration set, protect configuration, origination |
| `sms_send_media_message` | `msg/sms:send` | Mirrors `SendMediaMessage`; `MediaUrls` must live in the private media bucket, or `MediaUpload` stages an inline image (jpeg/png/gif ≤ 5 MB) there under `mms/` and substitutes the URL |
| `sms_describe_phone_numbers` | `msg/read` | Origination numbers with status/capabilities |
| `sms_get_message_status` | `msg/read` | The API has no per-message read, so this looks the `MessageId` up in the stage's EUM event trail (CloudWatch Logs) and returns the newest event type |

### Files (S3 + CloudFront signed URLs)

| Tool | Scope | Behaviour |
| --- | --- | --- |
| `files_put_object` | `msg/files:write` | Inline body ≤ 4 MB (text or base64) → `shared/<random>/<name>` with a per-object `expires-at`, returns a signed URL directly. Guardrails: content-type deny-list (HTML/executables), size, expiry window (≤ 365 d), bucket quota, rate limits |
| `files_create_upload_url` | `msg/files:write` | 15-minute presigned S3 `PUT` bound to content type and exact length (≤ 500 MB) for big files; sign afterwards |
| `files_create_signed_url` | `msg/files:write` | Signs (or re-signs) an object — canned policy, or custom with `IpAddress`. Re-signing only ever *extends* the stored expiry, so the cleanup job can't orphan a live link |
| `files_list_objects` | `msg/read` | Shared objects with sizes and expiries |
| `files_delete_object` | `msg/files:write` | Delete-as-revoke: the URL 403s on the next request (the `/files/*` behavior is deliberately uncached) |

Object keys are `shared/…` tool-side and `files/shared/…` in the bucket,
because CloudFront forwards the full `/files/…` path to the S3 origin.

### Diagnostics

`hello` (`msg/read`) echoes stage, caller, and auth method — the end-to-end
auth-chain probe every client connects with first.

## Shared machinery

- **Rate limiter**: sliding hour/day windows per tool in one DynamoDB
  TTL table; a counter-store error blocks the send (fails closed).
- **Event trails**: SES and EUM sends land in per-stage CloudWatch log
  groups via configuration sets injected into every send.
- **URL signing** (`internal/signing`): canned/custom CloudFront policies,
  RSA-SHA1 because the CDN contract demands it; the private key never
  leaves SSM except into the Lambda's memory.
- **Background jobs**: `files-cleanup` (daily) deletes shared objects whose
  `expires-at` passed.
