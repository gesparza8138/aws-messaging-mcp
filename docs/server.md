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

**`ServerMetadata.content_digests`** (`ses_send_email`): a SHA-256 and a
decoded byte count for every binary part the server received — `part: "raw"`
for a caller-supplied `Content.Raw` message, `part: "attachment[<i>]:<FileName>"`
for each `Simple` attachment (indexed because two attachments may share a
name), and `part: "assembled"` for the whole message when the server built one
for an inline send, alongside the per-attachment digests rather than instead
of them.
Text bodies are not digested; they arrive as plain JSON the caller can
already read back. The digests are computed after the guardrails pass and
before the `DryRun` fork, so a dry run and the real send of the same payload
report identical values: hash locally, compare against the dry run to prove
the bytes survived transit, then compare again on the real send. Absent when
nothing binary was sent.

> [!NOTE]
> Prefer the digests to reading `WouldCall` back. A dry run echoes the whole
> decoded payload re-encoded as base64 JSON, so a near-budget message can
> exceed the Lambda Function URL's ~6 MB buffered response limit. That is
> pre-existing behaviour for `Content.Raw`, not new, and it is exactly why
> integrity is proved with a 64-character digest instead. An inline send
> echoes more still: the assembled message re-encodes every attachment as
> base64 inside the MIME, and then the whole of it as base64 again for
> `Content.Raw.Data`.

**`ServerMetadata.mime_structure`** (`ses_send_email`): the MIME part tree of
the message that will actually be sent — headers, never bodies — on a
`DryRun` and on the real send alike, so an image that does not render can be
diagnosed from the call instead of by sending real mail to a real person and
asking what they saw. One entry per part:

```jsonc
[ { "path": "1",     "depth": 0, "content_type": "multipart/alternative", "bytes": 0 },
  { "path": "1.1",   "depth": 1, "content_type": "text/plain", "bytes": 4 },
  { "path": "1.2",   "depth": 1, "content_type": "multipart/related", "bytes": 0 },
  { "path": "1.2.1", "depth": 2, "content_type": "text/html", "bytes": 36 },
  { "path": "1.2.2", "depth": 2, "content_type": "image/png", "disposition": "inline",
    "content_id": "logo", "filename": "logo.png", "bytes": 16 } ]
```

`path` is dotted (`1` is the whole message, `1.2.1` the first child of its
second child), so sibling-hood — the thing a `cid:` reference depends on — is
readable straight off it: above, the image and the HTML part are both
children of the `multipart/related`. `bytes` is the part's body *as encoded
in the message*; a container reports 0 and its children carry the bytes.
The list is **flat** by necessity, not by taste: the tool's output schema is
inferred from Go types at registration and `jsonschema.For` refuses a named
recursive type, so a nested `Parts` field would panic every tool registration
([plan](plans/email-inline-mime.md) §6). Rebuild the tree from the paths.

It covers both messages the server owns: the one it assembled for an inline
send (reported from the assembler, so it cannot disagree with the bytes) and
a caller-supplied `Content.Raw` — inline `Data` or a `DataKey` — which is
parsed back under conservative bounds (depth 10, 200 parts) because those
bytes are untrusted. A message that will not parse omits the field rather
than failing the send: the raw ladder has already checked everything that
decides deliverability, and a diagnostic is not worth refusing a send over.
A `Simple` send omits it too — SES assembles that one, so there is no tree
for the server to describe.

## The tools

### Email (SES)

| Tool | Scope | Behaviour |
| --- | --- | --- |
| `ses_send_email` | `msg/email:send` | Mirrors `sesv2 SendEmail` (`Simple` or `Raw`, exactly one). Guardrails: sender allow-list (or the `From` inside raw MIME), recipient allow-list, max recipients, the raw ladder (`raw_base64` → `raw_size` → `raw_mime` → `sender_allow_list`, each stage its own decision), attachment decoding and combined size (`attachment_base64`, `attachment_size`, same `EMAIL_MAX_RAW_BYTES` budget), rate limits. `ContentDisposition: INLINE` with a `ContentId` (or a `ContentId` alone) renders as a `cid:` image: the server assembles that message itself — HTML part and inline parts as siblings inside a `multipart/related`, ordinary attachments outside it — and sends it as `Content.Raw`, because SES's own `Simple` assembly roots everything under `multipart/mixed` where a `cid:` never resolves ([plan](plans/email-inline-mime.md)). Write the `ContentId` with or without angle brackets; the HTML references the bare form. Those sends run four more guardrails (`attachment_fields`, `inline_content_id`, `inline_needs_html`, `inline_cid_refs` — a `cid:` the message does not declare is refused) plus `assembled_size` against SES's 40 MB ceiling. **Breaking change:** a `DryRun` of an inline send echoes `WouldCall.Content.Raw` — anything reading `WouldCall.Content.Simple.Attachments` for one now finds nothing there. Sends with no inline attachment are unchanged. An attachment may carry `RawContentKey` (a `shared/…` files-bucket key) instead of `RawContent`, and a whole raw message may carry `Content.Raw.DataKey` instead of `Content.Raw.Data` (exactly one of each pair) — the server reads those bytes itself; both paths also require `msg/read` (they are files-store reads), refuse keys outside `shared/` and objects past their expiry, and check the object's size before downloading it. A `DataKey` message runs the ladder from `raw_size` on: `raw_base64` is absent because nothing was base64-encoded, and `sender_allow_list` is decided *after* the read because the `From` header is inside the object — the recipient guardrails and the rate limiter still refuse before any S3 read. `ServerMetadata.content_digests` hashes each binary part it received, and `ServerMetadata.mime_structure` describes the part tree it will send. Injected: `ConfigurationSetName` (event trail), default `ReplyToAddresses` |
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
