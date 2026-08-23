# AWS prerequisites and registrations

One-time, mostly manual steps that gate the milestones in the [PRD](../PRD.md#16-milestones). Start the slow ones (SES production access, toll-free verification, RCS) **now** — they run in parallel with M0/M1 and take days to weeks.

Account: ID kept out of this public repo (`aws sts get-caller-identity` locally; GitHub secret `AWS_ACCOUNT_ID`), region `us-west-2`. Domain: `gabriel-esparza.com` (GoDaddy), subdomain `mcp.gabriel-esparza.com` delegated to Route 53 per [`dns.md`](dns.md).

## Identifiers to capture

Fill this in as you go; these become `infra/params/{dev,prod}.json` values.

| Parameter | dev | prod | Source |
| --- | --- | --- | --- |
| `SesSenderAddresses` | `mcp-dev@gabriel-esparza.com` | `mcp@gabriel-esparza.com` | §1 |
| `SesDomainIdentity` | `gabriel-esparza.com` | same | §1 |
| `SesMailFromDomain` | `bounce.dev.mcp.gabriel-esparza.com` | `bounce.mcp.gabriel-esparza.com` | §1 |
| `SesReplyTo` | `esparza.gabriel@gmail.com` | same | §1 |
| `SmsOriginationIdentities` | toll-free number ARN | same number (or a second one) | §2 |
| `SmsPoolId` *(optional)* | — | — | §2 |
| `RecipientAllowList` (dev only) | `esparza.gabriel@gmail.com`, owner's mobile (GitHub secret `E2E_TEST_PHONE`) | — | §5 |
| `CognitoUserEmail` | `esparza.gabriel@gmail.com` | same | §4 |
| `SigningPublicKeyPem` | from `scripts/rotate-signing-key.py` | separate key | §6 |

## 1. Amazon SES

### 1.1 Domain identity (once, shared by both stages)

Fully automated — the whole domain's DNS lives in Route 53 since M1, so there is nothing to do by hand. `infra/ses-domain.yaml` (deployed once from the workstation, like `bootstrap`) creates the `AWS::SES::EmailIdentity` for `gabriel-esparza.com` with Easy DKIM (RSA 2048) and writes every record into the root zone itself: the three DKIM CNAMEs, root SPF (`v=spf1 -all` — nothing sends as the bare domain), DMARC (`p=quarantine`, strict DKIM alignment, reports to the owner's mailbox), and the custom MAIL FROM (`bounce.mcp.gabriel-esparza.com`) with its MX and SPF records. SES shows **Verified** within minutes of the deploy.

### 1.2 Event trail (per stage, automated)

`infra/app.yaml` gives each stage a configuration set (injected server-side into every send) whose EventBridge destination lands sends, deliveries, bounces, and complaints in `/aws-messaging-mcp/<stage>/ses-events`.

### 1.3 Sender addresses

No mailboxes exist on `gabriel-esparza.com`, so senders are purely outbound identities. The stack's `SesSenderAddresses` allow-list is the only thing that distinguishes stages:

- dev: `mcp-dev@gabriel-esparza.com`
- prod: `mcp@gabriel-esparza.com`

Every email sets `ReplyToAddresses` to `esparza.gabriel@gmail.com` by default (server-injected unless the caller overrides), so replies reach a real inbox.

### 1.4 Leave the sandbox

Sandbox status is per **account and region** — dev and prod share `us-west-2`, so production access lifts the sandbox for both at once. Dev's recipient restriction is therefore not the sandbox but the `RecipientAllowList` guardrail (enforced in code with 100 % coverage); the sandbox merely doubles it up until access is granted. Request production access via Console → SES → Account dashboard → *Request production access*, or:

```bash
aws sesv2 put-account-details \
  --production-access-enabled \
  --mail-type TRANSACTIONAL \
  --website-url https://mcp.gabriel-esparza.com \
  --use-case-description file://docs/setup/ses-use-case.txt \
  --additional-contact-email-addresses esparza.gabriel@gmail.com \
  --contact-language EN
```

