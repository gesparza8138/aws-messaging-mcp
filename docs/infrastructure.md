# Infrastructure as code

Every AWS resource is CloudFormation — no SAM, no CDK, no Terraform, and
(almost) no console. This page maps the stacks, how values flow into them,
and the conventions that keep deploys safe in any order. Companions:
[server.md](server.md), [cicd.md](cicd.md),
[setup/deploy.md](setup/deploy.md).

## The stacks

| Stack | Deployed | Holds | Why its own stack |
| --- | --- | --- | --- |
| `bootstrap` | Workstation, rarely | GitHub OIDC provider, per-stage deploy roles, CFN service role, immutable artifact bucket | The trust root: the only stack ever touched by human credentials, so its blast radius is isolated and every change is a reviewed change set |
| `edge` | Workstation, rarely | `mcp.` hosted zone, ACM cert (us-east-1), the (empty) prod web ACL | Global/us-east-1 resources with `Retain` zones — recreating them changes NS records, learned the hard way |
| `root-dns` | Workstation, rarely | The whole `gabriel-esparza.com` zone: apex/site records, delegation to `mcp.`, SES DKIM/SPF/DMARC targets | Domain-wide DNS outlives any app decision |
| `ses-domain` | Workstation, once | Shared SES domain identity, Easy DKIM, MAIL FROM records | One identity per account-region, shared by both stages (M2-3) |
| `eum` | Workstation, once | The toll-free number (deletion-protected, `Retain`) and US protect configuration | The number's carrier verification doesn't survive re-creation; ids go to SSM so nothing number-shaped enters the public repo |
| `app` ×2 (`dev`, `prod`) | **CI only** | Everything else: Cognito, Lambda + Function URL, CloudFront distribution + behaviors, buckets, DynamoDB, config sets, alarms, dashboard, schedules | The actual service, one stack per stage, identical templates parameterized by stage |

The few things CloudFormation cannot express are scripted, not clicked:
Free-plan enrolment (`pricing-plan-manager` API), live allow-list
republishes (`scripts/update-my-ip.sh`, `scripts/e2e-allowlist.sh`), and
secret/key generation (`cmd/ops`). The only recurring console task left is
carrier/production-access paperwork.

## How values reach a deploy

Three flows, in increasing sensitivity:

1. **Params files** (`infra/params/{dev,prod}.json`, committed): stage
   knobs — sender addresses, allow-lists, rate limits, quotas.
2. **Workflow-resolved values** (read from SSM at deploy time, passed as
   parameters): the edge CIDR list, the toll-free number and ARN, the
   protect-configuration id, the files public key PEM. Empty-safe: a stage
   deploys cleanly before the source stack exists.
3. **NoEcho secrets** (SSM SecureString → parameter, never logged): origin
   secret, break-glass hash. The owner's phone number takes a fourth path —
   a GitHub secret merged into the recipient allow-list parameter at deploy
   time, so it exists in no file anywhere.

The CLI's `--parameter-overrides` mangles comma-laden values, so the
workflows build a JSON parameter file instead.

## Conventions that keep deploys boring

- **Conditional wiring beats ordering.** Cross-stack features gate on their
  inputs existing: no phone ARN → no SMS IAM statement and the tools don't
  register; no signing key → no key group, no `/files/*` behavior, no
  cleanup schedule. Any stack can deploy at any time; features light up
  when their prerequisites land.
- **Preview, then execute.** Every deploy creates a change set first; dev
  prints it to the job summary (`execute=false` to stop there), bootstrap
  changes are reviewed by hand, prod sits behind the environment gate.
- **`Retain` what can't come back**: hosted zones, the toll-free number.
  Rollbacks must never destroy identity-bearing resources.
- **Least privilege in layers**: CI identities can only pass the CFN
  service role; the service role's mutating actions are name-scoped to
  `aws-messaging-mcp-*`; the Lambda role grants per-resource actions with
  conditions (`ses:FromAddress` pins senders; objects only under the
  prefixes the tools use). `iam:PassRole` is service-conditioned to exactly
  the services that need a role passed.
- **Scanner suppressions are documented in place** — every `checkov`/
  `cfn_nag` skip carries its justification in the template metadata, so the
  iac-scans gate stays meaningful.
- **Buckets are single-purpose**: artifacts (CI-written, immutable), media
  (MMS staging, private, 7-day lifecycle), files (the *only* bucket behind
  CloudFront, OAC-locked, signed-URL-gated). What is internet-reachable is
  always obvious.

## Flat-rate plan constraints (prod)

The prod distribution rides the CloudFront Free plan, which has bitten
three times and is now a design input: no restricted price classes, the
web ACL may protect only the subscribed distribution (so it is empty and
prod-only), and **custom response-headers policies are rejected** — the
`/files/*` behavior uses the managed `SecurityHeadersPolicy` instead.
CloudFront metrics live only in us-east-1, so they are dashboard widgets,
not alarms. Dev stays pay-as-you-go and tolerates all of the above, which
is exactly why prod releases still go through a real deploy + smoke test.
