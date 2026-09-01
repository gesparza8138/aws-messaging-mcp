# Email composition plan — inline images, integrity, attach-by-reference

Status: **approved (2026-08-31) — all three PRs implemented**. Post-M5 work on
`ses_send_email` only ([PRD](../PRD.md) §5.3, §8): no new tools, and — as it
turned out — no infrastructure change at all, PR3 included (EC-9). It answers a gap
report from a client that tried to send an email with an embedded image
(§1) and closes it in three PRs (§3). Exit criteria: ~~an HTML email with a
`cid:` image sends without the client hand-building MIME~~ (**not met — see
EC-13**; the fields ship but SES does not render them inline, and the work
moves to [email-inline-mime.md](email-inline-mime.md)); every send tool
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
| EC-1 | ~~The SDK's `sestypes.Attachment` already carries `ContentDisposition` (`ATTACHMENT`/`INLINE`), `ContentId`, `ContentTransferEncoding`, and `ContentDescription`; SES assembles the MIME for `Simple` + `Attachments`. Our schema exposed only `FileName`/`ContentType`/`RawContent`~~ **Superseded by EC-13**: the fields exist and are passed through, but SES assembles them under `multipart/mixed`, so exposing them did *not* give `cid:` images | ~~Gap-report items (2) and (3) are a schema omission, not a missing feature: exposing the four fields gives `cid:` images with no raw MIME and no double base64.~~ The four fields still ship (they are the correct schema mirror), but gap-report items (2) and (3) are **not** closed: inline rendering needs the server to build the MIME. See [email-inline-mime.md](email-inline-mime.md). PR1 |
| EC-2 | `buildSendEmail` decoded both base64 payloads with `raw, _ :=` and dropped the error | A corrupt attachment silently became empty bytes instead of a refusal. The guardrails now decode once and hand the bytes to the builder, which no longer decodes at all. PR1 |
| EC-3 | Attachments had no size guardrail at all — only `Content.Raw` was capped | Any number of attachments could be pushed at SES until it refused. They now share the `EmailMaxRawBytes` budget as combined decoded bytes. PR1 |
| EC-4 | `RawEmail` reported bad base64 as `raw_size` and MIME-header failures as `sender_allow_list` | Gap-report item (5): the decision name lied about what failed. Split into `raw_base64` → `raw_size` → `raw_mime` → `sender_allow_list`, progressive so `ServerMetadata` shows the whole ladder. PR1 |
| EC-5 | Neither the schema nor the tool description mentioned any size limit; the server's is 10 MB decoded and SES's own ceiling is 40 MB | Gap-report item (6): the model had to guess. Both numbers are now in the tool description and the `RawContent` / `Raw.Data` field descriptions. PR1 |
| EC-6 | `DryRun` echoes `WouldCall` but nothing the client can check its bytes against | Gap-report item (4): a digest of what the server decoded is the cheap half of "prove it arrived intact". PR2 |
| EC-7 | The files bucket and the email tool never compose (gap-report item (1)) | An attachment that names an object the server already holds skips the model's context entirely — but it crosses a PRD rule (server-injected content the caller did not send inline), so it needs the rule amended, not worked around. PR3 |
| EC-8 | Found while writing PR2's full-chain test: the go-sdk infers `[]byte` as a JSON *array*, but `encoding/json` marshals it as a base64 *string*, and the SDK validates a tool's output against that inferred schema server-side. Every `ses_send_email` `DryRun` carrying binary content — any `Content.Raw`, any attachment — therefore failed validation and came back as a JSON-RPC error, not a result. Dating to M2 for `Raw`; the unit tests never saw it because they call the handler directly and bypass the SDK's validating wrapper | `DryRun` was unusable for exactly the payloads PR2 exists to verify. Fixed in PR2 by giving `ses_send_email` an explicit output schema (`jsonschema.For` with `[]byte` → `["null","string"]`); no other tool echoes `[]byte`. Regression tests go through the real MCP client. PR2 |
| EC-9 | The Lambda role already grants `s3:GetObject` on `${FilesBucket.Arn}/files/shared/*` (`infra/app.yaml`, Sid `FilesObjects`) — the files tools need it for `HeadObject`, and the same statement covers the read. `awsclients.FilesStore` was the only thing missing a `GetObject` | PR3 ships with no infrastructure change: the plan's "the Lambda role's read path" was already there. The real `*s3.Client` satisfies the widened interface structurally; only the two hand-written fakes needed the method. PR3 |
| EC-10 | The files bucket takes objects up to `FilesMaxUploadBytes` (500 MB), 50× the 10 MB email attachment budget | A referenced object is `HeadObject`-ed and refused on `ContentLength` *before* `GetObject`, so an oversize reference costs one metadata call instead of pulling 500 MB into a 6 MB-response Lambda to be refused by the size guardrail afterwards. PR3 |
| EC-11 | `CleanupFiles` runs daily, so an object stays readable for up to ~24 h after its `expires-at` passes | Attaching one would outlive the expiry the owner chose, so the read path refuses it with its own message ("awaiting cleanup") — distinct from not-found, because the two mean different things to the caller. PR3 |
| EC-12 | The content-type deny-list planned for PR3 is already enforced on every write into the bucket (`files_put_object` and the presigned `files_create_upload_url`, which binds the `Content-Type`) | Re-checking it on read would refuse nothing that could be there, so it was dropped. Email attachments are not served to a browser by CloudFront in any case — the deny-list exists for the link path. PR3 |
| EC-13 | **EC-1's premise was wrong.** SES `SendEmail` with `Simple` content assembles attachments under a root `multipart/mixed`; a `cid:` reference resolves only when the HTML part and the image part are siblings inside a `multipart/related`. Reported after the fact by a client: three sends (`image/png`, `image/jpeg`, and the `<angle-bracket>` `ContentId` spelling), all accepted, all delivered, none rendered, with `content_digests` proving the bytes arrived intact. AWS's own re:Post thread ([Sept 2025](https://repost.aws/questions/QUfip3VbVIT1-1tEenZyBzSg/email-attachments-in-ses-v2-incorrect-inline-documentation-and-mime-implementation)) confirms it and recommends raw MIME. The claim was never verified against a real delivery — the e2e test only ever did a `DryRun` | The plan's headline capability ("an HTML email with a `cid:` image sends without the client hand-building MIME") was never delivered, and the tool description actively directed callers away from the shape that does work. Corrected in `docs/plans/email-inline-mime.md` PR A (docs, tool description, and a non-blocking `inline_not_rendered` decision); server-side `multipart/related` assembly follows |
| EC-14 | §4's rationale for excluding `Content.Raw.DataKey` — that a bucket-sourced raw message "would bypass the sender-allow-list parse" — was wrong: `guardrails.RawEmail` parses *decoded* bytes, and bytes read from the bucket decode to the same thing | The exclusion needed a real reason (scope, not safety). Corrected in §4 below; `DataKey` is now a candidate PR in the inline-MIME plan, where a server-assembled message makes hand-built raw messages rarer but not obsolete |

## 3. The three PRs

| PR | Title | Contents | Deploys | Status |
| --- | --- | --- | --- | --- |
| 1 | `feat: native inline (CID) attachments and attachment guardrails` | The four SDK attachment fields in `internal/schemas`; `guardrails.EmailAttachments` (`attachment_base64`, `attachment_size`); split `RawEmail` decisions; guardrail-decoded bytes passed to `buildSendEmail` (no silent decode); size guidance in the schema, tool description, PRD §5.3/§8, and `docs/server.md` | — | implemented |
| 2 | `feat: content digests in ServerMetadata` | SHA-256 digest and decoded byte count per attachment and for `Content.Raw`, returned in `ServerMetadata` on both `DryRun` and real sends, so a client can verify what the server actually decoded (EC-6); plus the `ses_send_email` output-schema fix that made a binary `DryRun` reachable at all (EC-8) | — | implemented |
| 3 | `feat: attach by reference from the files bucket` | `Attachments[].RawContentKey` (a files-bucket key the server reads and attaches server-side); `GetObject` on the `FilesStore` interface; the checks around the read (exactly one of `RawContent`/`RawContentKey`, `msg/read` on top of `msg/email:send`, `shared/` prefix, expiry, size before download), after which the bytes rejoin the inline path (same `attachment_size` budget, same digest); the PRD rule-7 amendment and the Appendix C row for this whole plan. No IAM or template change (EC-9); no read-time content-type check (EC-12) | dev, then a release to prod | implemented |

## 4. Out of scope

- **`Content.Raw.DataKey`** — attach-by-reference for a whole raw MIME
  message. `RawContentKey` covered the use case in front of us, and that is
  the whole reason: it is **not** a safety boundary. `guardrails.RawEmail`
  parses the *decoded* bytes, so a message read from the bucket runs the
  identical `raw_base64` → `raw_size` → `raw_mime` → `sender_allow_list`
  ladder (EC-14). It is now a candidate PR in
  [email-inline-mime.md](email-inline-mime.md) §7.
- **The media bucket.** MMS media stays on its own bucket with its own
  24-hour expiry (PRD §5.3); email attachments read from the files bucket
  only.
- ~~**Server-side MIME assembly.**~~ **Now in scope** (EC-13). This bullet
  rested on EC-1's premise — "SES already assembles `Simple` +
  `Attachments`, so a multipart builder buys no capability we lack" — and
  the premise was wrong: SES's assembly is `multipart/mixed`, which cannot
  render a `cid:` image. The capability we lack is exactly the one the
  builder provides. Planned in
  [email-inline-mime.md](email-inline-mime.md).
- **Image validation beyond digests** — no decoding, re-encoding,
  dimension checks, or format sniffing of attachment bytes.
- **Response-size cap redesign.** Digests are bounded; returning content
  back to the model is not, and the cap that would need is its own piece
  of work.
