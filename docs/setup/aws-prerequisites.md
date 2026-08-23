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
| `RcsAgentId` | test agent | launched agent | §3 |
| `RecipientAllowList` (dev only) | `esparza.gabriel@gmail.com`, owner's mobile (GitHub secret `E2E_TEST_PHONE`) | — | §5 |
| `CognitoUserEmail` | `esparza.gabriel@gmail.com` | same | §4 |
| `SigningPublicKeyPem` | from `scripts/rotate-signing-key.py` | separate key | §6 |

## 1. Amazon SES

### 1.1 Domain identity (once, shared by both stages)

`infra/app.yaml` (prod) creates `AWS::SES::EmailIdentity` for `gabriel-esparza.com` with Easy DKIM (RSA 2048) and outputs the three DKIM tokens. Because the root domain's DNS is at GoDaddy, add these by hand:

| Type | Host (GoDaddy "Name") | Value | Purpose |
| --- | --- | --- | --- |
| CNAME | `<token1>._domainkey` | `<token1>.dkim.amazonses.com` | DKIM |
| CNAME | `<token2>._domainkey` | `<token2>.dkim.amazonses.com` | DKIM |
| CNAME | `<token3>._domainkey` | `<token3>.dkim.amazonses.com` | DKIM |
| TXT | `_dmarc` | `v=DMARC1; p=quarantine; rua=mailto:esparza.gabriel@gmail.com; adkim=s; aspf=r` | DMARC (no mail is hosted on the domain, so a strict policy is safe) |
| TXT | `@` | `v=spf1 -all` | Root SPF: nothing sends as the bare domain; SES uses the MAIL FROM subdomain below |

Get the tokens with `aws sesv2 get-email-identity --email-identity gabriel-esparza.com --query DkimAttributes.Tokens`. SES shows **Verified** within minutes to an hour after the CNAMEs resolve.

### 1.2 Custom MAIL FROM (automated)

Per stage, the stack sets `MailFromAttributes.MailFromDomain` to `bounce.<stage-host>` and creates the required `MX` and `TXT (v=spf1 include:amazonses.com -all)` records in the Route 53 zone. This keeps SPF alignment without touching GoDaddy and gives bounces a place to land (SES event destination → CloudWatch Logs).

### 1.3 Sender addresses

No mailboxes exist on `gabriel-esparza.com`, so senders are purely outbound identities. The stack's `SesSenderAddresses` allow-list is the only thing that distinguishes stages:

- dev: `mcp-dev@gabriel-esparza.com`
- prod: `mcp@gabriel-esparza.com`

Every email sets `ReplyToAddresses` to `esparza.gabriel@gmail.com` by default (server-injected unless the caller overrides), so replies reach a real inbox.

### 1.4 Leave the sandbox (prod only)

Dev stays in the sandbox: it can only send to verified addresses, which is exactly the recipient allow-list behaviour we want. Prod needs production access. Console → SES → Account dashboard → *Request production access*, or:

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

### 2.1 Request the number

Console → AWS End User Messaging SMS → Phone numbers → *Request originator* → Country **United States**, capabilities **SMS + MMS**, number type **Toll-free**, two-way **off**. Or:

```bash
aws pinpoint-sms-voice-v2 request-phone-number \
  --iso-country-code US --message-type TRANSACTIONAL \
  --number-capabilities SMS MMS --number-type TOLL_FREE \
  --deletion-protection-enabled
```

Cost ≈ $2/month. Note the `PhoneNumberArn` and `PhoneNumberId`.

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

- Account-level SMS spend limit: set to **$20/month** (Console → Account settings, or `aws pinpoint-sms-voice-v2 set-text-message-spend-limit-override --monthly-limit 20`).
- Create a **protect configuration** that allows only `US` and attach it to the configuration set; the stack parameter `ProtectConfigurationId` carries it.

## 3. AWS End User Messaging — RCS

RCS is the most paperwork-heavy channel and the most likely to slip, which is why it is its own milestone (M4). Requirements at time of writing:

1. **Brand**: name, description, logo (224×224 PNG), hero image (1440×448), brand color, contact phone + email, **privacy-policy URL and terms-of-service URL** (both mandatory — host minimal pages at `https://mcp.gabriel-esparza.com/legal/privacy` and `/legal/terms`).
2. **RCS agent**: created under the brand with the above assets; the agent ID is the `OriginationIdentity` for `rcs_send_message`.
3. **Testing registration**: register your own device(s) as test devices so the agent can message them before launch. This is enough for `dev` and all E2E tests.
4. **Country launch registration (US)**: carrier review of the agent and use case before it can message the public. Use the same use-case text as §2.2. Expect several weeks; prod RCS is gated on this.
5. **Fallback**: RCS messages to non-RCS devices are dropped unless `Fallback` is supplied; the toll-free number from §2 is the fallback origination identity.

> [!WARNING]
> Brand verification for RCS is designed for businesses. A personal brand may be rejected or asked for proof of business registration. If that happens, keep RCS in **testing registration** only (messages to your registered devices) and treat public RCS as out of scope; SMS/MMS via toll-free covers everyone else.

## 4. Cognito user

The stack creates the user pool; create the single user afterwards (self-signup is disabled):

```bash
go run ./cmd/ops bootstrap-user --stage prod --email esparza.gabriel@gmail.com
# → creates the user with a temporary password, forces reset on first hosted-UI login, and requires TOTP enrolment
```

For `dev`, the script also creates the `e2e-ci` user with a permanent password (stored only as the GitHub environment secret) and the confidential `ci` app client with `USER_PASSWORD_AUTH`.

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
- [ ] RCS brand + agent created; test device registered
- [ ] RCS US launch registration submitted (prod, optional)
- [ ] Cognito owner user created, TOTP enrolled
- [ ] `e2e-ci` user + `ci` app client created (dev)
- [ ] Signing key pair generated per stage
- [ ] CloudFront distribution enrolled in Free plan
- [ ] Budget notification email confirmed
