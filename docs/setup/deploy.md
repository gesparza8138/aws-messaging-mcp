# Deploying the stacks

Three stacks (PRD §9.1): `bootstrap` and `edge` are deployed **once, manually,
from the owner's workstation** — the only deploys that ever use human AWS
credentials. The `app` stack (M1+) deploys exclusively through GitHub Actions
with OIDC.

> [!IMPORTANT]
> House rules for every deploy, dev included: review the change set before
> executing it, and never put the AWS account ID or your home IP in a committed
> file — this is a public repository. Use `aws sts get-caller-identity` for the
> account and pass IPs as CLI parameter overrides.

## 1. Bootstrap stack (`infra/bootstrap.yaml`, us-west-2)

Creates the GitHub OIDC provider, the per-stage deploy roles (trust scoped to
`environment:dev` / `environment:prod`), the CloudFormation service role, and
the artifact bucket.

```bash
aws sso login   # or however you obtain credentials
aws cloudformation deploy \
  --stack-name aws-messaging-mcp-bootstrap \
  --template-file infra/bootstrap.yaml \
  --capabilities CAPABILITY_NAMED_IAM \
  --region us-west-2 \
  --no-execute-changeset
# review the change set shown, then execute it:
aws cloudformation execute-change-set --change-set-name <arn-from-above> --region us-west-2
aws cloudformation describe-stacks --stack-name aws-messaging-mcp-bootstrap \
  --region us-west-2 --query 'Stacks[0].Outputs' --output table
```

Afterwards, confirm the role ARNs in the outputs match the GitHub environment
secrets `AWS_DEPLOY_ROLE_ARN` (dev and prod) set by `scripts/setup-github.sh`.

## 2. Edge stack (`infra/edge.yaml`, us-east-1)

Creates the hosted zone, the ACM certificate (DNS-validated), and the WAF
WebACL + IP set. **The stack will sit in `CREATE_IN_PROGRESS` until the NS
delegation at GoDaddy is live** — CloudFormation writes the ACM validation
CNAME into the new zone, but ACM can only see it once the four NS records
exist at GoDaddy ([dns.md](dns.md) §2). Sequence:

```bash
aws cloudformation deploy \
  --stack-name aws-messaging-mcp-edge \
  --template-file infra/edge.yaml \
  --region us-east-1 \
  --parameter-overrides HomeIpCidr="$(curl -s https://checkip.amazonaws.com)/32" \
  --no-execute-changeset
# review, execute, then immediately fetch the name servers:
aws route53 list-hosted-zones-by-name --dns-name mcp.gabriel-esparza.com \
  --query 'HostedZones[0].Id' --output text
aws route53 get-hosted-zone --id <ZoneId> --query 'DelegationSet.NameServers'
```

Add the four NS records at GoDaddy while ACM waits, then verify:

```bash
dig +short NS mcp.gabriel-esparza.com @8.8.8.8
aws cloudformation wait stack-create-complete --stack-name aws-messaging-mcp-edge --region us-east-1
```

## 3. Verify OIDC and the prod gate

Run the `verify-oidc` workflow (`gh workflow run verify-oidc.yml`):

- the **dev** job must print the dev role identity (account masked);
- the **prod** job dispatched from `main` must be rejected by the prod
  environment's tag-only deployment branch policy; dispatched from a `v*` tag
  it waits for the required reviewer — either outcome proves a prod deploy
  cannot happen without the gate.

## 3b. Edge -> app handoff (SSM)

The app stack resolves edge values through SSM dynamic references so no ARN
ever enters the repo. After any edge (re)deploy, write its outputs to
us-west-2 (note: SSM reserves names starting with `aws`, hence the
`/messaging-mcp/` prefix):

```bash
aws ssm put-parameter --region us-west-2 --name /messaging-mcp/edge/hosted-zone-id   --type String --value <HostedZoneId>   --overwrite
aws ssm put-parameter --region us-west-2 --name /messaging-mcp/edge/certificate-arn  --type String --value <CertificateArn>  --overwrite
aws ssm put-parameter --region us-west-2 --name /messaging-mcp/edge/web-acl-arn      --type String --value <WebAclArn>      --overwrite
aws ssm put-parameter --region us-west-2 --name /messaging-mcp/edge/ip-set-arn       --type String --value <AllowedIpSetArn> --overwrite
```

## 3c. SES domain stack (`infra/ses-domain.yaml`, us-west-2, once)

Creates the shared domain identity with Easy DKIM and writes every email-auth
record itself: three DKIM CNAMEs, root SPF (`v=spf1 -all`), and DMARC into the
Route 53 root zone (id in SSM `/messaging-mcp/root-zone-id`), plus the custom
MAIL FROM `MX`/`TXT` into the `mcp` zone. Nothing to do at a registrar.

```bash
aws ssm put-parameter --region us-west-2 --name /messaging-mcp/root-zone-id --type String --value <RootZoneId> --overwrite
aws cloudformation deploy --stack-name aws-messaging-mcp-ses-domain \
  --template-file infra/ses-domain.yaml --region us-west-2 --no-execute-changeset
# review, execute, then watch SES verify the identity (minutes):
aws sesv2 get-email-identity --email-identity gabriel-esparza.com --region us-west-2 \
  --query '{Verified:VerifiedForSendingStatus,Dkim:DkimAttributes.Status,MailFrom:MailFromAttributes.MailFromDomainStatus}'
```

SES sandbox status is per account-region, shared by both stages; until AWS
grants production access the account can only send to verified addresses.

## 4. App stack (`infra/app.yaml`)

Per-stage secrets must exist before the first deploy:

```bash
go run ./cmd/ops rotate-secret --stage dev   # origin secret + break-glass token
```

Then deploy through GitHub Actions only:

1. `gh workflow run deploy-dev.yml` - builds the static arm64 `bootstrap` binary
   (`provided.al2023`, no layer), uploads it content-hash-keyed, creates a change set, and prints the summary to the
   job summary **without executing**.
2. Review, then `gh workflow run deploy-dev.yml -f execute=true` to apply.

Stage parameters live in `infra/params/{dev,prod}.json`; secret-bearing
values (`OriginSecret`, `BreakGlassSha256`) are fetched from SSM by the
workflow and passed as NoEcho parameters, never committed. After the stack is
up, create the owner user:

```bash
go run ./cmd/ops bootstrap-user --stage dev --email <owner-email>
```

## Rollback

`bootstrap` and `edge` roll back automatically on failed create. For the app
stack, see PRD §10.3 (immutable artifacts, `scripts/rollback.sh`).
