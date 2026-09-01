# Pending items

Living checklist of everything in flight or waiting on someone. Update it
whenever an item lands (check it off with the date) or a new wait appears.
Last updated: 2026-08-31 (post-email-composition).

## Owner actions

- [x] **Toll-free verification form** — submitted 2026-08-24 via the
  registrations API (`registration-ef01565f27874d13acf093e2ca98fea8`,
  opt-in screenshot attached, number associated). Carrier review 1–2 weeks.
- [x] **`E2E_TEST_PHONE` secret** — set 2026-08-24; the owner number rides
  into dev's recipient allow-list at deploy time.
- [ ] **Resubmit the toll-free registration** — once the apex redirect is live,
  correct the company fields (URL now resolves) and submit a new version:
  `aws pinpoint-sms-voice-v2 submit-registration-version --registration-id
  registration-ef01565f27874d13acf093e2ca98fea8`. Denied 2026-08-25 for
  "Company Verification Failed".
- [ ] **Verify the owner handset against the EUM sandbox** — blocked until
  the toll-free number is `ACTIVE` (the OTP text needs an origination
  identity; record `vdn-6580cbf9c59340f293761c2809f18efe` is created and the
  resume command is in aws-prerequisites §2.3).

## Waiting on AWS

| Wait | Tracking | Poll | When it lands |
| --- | --- | --- | --- |
| ~~SES production access~~ | **Granted 2026-08-30** (case `178750837400500`; `ProductionAccessEnabled` true) | — | Sandbox lifted for both stages; no code change was needed — prod's recipient allow-list was already off, dev keeps its guardrail. PRD prerequisite updated |
| Toll-free carrier verification | **Denied 2026-08-25** — registration `registration-ef01565f27874d13acf093e2ca98fea8` is `REQUIRES_UPDATES`, reason "Company Verification Failed" (company contact/email/address/URL unverifiable against official records). Number `+18885777930`, id `phone-f6131eb3de7f4e6b96794acbdf4c6bec` | `aws pinpoint-sms-voice-v2 describe-registrations`, `describe-phone-numbers` (Status → `ACTIVE`) | Fix in flight: the apex `gabriel-esparza.com` served nothing, so the company URL could not be checked — the `apex-site` stack now 301s it to the mcp landing page ([../setup/deploy.md](../setup/deploy.md) §3d). Resubmission pending; then texts stop being carrier-filtered and EUM production access can be requested (next row) |
| EUM SMS production access (M3-6) | Request **after** carrier verification approves: Console → End User Messaging SMS → Account → *Request production access* | `aws pinpoint-sms-voice-v2 describe-account-attributes` (`ACCOUNT_TIER`) | Set the $20/month spend limit override (`set-text-message-spend-limit-override`); real sends reach unverified destinations; e2e sandbox skips become dead code to keep |
| ~~Lambda concurrency quota~~ | **Granted 2026-08-24** (account limit now 1000) | — | `ReservedConcurrency=5` shipped in M5 PR D |

## Next milestones

- [x] **M4b — file sharing** (released to prod as v0.4.1 on 2026-08-24;
  PRs #55-#63; live-verified: signed download 200, tampered/deleted 403).
  See [docs/files.md](../files.md).
- [x] **M5 — hardening** (released to prod as **v1.0.0** on 2026-08-24;
  PRs #66-#71). Alarms/budget/dashboard live, both drills executed with two
  rotation defects found and fixed, subsystem guides published, reserved
  concurrency active. **The PRD's build scope is complete.**
- [x] **Email composition** (merged to `main` 2026-08-31; PRs #76, #80 —
  #77/#78/#79 were casualties of stacked bases being deleted on merge).
  Inline CID attachments, attachment guardrails, split raw decisions,
  content digests, and attach-by-reference from the files bucket. Fixed
  along the way: silently dropped attachment base64 errors, unmetered
  attachments, and binary `DryRun` failing the SDK's output validation
  since M2. See [email-composition.md](email-composition.md).
  Awaiting the `v1.1.0` release (dev deploy + e2e first).

## Standing notes

- The `msg/rcs:send` Cognito scope stays defined but dormant (RCS descoped
  2026-08-23) so existing client consents keep refreshing.
- Deploys: dev via `gh workflow run deploy-dev.yml -f execute=true`; prod
  only via a `v*` tag + the GitHub `prod` environment approval.
