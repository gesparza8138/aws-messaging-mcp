# Architecture

Deep-dive companion to [PRD §4](PRD.md#4-architecture). This page tracks what
is **implemented**; the PRD describes the target. Updated per milestone.
Subsystem detail lives in dedicated pages: [server.md](server.md) (the
Lambda and every tool), [cicd.md](cicd.md) (pipeline and security gates),
and [infrastructure.md](infrastructure.md) (stacks and IaC conventions).

## Application layout (M1, Go)

```text
cmd/server/main.go          # Lambda bootstrap (Function URL events) or local HTTP (--listen)
cmd/ops/main.go             # rotate-secret, bootstrap-user
internal/settings/          # env -> Settings; SSM origin-secret resolver (cold start)
internal/auth/              # origin secret, break-glass, JWT verifier + JWKS cache, scopes (100 %)
internal/httpapi/           # mux + middleware chain + OAuth metadata documents
internal/mcpserver/         # MCP server (go-sdk), hello tool; M2+: ses/sms/rcs/files
internal/lambdaadapter/     # buffered Function URL event <-> http.Handler
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
3. **Tool dispatch** — the MCP server (official Go SDK, Streamable HTTP,
   stateless, JSON responses per PRD R3) runs the tool; each tool calls
   `auth.RequireScope` itself, so a missing scope is a *tool error* the model
   can read, not a transport 401.

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

- The MCP SDK's localhost/DNS-rebinding (Host header) protection is disabled:
  CloudFront terminates the public hostname, so the Host seen by Lambda is the
  Function URL's; the origin secret and bearer auth are the actual gate.
- The SDK's built-in auth hooks are not used; the PRD-mandated chain (origin →
  break-glass → JWT → scopes) lives in explicit, fully unit-tested code under
  `internal/auth/` (100 % coverage enforced in CI).
- `/mcp` and `/mcp/` are both registered on the mux and no redirect is ever
  issued (a redirect would carry the Lambda's Host - the raw Function URL).
- The JWKS cache is lazy (first token fetches) and serves a stale set through
  a transient fetch failure, so a cold start never blocks on Cognito.
- Cold start: 217 ms init at 256 MB (`provided.al2023`, arm64), versus
  ~2,950 ms for the Python/Web Adapter predecessor.

## Testing

Unit tests cover the full PRD §11.1 auth matrix and the Function URL
adapter. Integration tests (`internal/httpapi`) run the real handler under
`httptest` with a locally generated RSA key standing in for the Cognito
JWKS, then exercise genuine MCP round-trips with the go-sdk client
(`initialize` → `tools/list` → `tools/call`) at both `/mcp` and `/mcp/` with
OAuth and break-glass tokens, plus the 401/403/metadata contracts, offline.
