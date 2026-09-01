# Inline-MIME plan — make `cid:` images actually render

Status: **in progress**. PR A (this plan, the docs correction, and the
non-blocking `inline_not_rendered` warning) and PR B (the `internal/mimebuild`
assembler plus the inline guardrails, both unwired) are shipped; PRs C–E are
not started. Post-M5 work on `ses_send_email` only ([PRD](../PRD.md) §5.3, §8):
no new tools, no infrastructure, no new environment variable. It reopens
what [email-composition.md](email-composition.md) claimed to have closed
(EC-13) after a client proved the claim false in production. Exit criteria:
an HTML email declared through `Content.Simple` with an `INLINE` attachment
renders the image in the body in Gmail, Apple Mail, and Outlook; the
assembled part tree is observable from the client; nothing that sends today
starts being refused.

## 1. The report

Verbatim from the client that hit this, kept as the requirements source:

```text
# `ses_send_email`: inline CID images don't render, and the documented workaround is unreachable

**Summary.** There is currently no working path to send an HTML email with an image embedded inline in the body. The `Simple` content shape accepts inline-image parameters and reports success, but the delivered message does not render the image in the body. The `Raw` content shape, which the tool description offers as the alternative, is structurally unreachable for any realistic image size when the caller is an LLM agent.

**What we tried.** Three sends, all accepted, all returning a MessageId, all with content_digests confirming byte-intact arrival: (1) Simple + Attachments[0] with ContentDisposition INLINE, ContentId "weekend-chart", ContentType image/png, RawContentKey shared/.../weekend_chart_20260901.png, HTML containing <img src="cid:weekend-chart"> - image not rendered; (2) same with ContentType image/jpeg - not rendered; (3) same with ContentId "<weekend-chart>" (RFC 2045 angle-bracket form) - not rendered.

**Diagnosis.** For a cid: reference in an HTML part to resolve, the HTML part and the referenced image part have to be siblings inside a multipart/related container. If they are siblings in a multipart/mixed container instead, mainstream clients treat the image as an ordinary attachment and leave the cid: reference unresolved - exactly the observed behaviour. Nothing the caller can pass through the current parameter surface influences that container choice. We could not confirm this directly, because the assembled message isn't observable from the client side.

**R1** - Inline images declared through Simple must actually render inline in Gmail (web, iOS, Android), Apple Mail, and Outlook. Also define and document the expected ContentId form (with or without angle brackets) and normalise whichever the caller supplies. Multiple inline images must work, and a text/plain alternative must remain possible alongside the HTML.

**R2** - If Raw is the guaranteed path, it needs an object reference instead of inlined bytes. Content.Raw.Data takes the complete MIME message as a base64 string inlined in the tool call, generated token-by-token inside a bounded per-response output budget. For the chart in question: 74,601 bytes of PNG becomes ~100,800 chars base64 as a MIME body part, plus ~15,500 chars of HTML, plus ~1,000 chars of headers/boundaries = ~117,300 chars of MIME message, base64-encoded again for Content.Raw.Data = ~156,400 chars in a single string parameter. That is ~1.78x the original binary before the HTML is counted. Shrinking the image to ~18 KB to fit renders the chart's axis labels illegible.

**R3** - Make the assembled message observable before it's sent: the MIME part tree and the headers on each part, not bodies. When an inline image fails to render, the caller cannot currently distinguish wrong parameters from server-side assembly from a receiving-client quirk. The only available diagnostic was to send real email to a real person and ask what they saw.

**R4** - Correct the tool description if Simple can't do this today. For an LLM caller the tool description IS the entire specification - there is no README to cross-check, no source to read, no colleague to ask. A description that promises a capability the tool doesn't deliver actively directs the caller away from whatever does work, and overrides more accurate instructions the caller may have been given. In this case the operator's own runbook said the Simple shape cannot guarantee inline CID and to use Content.Raw.Data; the tool description contradicted it and won. It was wrong.

**Working well - do not change:** RawContentKey on attachments (large binaries reach the message without transiting the model's context); files_create_upload_url (presigned PUT made a 75 KB upload possible at all); ServerMetadata.content_digests (SHA-256 + byte count let us prove payload integrity and rule it out as a cause within one call).
```

