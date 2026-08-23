# Architecture

Deep-dive companion to [PRD §4](PRD.md#4-architecture). This page tracks what
is **implemented**; the PRD describes the target. Updated per milestone.

## Application layout (M1)

```text
src/aws_messaging_mcp/
├── main.py          # FastAPI app: auth middleware, well-known routes, MCP mount
├── settings.py      # env-driven config + SSM secret resolution at cold start
├── auth/
│   ├── origin.py    # X-Origin-Secret constant-time check (first, cheapest)
│   ├── breakglass.py# static-token SHA-256 fallback (PRD A7)
│   ├── jwt.py       # Cognito access-token verification (PRD A8)
│   ├── scopes.py    # per-tool scope enforcement (PRD A6)
│   └── principal.py # the authenticated caller passed to tools
└── tools/           # hello (M1); ses/sms/rcs/files arrive M2-M4b
```

## Request path (implemented)

1. **Origin secret** — every request must carry `X-Origin-Secret` matching the
   SSM-provisioned value (constant-time compare). Enforced for *all* paths;
   disabled only for local development (`REQUIRE_ORIGIN_SECRET=false`).
   Missing configuration fails closed.
2. **Bearer auth** — for everything except `/healthz` and `/.well-known/*`:
   break-glass hash first (if enabled), then Cognito JWT (RS256 via JWKS
   cached ≤ 1 h, `iss`, `exp`, `token_use == "access"`, allow-listed
   `client_id`). Failures return `401` with
   `WWW-Authenticate: Bearer resource_metadata="…/.well-known/oauth-protected-resource"`.
3. **Tool dispatch** — the MCP server (python SDK ≥ 2.0, Streamable HTTP,
   stateless, JSON responses per PRD R3) runs the tool; each tool calls
   `require_scope` itself, so a missing scope is a *tool error* the model can
   read, not a transport 401.

## OAuth metadata

- `/.well-known/oauth-protected-resource` (and `…/mcp`): RFC 9728 document.
  `AUTH_METADATA_MODE` selects the advertised authorization server:
  - `direct` — the Cognito issuer itself.
  - `fronted` — this host (`{base}/oauth`), whose RFC 8414 document mirrors
    Cognito's endpoints **plus** `code_challenge_methods_supported: ["S256"]`,
    the field Cognito omits and some clients require for PKCE (PRD R2).
- `/.well-known/oauth-authorization-server[/oauth]`: the fronted document,
  built statically from the issuer + hosted-UI domain (no outbound fetch).

## Notable implementation decisions

- The MCP SDK's own DNS-rebinding (Host header) protection is disabled:
  CloudFront terminates the public hostname, so the Host seen by Lambda is the
  Function URL's; the origin secret and bearer auth are the actual gate.
- The SDK's built-in auth framework (`token_verifier` et al.) is not used; the
  PRD-mandated chain (origin → break-glass → JWT → scopes) lives in explicit,
  fully unit-tested modules under `auth/` (100 % coverage enforced in CI).
- `mcp>=2.0`: `MCPServer` with `streamable_http_app(stateless_http=True,
  json_response=True)` mounted under `/mcp` (canonical URL keeps the PRD's
  trailing slash: `POST /mcp/`).

## Testing

Unit tests cover the full PRD §11.1 auth matrix. Integration tests boot the
real app under uvicorn on a loopback port with a locally generated RSA key
standing in for the Cognito JWKS, then exercise genuine MCP round-trips
(`initialize` → `tools/list` → `tools/call`) with OAuth and break-glass
tokens, plus the 401/metadata contracts, entirely offline.