Suggested `ses-use-case.txt` (edit to taste, keep it factual):

> This account sends low-volume transactional email from a personal automation service operated by the account owner (Gabriel Esparza). Messages are generated on demand by the owner's AI assistant (Claude) through a private, authenticated API and are sent to the owner himself and to a small number of individuals who have asked him for the information (for example a document link, a reminder, or a confirmation). Expected volume is under 500 messages per month. There is no marketing content, no purchased lists, and no bulk sending. Bounces and complaints are delivered to a CloudWatch log group via an SES configuration set event destination and reviewed by the owner; a server-side allow-list restricts sender addresses, and per-hour and per-day rate limits are enforced before any call to SES. Recipients can reply directly to the owner's address, which is set as Reply-To on every message.

Approval is usually 24–48 hours; AWS sometimes asks follow-up questions by email.

## 2. AWS End User Messaging — toll-free number (SMS + MMS)

### 2.1 Request the number (CloudFormation)

The toll-free number and the protect configuration are stack-managed —
`infra/eum.yaml`, deployed once from the workstation like `ses-domain`
(M3-1; `AWS::SMSVOICE::*` became CFN-native). The stack writes the number,
its ARN/id, and the protect-configuration id to SSM under
`/messaging-mcp/eum/*`, so nothing number-shaped lives in the repo.

```bash
aws cloudformation deploy --stack-name aws-messaging-mcp-eum \
  --template-file infra/eum.yaml --no-execute-changeset
# review, then execute the change set — allocation starts the ~$2/month lease
```

The number is deletion-protected and `Retain`ed: carrier verification is not
reproducible on a re-created number.

### 2.2 Toll-free verification form

Unverified toll-free traffic is filtered by carriers; submit verification right after the number is allocated (Console → Phone numbers → select number → *Create registration* → Toll-free). Suggested answers:

| Field | Suggested value |
| --- | --- |
| Company name | Gabriel Esparza |
| Company website | `https://mcp.gabriel-esparza.com` (serves a one-page description + the opt-in page below) |
| Company address / contact | your details |
| Use case category | **Transactional** / Account notifications |
| Message volume (monthly) | < 1,000 |
| Use case description | *Personal notification service. The account owner's AI assistant sends one-off transactional text messages on the owner's behalf: reminders, confirmations, and links to documents the recipient requested. Recipients are the owner himself and individuals who explicitly asked to receive the message. No marketing, no recurring campaigns.* |
| Opt-in workflow description | *Recipients opt in by asking the owner directly (in person, by phone, or in writing) to send them the information by text. The owner records the request before the message is sent; the service enforces a recipient allow-list in non-production and rate limits in production. Every message includes the sender's name. Recipients can reply STOP to opt out, which is handled automatically by AWS End User Messaging.* |
| Opt-in image URL | Required. Host a simple page at `https://mcp.gabriel-esparza.com/opt-in` (static HTML served by the Lambda) that says: *"By asking Gabriel to text you, you agree to receive a one-time message from 8xx-xxx-xxxx. Message and data rates may apply. Reply STOP to opt out, HELP for help."* — and link its URL/screenshot. |
| Sample message 1 | `Gabriel Esparza: Here is the document you asked for: https://mcp.gabriel-esparza.com/files/... (link expires in 7 days). Reply STOP to opt out.` |
| Sample message 2 | `Gabriel Esparza: Reminder — our call is today at 3:00 PM PT. Reply STOP to opt out.` |
| Additional info | *Single-sender, owner-operated service; sends are authenticated with OAuth and rate-limited server-side.* |

Carriers typically review in 1–2 weeks. Until approved, expect heavy filtering; keep E2E tests pointed at your own handset and accept occasional delivery `xfail`.

### 2.3 Spend and protect settings

