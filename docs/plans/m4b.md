# M4b plan — file sharing via CloudFront-signed URLs

Status: **draft — awaiting owner approval (gate M4b-G1)**. Scope per
[PRD](../PRD.md) §16 M4b: the `files_*` tools, files bucket behind the
existing distribution, signing key group, cleanup schedule, tests, docs,
prod release. Exit criteria: a signed link downloads from a non-allow-listed
network; expired/tampered links 403; cleanup verified.

## 1. Findings that change the PRD

| # | Finding | Consequence |
| --- | --- | --- |
| M4b-1 | The cleanup schedule invokes the Lambda directly with `{"task":"files-cleanup"}` — not a Function URL event, which is all `internal/lambdaadapter` handles | The handler grows a task branch: payloads carrying `task` dispatch to an internal job runner before the HTTP adapter; unit-tested against both event shapes |
| M4b-2 | The CloudFront public key must exist as template input before the first M4b deploy (chicken-and-egg with the stack) | `cmd/ops rotate-signing-key` generates the RSA-2048 pair *first*: private key → SSM SecureString `/messaging-mcp/<stage>/files-signing-key`, public PEM → SSM `/messaging-mcp/<stage>/files-public-key-pem`; the deploy workflows read the PEM into a parameter like the other SSM-fed values. Rotation = re-run + redeploy (key group swap invalidates all old links, which is the documented emergency lever, R9) |
| M4b-3 | CloudWatch `BucketSizeBytes` (the PRD's quota source) lags ~24 h | The quota guardrail treats it as approximate: refuse `files_put_object` when the last-known size exceeds `FilesBucketQuotaBytes`, and note the lag in the tool error. Exact accounting is not worth a live `ListObjectsV2` scan on every upload |
| M4b-4 | CloudFront signed URLs use RSA-SHA1 signatures (fixed by the CDN contract, not by us) | `gosec` will flag SHA1: annotated `#nosec` with the rationale; the hash is CloudFront's protocol requirement, not an integrity choice we control |
| M4b-5 | Presigned S3 `PUT` uploads (`files_create_upload_url`) bypass CloudFront and need direct S3 reachability plus CORS | Uploads go straight to the bucket endpoint (allowed: presigned URLs carry their own auth); bucket CORS permits `PUT` from any origin since the URL itself is the secret. The edge allow-list is not involved |

## 2. Design

### 2.1 Infra (`infra/app.yaml`, per stage)

- **`FilesBucket`** `aws-messaging-mcp-files-<account>-<stage>`: private, SSE-S3,
  no versioning, CORS for presigned `PUT`, abort-incomplete-multipart 1 day.
  No lifecycle expiry — deletion is the cleanup job's job (M4b-1) because the
  expiry is per-object metadata, not an age.
- **`FilesBucketPolicy`**: `s3:GetObject` for `cloudfront.amazonaws.com` with
  `AWS:SourceArn` = the stage's distribution; deny insecure transport.
- **`FilesOAC`** + second cache behavior **`/files/*`** on the existing
  distribution: S3 origin, `TrustedKeyGroups: [SigningKeyGroup]`,
  `CachingOptimized`, GET/HEAD, **`FilesResponseHeadersPolicy`** (nosniff,
  no-store, restrictive CSP).
- **`SigningPublicKey`** + **`SigningKeyGroup`**: PEM from the
  `FilesPublicKeyPem` parameter (SSM-fed, M4b-2). Key pair id lands in env.
- **`FilesCleanupSchedule`** (`AWS::Scheduler::Schedule`): daily, invokes the
  function with `{"task":"files-cleanup"}`; scheduler role with
  `lambda:InvokeFunction` on the function only.
- Lambda role: `s3:PutObject/GetObject/DeleteObject` on `shared/*`,
  `s3:ListBucket` on the bucket, `cloudwatch:GetMetricData` for the quota
  read, plus the signing-key `ssm:GetParameter`.
- New parameters: `FilesPublicKeyPem`, `FilesMaxExpiryDays` (365),
  `FilesMaxUploadBytes` (500 MB), `FilesBucketQuotaBytes` (5 GB dev / 20 GB
  prod via params files).

### 2.2 Tools

| Tool | Scope | Notes |
| --- | --- | --- |
| `files_put_object` | `msg/files:write` | Inline body ≤ 4 MB decoded; key `shared/<uuid>/<FileName>`; `x-amz-meta-expires-at` set; returns the signed URL directly. `DryRun` returns the would-be `PutObjectInput` + policy summary |
| `files_create_upload_url` | `msg/files:write` | S3 presigned `PUT`, 15 min, bound to `ContentType`+`ContentLength` (≤ `FilesMaxUploadBytes`) |
| `files_create_signed_url` | `msg/files:write` | Canned policy (expiry only) or custom policy (`IpAddress`); re-signing bumps `x-amz-meta-expires-at` to the later expiry (metadata copy-in-place) |
| `files_list_objects` | `msg/read` | `shared/` listing with per-object expiry from metadata (HeadObject fan-in capped) |
| `files_delete_object` | `msg/files:write` | Deletes the object; the URL then 403s (S3 404 → CloudFront 403 via OAC) |

Guardrails: exactly-one-of `ExpiresIn`/`DateLessThan`, expiry ≤
`FilesMaxExpiryDays`, content-type deny-list (`text/html`, executables;
`AllowRiskyContentTypes=true` honored in dev only), body/upload size caps,
bucket quota (M4b-3). All in `internal/guardrails`, 100 % coverage.

### 2.3 Code

`internal/signing` (CloudFront URL signer: canned + custom policy, RSA-SHA1
`#nosec` per M4b-4, key lazily resolved from SSM like the origin secret);
`internal/schemas/files.go` (PutObject mirror subset; contract test against
`s3.PutObjectInput` for the mirrored fields; signing fields are tool-only);
`internal/mcpserver/files.go`; cleanup job in `internal/tasks` (list
`shared/`, delete objects whose `expires-at` passed); `cmd/ops
rotate-signing-key`; adapter task branch (M4b-1). S3 presigner via
`s3.NewPresignClient`.

### 2.4 Tests

Unit + contract as usual; signer golden-tested against `aws cloudfront sign`
output for a fixed key/URL/expiry. E2E: put → fetch signed URL (runner is
exempt under `/files/*` regardless of allow-list — negative proof: fetch
*after* withdrawing the runner IP in the `always()` step is skipped; instead
tamper the signature → 403, expire → 403, delete → 403, presigned `PUT`
round-trip ≤ 5 MB, cleanup task invoked directly via `aws lambda invoke`
with a pre-expired object.

## 3. PR breakdown

| PR | Title | Contents | Deploys |
| --- | --- | --- | --- |
| A | `feat: signing key tooling and adapter task branch` | `cmd/ops rotate-signing-key`, `internal/signing`, adapter task dispatch | — (keys generated per stage, owner runs one command each) |
| B | `feat: files schemas, guardrails, tools` | schemas + contract tests, guardrails, five tools, cleanup job | — |
| C | `infra: files bucket, key group, /files/* behavior, cleanup schedule` | app.yaml + params + workflows PEM wiring | dev |
| D | `test: files e2e` | e2e file suite | dev (e2e runs) |
| E | `docs: files tool pages, prd corrections, runbook` | gendocs output, `docs/runbooks/rotate-secrets.md` update, PRD M4b-1..5 | — |

Then release `v0.4.0` → prod.

## 4. Owner gates

| Gate | When | What you do |
| --- | --- | --- |
| M4b-G1 | now | Approve this plan |
| M4b-G2 | after PR A | Run `go run ./cmd/ops rotate-signing-key --stage dev` and `--stage prod` (I hand you the exact commands; generates keys, writes SSM — no AWS charges) |
| M4b-G3 | release | Approve the `prod` deployment of `v0.4.0` |

No billable resources: the bucket, key group, behavior, and schedule are all
free-tier/no-cost at this usage; storage pennies at the 5 GB cap.

## 5. Risks

- R9 (long-lived link leaks) mitigations all land here: per-object delete,
  optional `IpAddress` policy, key rotation as the kill switch.
- R10 (Free-plan 100 GB/month): `BytesDownloaded` alarm is M5; the quota
  guardrail bounds stored bytes now.
- The distribution update (new behavior + key group) is a CloudFront config
  change — no replacement, but propagation takes minutes; e2e polls.
