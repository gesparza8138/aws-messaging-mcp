# Connecting Claude clients

How each Claude surface connects to the MCP server (PRD §3, Appendix B).
Both clients use **pre-registered public OAuth clients** on the Cognito user
pool (PRD §6.2) — Cognito supports neither dynamic client registration nor
CIMD, so the client id is pasted in.

| Value | dev | prod |
| --- | --- | --- |
| MCP URL | `https://dev.mcp.gabriel-esparza.com/mcp/` | `https://mcp.gabriel-esparza.com/mcp/` |
| Claude Code client id (`ClaudeCodeClientId` output) | `6giq64ctd72hqras9tb6udlkph` | `csorma81n5c73hpmap2kes337` |
| Claude hosted client id (`ClaudeHostedClientId` output) | `1l5s2dpcaq6rqptf4toil9dmsg` | `4banpj0uq14bfe7a5vs8g374j6` |
| Hosted UI | `https://messaging-mcp-dev.auth.us-west-2.amazoncognito.com` | `https://messaging-mcp-prod.auth.us-west-2.amazoncognito.com` |

Client ids are public by design (PKCE public clients); the security is in the
exact-match redirect URIs, PKCE, TOTP MFA, and 15-minute access tokens.

## First login (once per stage)

The owner user is created by `go run ./cmd/ops bootstrap-user` with a temporary
password. The **first OAuth flow from any client** walks you through: change
password → enrol TOTP (scan the QR code with your authenticator app) → consent.
Every later login is password + TOTP code.

## Claude Code

```bash
claude mcp add --transport http aws-messaging-dev https://dev.mcp.gabriel-esparza.com/mcp/ \
  --client-id 6giq64ctd72hqras9tb6udlkph \
  --callback-port 8765 \
  --scope user
claude mcp login aws-messaging-dev
```

> [!WARNING]
> Cognito matches redirect URIs exactly, port included, so `--callback-port 8765`
> is mandatory — it is what the `claude-code` app client has registered
> (`http://localhost:8765/callback` and `http://127.0.0.1:8765/callback`).

Your workstation's public IP must be in the edge WAF allow-list; refresh it
with `scripts/update-my-ip.sh` when it changes (PRD R7).

Then, in a Claude Code session: ask it to call the `hello` tool on
`aws-messaging-dev`. The response echoes the stage and your Cognito `sub`.

### PRD R2 (Cognito PKCE metadata) - verified

Cognito omits `code_challenge_methods_supported`; the current Claude Code and
the hosted bridge both complete PKCE S256 regardless (verified 2026-08-22 on
dev in `direct` mode). If a future client version regresses, set
`AuthMetadataMode=fronted` in `infra/params/<stage>.json`, redeploy, and
re-login - the server then advertises its own RFC 8414 document with the field.

## Claude Desktop / claude.ai (custom connector)

Settings → Connectors → **Add custom connector**:

- Name: anything (e.g. `Gabes Dev MCP`)
- URL: `https://dev.mcp.gabriel-esparza.com/mcp/` (with or without the trailing slash)
- OAuth Client ID: `1l5s2dpcaq6rqptf4toil9dmsg`
- Client secret: *(leave empty)*

Expand **Advanced settings** to reach the client-id field - without it the
connector attempts dynamic registration and fails. Connect → Cognito hosted UI
→ password + TOTP → consent. Anthropic's hosted
bridge stores the refresh token (365 days, rotated) so Routines keep working
without re-authentication (PRD G2).

## Routines

Create a Routine that calls the `hello` tool on the connector. To prove the
refresh path, run it **more than 15 minutes** after the last interactive
login — the access token will have expired and the bridge must refresh.

## Break-glass (temporary, dev)

Only when OAuth is broken and you need the tools now (PRD A7, runbook
`docs/runbooks/auth-recovery.md`):

```bash
claude mcp add --transport http aws-messaging-dev-bg https://dev.mcp.gabriel-esparza.com/mcp/ \
  --header "Authorization: Bearer ${MSG_MCP_BREAK_GLASS}"
```

The token is shown once by `go run ./cmd/ops rotate-secret`; only its SHA-256
is stored. Rotate it again when OAuth is back.
