# DNS setup: delegating `mcp.gabriel-esparza.com` from GoDaddy to Route 53

The domain `gabriel-esparza.com` is registered and DNS-hosted at GoDaddy. Rather than moving the whole domain, we **delegate one subdomain** to a Route 53 hosted zone. Everything under `mcp.gabriel-esparza.com` is then managed by CloudFormation; everything else at GoDaddy is untouched.

| Name | Purpose | Stage |
| --- | --- | --- |
| `mcp.gabriel-esparza.com` | MCP endpoint (`/mcp/`), OAuth metadata (`/.well-known/...`), shared files (`/files/*`) | prod |
| `dev.mcp.gabriel-esparza.com` | Same, dev stack | dev |
| `auth.mcp.gabriel-esparza.com` *(optional, later)* | Cognito hosted UI custom domain; the free `*.auth.us-west-2.amazoncognito.com` prefix domain is used until then | prod |

## 1. Create the hosted zone (AWS side)

The `edge` stack creates `AWS::Route53::HostedZone` for `mcp.gabriel-esparza.com` and outputs its four name servers. For the first-time bootstrap you can also do it by hand:

```bash
aws route53 create-hosted-zone \
  --name mcp.gabriel-esparza.com \
  --caller-reference "mcp-$(date +%s)" \
  --hosted-zone-config Comment="Delegated from GoDaddy for aws-messaging-mcp"
aws route53 get-hosted-zone --id <ZoneId> --query 'DelegationSet.NameServers'
```

You will get four names like `ns-123.awsdns-45.com`, `ns-678.awsdns-90.net`, `ns-1011.awsdns-12.org`, `ns-1314.awsdns-15.co.uk`.

## 2. Add the NS records at GoDaddy

GoDaddy → My Products → `gabriel-esparza.com` → **DNS** → *Add new record*, four times:

| Type | Name | Value | TTL |
| --- | --- | --- | --- |
| NS | `mcp` | `ns-123.awsdns-45.com` | 1 hour |
| NS | `mcp` | `ns-678.awsdns-90.net` | 1 hour |
| NS | `mcp` | `ns-1011.awsdns-12.org` | 1 hour |
| NS | `mcp` | `ns-1314.awsdns-15.co.uk` | 1 hour |

Notes:

- Enter the **host as `mcp`**, not the full name; GoDaddy appends the domain.
- Do **not** put a trailing dot on the values in GoDaddy's UI.
- Do not change the domain's own nameservers (the "Nameservers" section) — that would move the whole domain to Route 53.
- No other records under `mcp` should exist at GoDaddy; if an `A` or `CNAME` for `mcp` exists, delete it first.

Verify after a few minutes:

```bash
dig +short NS mcp.gabriel-esparza.com            # should list the four awsdns servers
dig +short NS mcp.gabriel-esparza.com @8.8.8.8
```

## 3. Certificates

`AWS::CertificateManager::Certificate` in the `edge` stack (region `us-east-1`, required by CloudFront) requests a cert for `mcp.gabriel-esparza.com` with SAN `*.mcp.gabriel-esparza.com`, using **DNS validation** with `HostedZoneId` pointing at the delegated zone, so CloudFormation writes the validation CNAME itself and the stack waits until issued. Nothing to do at GoDaddy.

## 4. Records CloudFormation manages

| Record | Type | Target | Stack |
| --- | --- | --- | --- |
| `mcp.gabriel-esparza.com` | A + AAAA alias | prod CloudFront distribution | app (prod) |
| `dev.mcp.gabriel-esparza.com` | A + AAAA alias | dev CloudFront distribution | app (dev) |
| `_<token>.mcp.gabriel-esparza.com` | CNAME | ACM validation | edge |
| `auth.mcp.gabriel-esparza.com` | A alias | Cognito custom domain CloudFront | app (prod), optional |

## 5. Email sender domain (SES) — decision needed

SES needs DKIM records for whatever domain appears in `From:`.

| Option | `From:` looks like | Where DKIM CNAMEs go | Notes |
| --- | --- | --- | --- |
| **A. Root domain** | `gabe@gabriel-esparza.com` | GoDaddy (3 CNAMEs under `<selector>._domainkey`) plus a `_dmarc` TXT | Nicest sender address; must not conflict with existing email provider's SPF — SES adds its own `include:amazonses.com` if you use a custom MAIL FROM |
| **B. Delegated subdomain** | `gabe@mcp.gabriel-esparza.com` | Route 53, fully automated by CloudFormation (`AWS::SES::EmailIdentity` + `DkimAttributes` + `Route53` records) | Zero GoDaddy changes; address is less pretty |
| **C. Verified address only** | `esparza.gabriel@gmail.com` | none | Works in SES sandbox for testing but Gmail's DMARC policy causes rejection/spoof warnings in production; not recommended |

Recommendation: **B for `dev`** (fully automated, no risk to your real mail) and **A for `prod`** once you add the three DKIM CNAMEs at GoDaddy. The PRD's sender allow-list is a stack parameter, so this is configuration, not code.

## 6. Cognito callback and OAuth metadata

These all derive from the hostnames above and are set as stack parameters:

- Protected resource metadata `resource`: `https://mcp.gabriel-esparza.com/mcp/` (prod), `https://dev.mcp.gabriel-esparza.com/mcp/` (dev)
- Cognito hosted UI: `https://<prefix>.auth.us-west-2.amazoncognito.com` until a custom domain is added
- Claude Code callback: `http://localhost:8765/callback`
- Claude hosted callback: `https://claude.ai/api/mcp/auth_callback`

## 7. Rollback

Deleting the four NS records at GoDaddy detaches the subdomain; nothing else on the domain is affected.
