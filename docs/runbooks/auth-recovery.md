# Runbook: auth recovery

When a client (Claude Code, Claude Desktop/claude.ai, a Routine) starts
failing with 401s or "authorization failed".

## 1. Find out which layer says no

```bash
# Last 15 minutes of requests by source and status (dev)
aws logs filter-log-events --log-group-name /aws/lambda/aws-messaging-mcp-dev \
  --region us-west-2 --start-time $(( ($(date +%s) - 900) * 1000 )) \
  --filter-pattern '"HTTP/1.1"' --query 'events[].message' --output text \
  | grep -oE '[0-9.]+:0 - "[A-Z]+ [^"]*" [0-9]+' | sort | uniq -c | sort -rn
```

| What you see | Meaning | Fix |
| --- | --- | --- |
| Nothing from the client's IP | Blocked at the WAF | `scripts/update-my-ip.sh` (your IP changed) or check Anthropic's egress range in the edge IP set |
| `403` on `/mcp/` | Request bypassed CloudFront (no origin secret) | Client is using the raw Function URL; point it at the public hostname |
| `401` on `/mcp/` with a token present | Token rejected: expired, wrong client id, wrong pool | Re-login; if it persists, compare the token's `iss`/`client_id` with the stack outputs |
| `200`s but the client still says unauthenticated | Client-side token storage problem (Claude Code R2 bug) | Switch the stage to `AuthMetadataMode=fronted`, redeploy, re-login |
| Client error before any request arrives | Discovery failed | Confirm `/.well-known/oauth-protected-resource` returns 200 from outside |

Anthropic's hosted bridge comes from `160.79.106.x`; your workstation is the
IP you last registered.

## 2. Re-consent

- **Claude Code:** `claude mcp logout aws-messaging-dev && claude mcp login aws-messaging-dev`
- **Desktop / claude.ai:** Settings → Connectors → the connector → Reconnect
- Routines reuse the connector's credentials; reconnecting the connector fixes them.

## 3. Break-glass (last resort)

Grants `msg/read` only in dev (scopes are a stack parameter). Token shown once
by `scripts/rotate-secret.py`; only its SHA-256 is stored.

```bash
claude mcp add --transport http aws-messaging-dev-bg https://dev.mcp.gabriel-esparza.com/mcp/ \
  --header "Authorization: Bearer ${MSG_MCP_BREAK_GLASS}"
```

Rotate it again (`rotate-secret.py --stage dev --break-glass-only`) once
OAuth is back. Break-glass is off in prod by default (PRD S10); enabling it
is a parameter change and therefore an approved pipeline run.

## 4. Cognito user locked / MFA lost

```bash
aws cognito-idp admin-set-user-mfa-preference --user-pool-id <pool> --username <email> \
  --software-token-mfa-settings Enabled=false,PreferredMfa=false --region us-west-2
```

then re-login to re-enrol TOTP. Password reset: `admin-set-user-password --permanent`.
