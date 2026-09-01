# Email composition plan — inline images, integrity, attach-by-reference

Status: **approved (2026-08-31) — PR1 in flight**. Post-M5 work on
`ses_send_email` only ([PRD](../PRD.md) §5.3, §8): no new tools, and no
infrastructure before PR3. It answers a gap
report from a client that tried to send an email with an embedded image
(§1) and closes it in three PRs (§3). Exit criteria: an HTML email with a
`cid:` image sends without the client hand-building MIME; every send tool
can prove what it received; attachments can name a file the server already
holds instead of shipping its bytes through the model.

## 1. The gap report

Verbatim from the client that hit this, kept as the requirements source:

> Every byte of email content must pass inline through the model's tool-call arguments. There is no way to reference content that already exists somewhere else — a local file, an S3 object, or a previously uploaded asset. For a text-only email that's fine; for anything with an embedded or attached image it forces the model to ferry tens of thousands of base64 characters through its own output, which is slow, token-expensive, and risky (a single mistyped character corrupts the message, and a subtle corruption that's still valid base64 sends silently).
>
> Specific gaps: (1) ses_send_email accepts no content-by-reference; Content.Raw.Data and Attachments[].RawContent are inline-base64-only; the file store and email sender don't compose. (2) Inline CID images require the Raw MIME shape, and Raw is all-or-nothing — the HTML body gets wrapped in base64 and the PNG gets base64'd twice. (3) No server-side MIME assembly — the client must know MIME internals. (4) DryRun can't validate what matters: whether the decoded MIME parses, whether attachment bytes are intact, no checksum echo, no post-send verification. (5) Guardrail errors are terse and conflate the guardrail name with a different failure (raw_size: invalid base64). (6) No size guidance in the schema.
>
> One-sentence version: the email tool and the file store live on the same server but can't reference each other, so any binary content has to round-trip through the model's context as inline base64.

## 2. Findings

| # | Finding | Consequence |
| --- | --- | --- |
| EC-1 | The SDK's `sestypes.Attachment` already carries `ContentDisposition` (`ATTACHMENT`/`INLINE`), `ContentId`, `ContentTransferEncoding`, and `ContentDescription`; SES assembles the MIME for `Simple` + `Attachments`. Our schema exposed only `FileName`/`ContentType`/`RawContent` | Gap-report items (2) and (3) are a schema omission, not a missing feature: exposing the four fields gives `cid:` images with no raw MIME and no double base64. PR1 |
| EC-2 | `buildSendEmail` decoded both base64 payloads with `raw, _ :=` and dropped the error | A corrupt attachment silently became empty bytes instead of a refusal. The guardrails now decode once and hand the bytes to the builder, which no longer decodes at all. PR1 |
| EC-3 | Attachments had no size guardrail at all — only `Content.Raw` was capped | Any number of attachments could be pushed at SES until it refused. They now share the `EmailMaxRawBytes` budget as combined decoded bytes. PR1 |
| EC-4 | `RawEmail` reported bad base64 as `raw_size` and MIME-header failures as `sender_allow_list` | Gap-report item (5): the decision name lied about what failed. Split into `raw_base64` → `raw_size` → `raw_mime` → `sender_allow_list`, progressive so `ServerMetadata` shows the whole ladder. PR1 |
| EC-5 | Neither the schema nor the tool description mentioned any size limit; the server's is 10 MB decoded and SES's own ceiling is 40 MB | Gap-report item (6): the model had to guess. Both numbers are now in the tool description and the `RawContent` / `Raw.Data` field descriptions. PR1 |
| EC-6 | `DryRun` echoes `WouldCall` but nothing the client can check its bytes against | Gap-report item (4): a digest of what the server decoded is the cheap half of "prove it arrived intact". PR2 |
| EC-7 | The files bucket and the email tool never compose (gap-report item (1)) | An attachment that names an object the server already holds skips the model's context entirely — but it crosses a PRD rule (server-injected content the caller did not send inline), so it needs the rule amended, not worked around. PR3 |

## 3. The three PRs

| PR | Title | Contents | Deploys |
| --- | --- | --- | --- |
| 1 | `feat: native inline (CID) attachments and attachment guardrails` | The four SDK attachment fields in `internal/schemas`; `guardrails.EmailAttachments` (`attachment_base64`, `attachment_size`); split `RawEmail` decisions; guardrail-decoded bytes passed to `buildSendEmail` (no silent decode); size guidance in the schema, tool description, PRD §5.3/§8, and `docs/server.md` | — |
| 2 | `feat: content digests in ServerMetadata` | SHA-256 digest and decoded byte count per attachment and for `Content.Raw`, returned in `ServerMetadata` on both `DryRun` and real sends, so a client can verify what the server actually decoded (EC-6) | — |
| 3 | `feat: attach by reference from the files bucket` | `Attachments[].RawContentKey` (a files-bucket key the server reads and attaches server-side), its guardrails (key ownership, size against the same budget, content-type deny-list), the Lambda role's read path, and the PRD rule-7 amendment plus the Appendix C decision-log rows for this whole plan | dev, then a release to prod |

## 4. Out of scope

- **`Content.Raw.DataKey`** — attach-by-reference for a whole raw MIME
  message. `RawContentKey` covers the real use case; a raw message pulled
  from a bucket would bypass the sender-allow-list parse we do on the
  bytes we were handed.
- **The media bucket.** MMS media stays on its own bucket with its own
  24-hour expiry (PRD §5.3); email attachments read from the files bucket
  only.
- **Server-side MIME assembly.** SES already assembles `Simple` +
  `Attachments`; writing our own multipart builder would add a MIME
  implementation to maintain for no capability we lack.
- **Image validation beyond digests** — no decoding, re-encoding,
  dimension checks, or format sniffing of attachment bytes.
- **Response-size cap redesign.** Digests are bounded; returning content
  back to the model is not, and the cap that would need is its own piece
  of work.