- **SMS sandbox (M3-6):** the EUM account starts in tier `SANDBOX` — sends reach only *verified destination numbers* and every spend limit is capped at $1/month. Verify your own handset for e2e (`aws pinpoint-sms-voice-v2 create-verified-destination-number --destination-phone-number +1XXXXXXXXXX`, then `send-destination-number-verification-code` and `verify-destination-number` with the texted code). Request production access (Console → AWS End User Messaging SMS → Account → *Request production access*) **after** the toll-free verification completes.
- Account-level SMS spend limit: once production access raises the ceiling, set it to **$20/month** (`aws pinpoint-sms-voice-v2 set-text-message-spend-limit-override --monthly-limit 20`); in the sandbox the $1 cap already applies.
- Create a **protect configuration** that allows only `US` and attach it to the configuration set; the stack parameter `ProtectConfigurationId` carries it.

## 3. RCS — descoped

RCS was removed from scope on 2026-08-23 (PRD Appendix C): brand
verification, per-country launch registration, and carrier review are
disproportionate for a personal tool that SMS/MMS covers.

## 4. Cognito user

The stack creates the user pool; create the single user afterwards (self-signup is disabled):

```bash
go run ./cmd/ops bootstrap-user --stage prod --email esparza.gabriel@gmail.com
# → creates the user with a temporary password, forces reset on first hosted-UI login, and requires TOTP enrolment
```

E2E needs no user at all: the dev-only confidential `ci` app client authenticates with the `client_credentials` grant, and its id/secret live in SSM (`/messaging-mcp/dev/ci-client-{id,secret}`) — see [`docs/testing.md`](../testing.md).

## 5. Test recipients (dev)

| Kind | Value | Used by |
| --- | --- | --- |
| Email | `esparza.gabriel@gmail.com` (or a `+mcp-e2e` alias) — must be **verified in SES** because dev stays in the sandbox | `ses_send_email` E2E |
| Phone | Owner's mobile — GitHub secret `E2E_TEST_PHONE`, never committed | `sms_*` E2E, RCS test device |

These go into `RecipientAllowList` for dev and into the GitHub `dev` environment variables `E2E_TEST_EMAIL` / `E2E_TEST_PHONE`.

## 6. CloudFront signing key

```bash
./scripts/rotate-signing-key.py --stage dev    # generates RSA-2048, stores private key in SSM, prints public PEM
```

Pass the printed PEM as `SigningPublicKeyPem` when deploying. Use a different key per stage.

## 7. CloudFront Free plan and Cognito tier

- Flat-rate plans are available in this account (CloudFront console shows a **Pricing plan** column and a *Create a flat-rate distribution* flow; the existing `hsa-shoebox-site` distribution is on pay-as-you-go). After the first deploy creates the distribution, enrol it in the **Free** plan (Console → CloudFront → distribution → Pricing plan) unless the template can set it directly. Confirm the plan shows WAF included; otherwise WAF bills at pay-as-you-go (~$6/month) until fixed.
- Cognito pools created by CloudFormation default to the **Essentials** tier; confirm in the console (User pool → Settings → Feature plan). Do not select Plus.

## 8. Budget alarm

`AWS::Budgets::Budget` in the stack needs an email for notifications; `esparza.gabriel@gmail.com` is the default parameter value. Confirm the SNS subscription email when it arrives or alarms will never deliver.

## Checklist

- [ ] NS delegation for `mcp` added at GoDaddy ([`dns.md`](dns.md))
- [ ] DKIM CNAMEs ×3, `_dmarc` TXT, root SPF TXT added at GoDaddy
- [ ] SES domain identity shows **Verified**
- [ ] SES production access requested (prod) / granted
- [ ] Test email address verified in SES (dev sandbox)
- [ ] Toll-free number allocated; ARN recorded
- [ ] Toll-free verification submitted / approved
- [ ] SMS monthly spend limit set; protect configuration created
- [ ] Cognito owner user created, TOTP enrolled
- [ ] `e2e-ci` user + `ci` app client created (dev)
- [ ] Signing key pair generated per stage
- [ ] CloudFront distribution enrolled in Free plan
- [ ] Budget notification email confirmed