## 2. Root cause

The client's diagnosis is correct, and it is confirmed by AWS twice over.

`SendEmail` with `Content.Simple` + `Attachments` assembles the message with
a **`multipart/mixed`** root: the HTML body part and every attachment part
become siblings of that container. A `cid:` reference resolves only when the
HTML part and the referenced part are siblings inside a
**`multipart/related`** (RFC 2387), which is the container that tells a
client "these parts are one compound document". Given `multipart/mixed`,
mainstream clients do exactly what the spec asks of them: they treat each
attachment as a separate, downloadable body part and leave the `cid:` URI
unresolved. Nothing on the `Attachments[]` surface selects the container.

- The [`Attachment` API reference](https://docs.aws.amazon.com/ses/latest/APIReference-V2/API_Attachment.html)
  documents `ContentId` as "Unique identifier for the attachment, used for
  referencing attachments with INLINE disposition in HTML content" (length
  1..78). It describes what the field is *for*; it never promises a
  `multipart/related`, and the promise is the part that matters.
- The re:Post thread [Email attachments in SES V2 : incorrect inline
  documentation and MIME implementation](https://repost.aws/questions/QUfip3VbVIT1-1tEenZyBzSg/email-attachments-in-ses-v2-incorrect-inline-documentation-and-mime-implementation)
  (September 2025) reports the identical symptom and reaches the identical
  conclusion; AWS's recommended workaround is to send raw MIME.

The honest part: **we never verified our claim against a real delivery.**
EC-1 read the SDK struct, saw `ContentDisposition`/`ContentId`, and inferred
the container. Every test we wrote — unit, full-chain, and the e2e
`TestAttachByReference` — asserts on the *request* we would send. The e2e
test only ever did a `DryRun`, so no assembled message ever left the account
and no mailbox was ever looked at. A capability claim about what a *client*
renders cannot be validated by inspecting an API request, and this plan's §8
is written around that.

## 3. Requirements

| # | Requirement | Notes |
| --- | --- | --- |
| R1 | Inline images declared through `Simple` must render in the body in Gmail (web, iOS, Android), Apple Mail, and Outlook. The expected `ContentId` form is defined and documented, and whichever form the caller supplies is normalised. Multiple inline images work, and a `text/plain` alternative remains possible alongside the HTML | The headline. §4 |
| R2 | If `Raw` is the guaranteed path it needs an object reference instead of inlined bytes: 74,601 bytes of PNG → ~100,800 chars of base64 body part + ~15,500 chars of HTML + ~1,000 chars of headers and boundaries = ~117,300 chars of MIME, base64'd again for `Content.Raw.Data` = **~156,400 chars in one string parameter**, ~1.78× the original binary before the HTML is counted. Shrinking the chart to ~18 KB to fit makes its axis labels illegible | R1 makes `Raw` the rare path rather than the only one, which downgrades this from blocking to worthwhile. `Content.Raw.DataKey` is PR E |
| R3 | The assembled message must be observable before it is sent: the part tree and each part's headers, not bodies | `ServerMetadata.mime_structure` on `DryRun` and real sends. PR D, and see the §6 hazard |
| R4 | The tool description must be correct about what `Simple` can do | Shipped in PR A. For an LLM caller the description *is* the specification: it overrode the operator's own runbook, which had been right |

## 4. Design

**Assemble the MIME ourselves only when we must.** When any `Simple`
attachment has `ContentDisposition: INLINE` or a non-empty `ContentId`, the
server builds the complete message and sends it as `Content.Raw`. Otherwise
nothing changes: the call goes out as `Content.Simple` and SES assembles it,
exactly as today. That keeps the blast radius at the messages that are
broken today and guarantees no regression for the ones that are not.

Target tree, **alternative-outer**:

```text
multipart/mixed                              (only when non-inline attachments exist)
├── multipart/alternative                    (only when both Text and Html are set)
│   ├── text/plain
│   └── multipart/related; type="text/html"
│       ├── text/html
│       └── image/…  ×N                      (Content-ID: <cid>, Content-Disposition: inline)
└── application/…  ×M                        (Content-Disposition: attachment; filename=…)
```

Both outer layers collapse when they hold one child: HTML-only with one
inline image and no ordinary attachments is a bare `multipart/related`, and
a message with neither inline parts nor attachments never enters this path
at all.

**Why alternative-outer and not related-outer.** RFC 2387's `type=`
parameter must name the media type of the **root part** of the related
group. With `multipart/alternative` inside `multipart/related`, the root of
the related group is the `alternative`, so the correct header is
`multipart/related; type="multipart/alternative"` — and getting that
parameter wrong is itself a documented cause of the exact symptom we are
fixing. Alternative-outer keeps the related group's root a plain
`text/html`, so `type="text/html"` is trivially correct, and it is also what
the mainstream senders emit, which is the shape client heuristics are tuned
for.

Non-obvious requirements, each of which is a way to get this wrong:

- **Header injection is a new attack surface.** `mime/multipart.Writer` does
  not sanitise header values — a `\r\n` in a `FileName` or a subject splits
  the header block. Every value we place in a header is encoded, not
  interpolated: `Subject` and `ContentDescription` through `mime.QEncoding`,
  addresses through a `net/mail.ParseAddress` round-trip (parse, then re-emit
  from the parsed struct, never the caller's string), filenames through
  `mime.FormatMediaType`, and `ContentId` restricted to
  `[A-Za-z0-9._@+-]`. Today none of these reach a header we write, so none
  of it is currently a concern; the moment we assemble, all of it is.
- **`base64.NewEncoder` does not wrap lines.** A 75 KB image becomes one
  100,000-character line, and RFC 5321 caps a line at 1000 octets including
  CRLF. We need a wrapping writer at 76 characters; forgetting it produces
  a message that is accepted locally and mangled or rejected in transit.
- **We lose SES's own validation.** SES currently checks `FileName`,
  `ContentType`, `ContentId`, and `ContentTransferEncoding` for us and
  refuses the call. Once we build the message, SES sees only opaque bytes,
  so those lengths and shapes become **our** checks (`ContentId` 1..78 per
  the API reference, and a `FileName` that survives `FormatMediaType`).
- **`Bcc` must never become a header.** Bcc recipients ride on
  `call.Destination.BccAddresses`, which SES honours for a `Raw` message
  without disclosing them; writing a `Bcc:` header would leak every hidden
  recipient to every recipient. `To` and `Cc` *do* become headers, because a
  raw message with no `To` displays as undisclosed-recipients.
- **`Reply-To` becomes our header**, so `call.ReplyToAddresses` must be
  cleared on this path — set in both places, SES adds a second `Reply-To`
  and clients disagree about which wins.
- **Emit a `Date` header** (SES adds one for `Simple`, and a raw message
  without one looks like spam), from an injected clock so tests are
  deterministic.

**`ContentId` normalisation.** Accept either `weekend-chart` or
`<weekend-chart>`; match the HTML's `cid:` target against the **bare** form;
always emit the header as `Content-ID: <weekend-chart>`. Document the rule
in the schema so the caller does not have to guess, which is the second half
of R1. (The client's third attempt used the angle-bracket form on the
assumption that it might be the difference. It was not, but it should never
have been a question.)

## 5. Where the code goes

New package **`internal/mimebuild`** (standard library only —
`mime`, `mime/multipart`, `net/mail`, `encoding/base64`, `bytes`): the
assembler that turns a `schemas.Message` plus decoded attachment bytes into
a complete MIME message, and the reader-side walker PR D uses to describe
the tree it just built.

It is deliberately **not** in `internal/guardrails`, which carries a
100 %-per-function coverage gate. A multipart builder is mostly `if err !=
nil` guards over a `bytes.Buffer` that cannot fail; reaching 100 % would
mean building fault-injection harnesses — a failing `io.Writer`, a
`multipart.Writer` forced to error — to cover branches that exist only
because the signatures return an error. That is real work for zero
defect-detection value, and it dilutes what the 100 % gate means where it
does matter.

The **pure string validation** goes in `internal/guardrails/email.go`, where
100 % is both achievable and worth having: `ContentId` charset and length,
`FileName` acceptability, address round-tripping, and header-value
rejection. Those are total functions over strings with genuinely
interesting branches, and they are the security-relevant half.

## 6. Hazard: `mime_structure` must be flat

R3's part tree is naturally a recursive type (`type Part struct { …; Parts
[]Part }`). **It must not be.** `jsonschema.For` returns an error on any
named recursive type, `sendEmailOutputSchema()` turns that error into a
`panic`, and the panic happens inside `NewServer` — which would take down
every tool registration, `cmd/gendocs`, and the Lambda cold start, for a
diagnostic field. `go test` in `internal/mcpserver` would catch it, but the
failure mode is loud out of all proportion to the feature.

So `mime_structure` is a **flat list**: one entry per part, each carrying its
own index and its parent's, with the tree reconstructed by the reader if it
wants one. This is EC-8's twin one layer deeper — the same lesson (the
output schema is inferred from Go types at construction time, and inference
has opinions) arriving through a different door.

## 7. PRs

| PR | Title | Contents | Status |
| --- | --- | --- | --- |
| A | `fix: correct the inline-CID claim and warn callers` | This plan; the tool description, `internal/schemas` doc comments, PRD §5.3, `docs/server.md`, `docs/files.md`, the e2e comment, and `email-composition.md` (EC-13, EC-14, §4) corrected; a non-blocking `inline_not_rendered` guardrail decision on any `INLINE`/`ContentId` attachment; PRD Appendix C row | **shipped** |
| B | `feat: internal/mimebuild multipart assembler` | The assembler and the tree walker, plus `internal/guardrails/email.go` string validation (`attachment_fields`, `inline_content_id`, `inline_needs_html`, `inline_cid_refs`). Unwired: nothing calls it, `ses_send_email` behaves exactly as it does today. Tests include the structural parse of §8 | **shipped** |
| C | `feat: assemble multipart/related for inline attachments` | Wire the assembler into `sendEmail` behind the "any `INLINE` or `ContentId`" condition; clear `ReplyToAddresses` on that path; the assembled-size check (§9); remove the `inline_not_rendered` warning; correct every doc PR A corrected, again; e2e sends a real inline image and the owner confirms rendering | planned |
| D | `feat: ServerMetadata.mime_structure` | The flat part list (§6) on `DryRun` and real sends, for both the assembled path and a caller-supplied `Content.Raw` (R3) | planned |
| E | `feat: Content.Raw.DataKey` | Attach-by-reference for a whole raw MIME message (R2, EC-14), on the same read path as `RawContentKey` | planned |

## 8. Verification

**The acceptance criterion is a rendered image in a real client** — Gmail
web, Gmail iOS, Gmail Android, Apple Mail, and Outlook — not a green test.
Every layer below that is a proxy, and the whole reason this defect shipped
is that we accepted a proxy (an SDK struct field) for the thing itself.

| Layer | What it proves |
| --- | --- |
| **Structural parse** (unit, PR B) | Re-read the assembled bytes with `mime/multipart.Reader` and assert the tree: root type, `type=` parameter, part order, `Content-ID` on the inline parts, `Content-Disposition` on the attachments, no line over 998 octets. **Nothing in the repo does this today** — every existing test asserts on the request we build, never on bytes parsed back — and it is the gate that would have caught EC-13 if the message had been ours to inspect |
| **Injection tests** (unit, PR B) | `\r\n`, `\n`, and long-header attempts in `FileName`, `Subject`, `ContentDescription`, `ContentId`, and every address field are refused, not encoded into the message |
| **Full-chain** (PR C) | The MCP client round trip, as EC-8 taught: a handler-level test bypasses the SDK's validating wrapper and would miss an output-schema fault |
| **e2e** (PR C) | A real send to the owner's mailbox with a real PNG, and the owner opens it in each client. Recorded in this document with the date, like the M5 drills |

## 9. Budgets

- **`EmailMaxRawBytes` (10 MB, default) keeps metering decoded attachment
  bytes**, exactly as it does today. Nothing that sends now is newly
  refused — that is the point.
- **A separate assembled-size check** against SES's own 40 MB ceiling, as a
  constant, applied to the assembled message.
- **Do not reuse the 10 MB number after assembly.** Base64 inflates by 4/3
  before boundaries and headers, so 10 MB of attachments assembles to
  ~13.7 MB; checking the assembled bytes against 10 MB would start refusing
  sends that work today, which is the one outcome this plan may not produce.
- **No new environment variable and no infrastructure change**: both numbers
  already exist (`EMAIL_MAX_RAW_BYTES` in settings, 40 MB as SES's
  documented ceiling).

## 10. Findings

Continuing the EC-N sequence of
[email-composition.md](email-composition.md) §2, which this plan reopened.

| # | Finding | Consequence |
| --- | --- | --- |
| EC-15 | **`mime.QEncoding.Encode` splits a long value into several encoded words but joins them with a plain space, not a fold.** A 800-character subject is therefore one 800-character header line, and RFC 5322 caps a line at 998 octets. Folding the value at its own spaces is not a fix either: unfolding restores the space, so folding *unencoded* text inserts a space at every fold point, and a long value with no spaces has nowhere to fold | `encodeHeaderText` keeps `QEncoding.Encode` for the common case (it is the CRLF defence and it leaves plain ASCII readable) and re-encodes an over-long value as a run of encoded words joined by CRLF + space. Whitespace between adjacent encoded words is dropped when they are decoded, so the original string is rebuilt exactly. PR B |
| EC-16 | **`multipart.Reader.NextPart` transparently decodes a quoted-printable body and hides the `Content-Transfer-Encoding` header.** A walker built on it describes a message no recipient receives, and reports byte counts that disagree with the ones the assembler reports for the very same bytes | `Walk` uses `NextRawPart`. This is what makes the Assemble → Walk round-trip test an assertion about the message rather than about the reader. PR B |
| EC-17 | **`mime.ParseMediaType` and `mime.FormatMediaType` are the content-type validation we would otherwise have written.** A type carrying a CRLF is not `token/token`, so parsing refuses it before it can split the header block, and `FormatMediaType` returns `""` rather than an unusable header. Parsing the caller's `ContentType` instead of interpolating it also keeps a type that carries parameters (`text/calendar; method=REQUEST`) intact, which interpolation would have broken | `attachmentNode` parses, then re-emits with the `name` parameter added. §4's "filenames through `FormatMediaType`" turned out to cover content types too. PR B |
| EC-18 | **`net/mail.ParseAddress` already refuses CR and LF anywhere in an address**, including inside a quoted display name, and `Address.String` quotes or encodes what it re-emits | The address defence is the parse itself; a separate CRLF pre-check would be unreachable code. §5 assigned "address round-tripping" to `internal/guardrails`, but it belongs where the header is written: the sender allow-list already parses `From`, the recipient guardrails already run, and a second decision would only duplicate them. The guardrail half of PR B is the four decisions that have no other home (`attachment_fields`, `inline_content_id`, `inline_needs_html`, `inline_cid_refs`). PR B |
| EC-19 | **"Which attachments are inline" had to be decided, not assumed.** The reporting client set `ContentDisposition: INLINE` *and* `ContentId`, but nothing forces a caller to set both, and a part with a `ContentId` the HTML references has to be inside the `multipart/related` or the reported bug simply reappears for that caller | Both packages apply one rule: `INLINE` is inline, a `ContentId` with no disposition at all is inline, and an explicit `ATTACHMENT` wins (so a file can still carry a `Content-ID` without joining the related group). `guardrails.InlineAttachments` and `mimebuild.isInline` are written to the same rule so the checks cover exactly the parts the assembler will place inline. PR B |
| EC-20 | **`crypto/rand.Text` (Go 1.24) gives boundaries with no error to handle**: 26 base32 characters, all legal in a boundary, well inside `multipart.Writer`'s 70-character limit | The default boundary generator is one line and cannot fail, which keeps the injectable `Message.Boundary` seam purely a determinism device rather than an error path. PR B |
