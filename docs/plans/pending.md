# Pending items

Living checklist of everything in flight or waiting on someone. Update it
whenever an item lands (check it off with the date) or a new wait appears.
Last updated: 2026-08-24 (post-v0.4.1).

## Owner actions

- [ ] **Toll-free verification form** (M3-G3). Console → AWS End User
  Messaging SMS → Phone numbers → the toll-free number → Registrations →
  *Create registration* → Toll-free. Every field's suggested text is in
  [aws-prerequisites §2.2](../setup/aws-prerequisites.md); the opt-in page to
  reference/screenshot is <https://mcp.gabriel-esparza.com/opt-in> (live,
  renders the number). Carrier review takes 1–2 weeks.
- [ ] **`gh secret set E2E_TEST_PHONE --env dev`** (M3-G4) — owner mobile in
  E.164. Turns on the real SMS/MMS e2e stages; the deploy workflow merges it
  into the dev recipient allow-list at deploy time (never committed).
- [ ] **Verify the owner handset against the EUM sandbox** (optional until
  production access): `create-verified-destination-number` →
  `send-destination-number-verification-code` → `verify-destination-number`
  with the texted code (commands in [testing.md](../testing.md) /
  [aws-prerequisites §2.3](../setup/aws-prerequisites.md)).

## Waiting on AWS

| Wait | Tracking | Poll | When it lands |
| --- | --- | --- | --- |
| SES production access | Support case `178750837400500` (follow-up answered 2026-08-23) | `aws sesv2 get-account --query ProductionAccessEnabled` | Sandbox lifts for both stages (per account-region, M2-1); no code change — prod's recipient allow-list is already off, dev keeps its guardrail. Update PRD R-table |
| Toll-free carrier verification | Number `+18885777930`, id `phone-f6131eb3de7f4e6b96794acbdf4c6bec` | `aws pinpoint-sms-voice-v2 describe-phone-numbers` (Status → `ACTIVE`), `describe-registrations` | Texts stop being carrier-filtered; then request EUM production access (next row) |
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


## Standing notes

- The `msg/rcs:send` Cognito scope stays defined but dormant (RCS descoped
  2026-08-23) so existing client consents keep refreshing.
- Deploys: dev via `gh workflow run deploy-dev.yml -f execute=true`; prod
  only via a `v*` tag + the GitHub `prod` environment approval.
