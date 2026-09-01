# DNS setup: `gabriel-esparza.com` on Route 53

> [!IMPORTANT]
> **Superseded 2026-08-22 (see PRD Appendix C):** DNS hosting for the *whole*
> domain moved to Route 53 (`infra/root-dns.yaml`); only the registration
> stays at GoDaddy. The original subdomain-delegation model below survives
> unchanged as a *child zone*: `mcp.gabriel-esparza.com` still has its own
> hosted zone owned by the edge stack, but its NS delegation records now live
> in the Route 53 root zone instead of GoDaddy. The single remaining GoDaddy
> action, ever, is pointing the domain's nameservers at the root zone.

The domain `gabriel-esparza.com` is registered at GoDaddy. Everything under `mcp.gabriel-esparza.com` is managed by CloudFormation via the edge stack's child zone; the root zone (`infra/root-dns.yaml`) carries the pre-existing mail records, the `www` CNAME and the `mcp` delegation, and it holds the apex alias records written by the `apex-site` stack (§4).

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

## 2. Point the domain's nameservers at Route 53

Deploy `infra/root-dns.yaml` (once, manually — pass the mcp zone's four name
servers as `McpZoneNameServers`), then take the stack's `RootNameServers`
output and set them at the registrar:

GoDaddy → My Products → `gabriel-esparza.com` → **Nameservers** → *Change* →
*Enter my own nameservers* → paste the four `awsdns` values.

The root zone already carries the domain's pre-existing records (site `A`,
`www`, GoDaddy-mail `MX`) plus the `mcp` NS delegation, so nothing visibly
changes except who serves the DNS.

> [!NOTE]
> **Updated 2026-08-30:** the legacy site `A` record (the GoDaddy-era IP, which
> answered on neither port 80 nor 443) has been removed from the root zone,
> along with its `SiteIpAddress` parameter. The apex is now an alias to the
> `apex-site` stack's CloudFront distribution, which 301-redirects every request
> to `https://mcp.gabriel-esparza.com/`. It exists so that toll-free carrier
> verification reviewers find a live web presence at the company URL — the first
> registration was denied for "Company Verification Failed" (§4,
> [`../plans/pending.md`](../plans/pending.md)). The `www` CNAME is unchanged and
> now chains to that alias.

Verify after propagation (registrar changes can take up to an hour):

```bash
dig +short NS gabriel-esparza.com @8.8.8.8       # the four root-zone awsdns servers
dig +short NS mcp.gabriel-esparza.com @8.8.8.8   # the four child-zone awsdns servers
dig +short A gabriel-esparza.com @8.8.8.8        # apex redirect distribution (was the site IP)
```

## 3. Certificates

`AWS::CertificateManager::Certificate` in the `edge` stack (region `us-east-1`, required by CloudFront) requests a cert for `mcp.gabriel-esparza.com` with SAN `*.mcp.gabriel-esparza.com`, using **DNS validation** with `HostedZoneId` pointing at the delegated zone, so CloudFormation writes the validation CNAME itself and the stack waits until issued. Nothing to do at GoDaddy.

`infra/apex-site.yaml` (also `us-east-1`) requests a second certificate the same way — `gabriel-esparza.com` with SAN `www.gabriel-esparza.com`, validated into the **root** zone via its `RootZoneId` parameter. Because the zone is already live, issuance takes minutes.

## 4. Records CloudFormation manages

| Record | Type | Target | Stack |
| --- | --- | --- | --- |
| `gabriel-esparza.com` | A + AAAA alias | apex redirect distribution (301 → `https://mcp.gabriel-esparza.com/`) | apex-site |
| `_<token>.gabriel-esparza.com` | CNAME | ACM validation, apex cert (apex + `www` SAN) | apex-site |
| `www.gabriel-esparza.com` | CNAME | `gabriel-esparza.com` — unchanged, now chains to the apex alias | root-dns |
| `mcp.gabriel-esparza.com` | A + AAAA alias | prod CloudFront distribution | app (prod) |
| `dev.mcp.gabriel-esparza.com` | A + AAAA alias | dev CloudFront distribution | app (dev) |
| `_<token>.mcp.gabriel-esparza.com` | CNAME | ACM validation | edge |
| `auth.mcp.gabriel-esparza.com` | A alias | Cognito custom domain CloudFront | app (prod), optional |

The apex distribution serves no content of its own: a viewer-request CloudFront
Function 301s **every** request to the `mcp` landing page. It exists because
toll-free carrier verification reviewers check the company URL for a live web
presence, and the apex previously answered nothing. Deploy steps are in
[`deploy.md`](deploy.md) §3d.

## 5. Email sender domain (SES) — decision needed

SES needs DKIM records for whatever domain appears in `From:`.

| Option | `From:` looks like | Where DKIM CNAMEs go | Notes |
| --- | --- | --- | --- |
| **A. Root domain** | `gabe@gabriel-esparza.com` | Route 53 **root zone** (3 CNAMEs under `<selector>._domainkey`) plus a `_dmarc` TXT — automatable in CloudFormation since the whole domain moved to Route 53 | Nicest sender address. **Open question for M2:** the root zone carries GoDaddy-mail MX records; confirm whether that mailbox is used before adding a strict SPF/DMARC policy |
| **B. Delegated subdomain** | `gabe@mcp.gabriel-esparza.com` | Route 53 child zone, fully automated by CloudFormation (`AWS::SES::EmailIdentity` + `DkimAttributes` + `Route53` records) | Fully automated; address is less pretty |
| **C. Verified address only** | `esparza.gabriel@gmail.com` | none | Works in SES sandbox for testing but Gmail's DMARC policy causes rejection/spoof warnings in production; not recommended |

Recommendation: **B for `dev`** (fully automated, no risk to your real mail) and **A for `prod`** once you add the three DKIM CNAMEs at GoDaddy. The PRD's sender allow-list is a stack parameter, so this is configuration, not code.

## 6. Cognito callback and OAuth metadata

These all derive from the hostnames above and are set as stack parameters:

- Protected resource metadata `resource`: `https://mcp.gabriel-esparza.com/mcp/` (prod), `https://dev.mcp.gabriel-esparza.com/mcp/` (dev)
- Cognito hosted UI: `https://<prefix>.auth.us-west-2.amazoncognito.com` until a custom domain is added
- Claude Code callback: `http://localhost:8765/callback`
- Claude hosted callback: `https://claude.ai/api/mcp/auth_callback`

## 7. Rollback

Switch the domain's nameservers back to GoDaddy's defaults (GoDaddy keeps its
copy of the old zone); the Route 53 zones keep existing and can be re-pointed
at any time.
