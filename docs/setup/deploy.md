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

## 4. App stack

Arrives with M1 (PR-4): built and deployed by `deploy-dev.yml` under the `dev`
environment, using the artifact bucket and the CloudFormation service role.
Parameters reference will be documented alongside `infra/app.yaml`.

## Rollback

`bootstrap` and `edge` roll back automatically on failed create. For the app
stack, see PRD §10.3 (immutable artifacts, `scripts/rollback.sh`).
