# Sharing files

How the `files_*` tools turn a file into a link you can hand to someone —
and how to take that link back.

## The short version

Ask Claude to share a file: it calls `files_put_object` (inline, ≤ 4 MB) or
the `files_create_upload_url` → `PUT` → `files_create_signed_url` path for
anything bigger (≤ 500 MB). Either way you get back an
`https://mcp.gabriel-esparza.com/files/…` URL that works from **any**
network — `/files/*` is exempt from the edge IP allow-list because the
CloudFront signature in the URL is the access control.

## What protects the link

- **Signature**: the distribution only serves `/files/*` requests signed by
  the server's key (CloudFront trusted key group). Tampered or expired URLs
  get `403`.
- **Expiry**: every link carries one (`ExpiresIn` like `P3D`, or an absolute
  `DateLessThan`; 365-day maximum). Re-signing an object can only *extend*
  its stored expiry — a shorter re-sign never shortens an existing link.
- **Optional IP pinning**: `files_create_signed_url` with `IpAddress`
  restricts the link to a CIDR (custom policy).
- **Content rules**: HTML and executables are refused (they would run in the
  recipient's browser or machine); inline bodies cap at 4 MB, uploads at
  500 MB, and the bucket has a total quota backstop.

## Taking a link back

| Situation | Do this |
| --- | --- |
| One link should die now | `files_delete_object` with the object's key — the URL `403`s immediately |
| Every link should die | `go run ./cmd/ops rotate-signing-key --stage prod` + redeploy: the key-group swap invalidates all outstanding URLs |
| Nothing to do | Objects self-delete daily once their expiry passes (the `files-cleanup` schedule) |

## Attaching a shared object to an email

A shared object can go out as an email attachment without being uploaded
again: pass its key as `RawContentKey` in a `ses_send_email` attachment and
the server reads the bytes from the bucket itself — they never pass through
the model. Give it `ContentDisposition: INLINE` and a `ContentId` and it
renders in the body instead of arriving as an attachment: the server assembles
that message itself as a `multipart/related`, because SES's own `Simple`
assembly roots everything under `multipart/mixed` where a `cid:` reference
never resolves ([plan](plans/email-inline-mime.md)). There has to be an `Html`
body, and it should carry `<img src="cid:the-content-id">`: an inline part the
HTML never references still arrives as an ordinary attachment (the
`inline_cid_refs` decision says so), and a `cid:` the message does not declare
is refused.
The caller needs `msg/read` as well as `msg/email:send`, the key
must be under `shared/`, and the object must fit the 10 MB email budget.
An object whose expiry has passed is refused even though the daily cleanup
has not deleted it yet — the link is dead, so the attachment is too.

A whole hand-built MIME message can be uploaded and referenced the same way,
as `Content.Raw.DataKey` instead of `Content.Raw.Data` (exactly one of the
two). Same read path, same scope, same expiry and size checks — it exists
because inlining a message costs base64 twice over, so a 75 KB image reaches
~156,000 characters in one string parameter.

## What is and is not behind CloudFront

Only the **files bucket** sits behind CloudFront, locked to the distribution
by Origin Access Control and gated by the key group. The MMS media bucket
and everything else the server touches are private — deliberately, so it is
always obvious what is internet-reachable (PRD §5.2).
