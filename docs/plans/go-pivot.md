# Plan: pivot the MCP server from Python to Go

| Field | Value |
| --- | --- |
| Status | Approved 2026-08-23 — executing |
| Supersedes | Python toolchain sections of `docs/plans/m0-m1.md`; `docs/plans/m2.md` to be revised for Go before approval |

## Context

M0 and M1 are complete on dev and prod. The Python/FastAPI/Lambda Web Adapter
stack measured **~3.0 s cold-start init** (warm calls ~4 ms). The server will be
invoked a couple of times a day, so nearly every call is a cold start and Claude
waits ~3 s. Go on `provided.al2023`/arm64 typically inits in well under 100 ms
with no recurring cost (SnapStart/provisioned concurrency would eat the ≤ $2/mo
budget). The owner chose to pivot **now**, while the app is ~500 lines, before M2
(email) multiplies the code to port. Nothing in infra, DNS, auth design, CI
gating, or the deployed stacks changes except the Lambda packaging and the
application code.

**What the pivot must preserve exactly** (all verified live in M1): the auth
chain (origin secret → break-glass → Cognito JWT → scopes), the RFC 9728 /
RFC 8414 documents in both `direct` and `fronted` modes, the 401 contract (header
arrives remapped by the Function URL — clients fall back to well-known), `/mcp`
and `/mcp/` served identically with no redirects, stateless JSON Streamable
HTTP, the `hello` tool shape, and every environment-variable name `app.yaml`
already passes.

## Parked items — resume after the pivot (do not lose)

| Item | State | Resume action |
| --- | --- | --- |
| **M2 (email) plan** `docs/plans/m2.md` | Written, merged, **not yet approved** | Revise §2.3 (Go packages), §2.4 tests (SDK-reflection contract tests, fakes instead of moto, optional LocalStack), then request approval (gate M2-G1) |
| SES production access | Requested 2026-08-23, **PENDING** AWS review (email to owner) | When approved: M2 real sends; until then verify `esparza.gabriel@gmail.com` as an SES identity for sandbox sends (M2-G3) |
| Lambda concurrency quota | Request `4f0d5b06db494d6f929549b2c783d2abI11zymJo` PENDING (10 → 1000) | When approved: set `ReservedConcurrency=5` in `infra/params/*.json` (PRD S8) |
| R7 CI self-registration | Designed (m2.md M2-4), not built | Dev deploy role needs `cloudfront:{Get,Describe,Update,Publish}Function` on the dev function + `ssm:PutParameter` on `/messaging-mcp/edge/allowed-cidrs`; e2e job adds/removes runner IP |
| `scripts/enroll-pricing-plan.sh` | Broken with the local AWS CLI 2.25 (no `pricing-plan-manager` command); enrolment was done via boto3 | Rewrite against a current CLI or drop (subscription exists: `sub_3IKI0IVkTK0JDCSe2A740WsFoya`, prod dist + ACL + `mcp` zone) |
| Local tooling | AWS CLI 2.25 outdated; Go/golangci-lint not installed | `brew install go golangci-lint && brew upgrade awscli` (step 0) |
| Prod client connections | Claude Code prod login done (password reset 2026-08-23); Desktop prod connector + Routine not exercised | Optional: connector URL `https://mcp.gabriel-esparza.com/mcp/`, hosted client id `4banpj0uq14bfe7a5vs8g374j6` |
| Break-glass tokens | Shown once (dev, prod) in session output | Owner stores them; rotate with `scripts/rotate-secret.py --stage <s> --break-glass-only` (script stays Python → becomes `uv run`-less: port to Go CLI or keep as `uvx`-free python3 script using boto3 from a tiny venv — see §3.6) |
| PRD corrections found in M2 prep | M2-1..M2-5 captured in `docs/plans/m2.md` §1 | Fold into PRD when M2 executes |
| Scratchpad verify script (`verify_dev.py`) | Not in repo | Replaced by a Go live-check (§3.5) |
| Owner preferences | Saved in memory (concise summaries, copy-paste instructions, self-contained summaries, fast failure polling) | — |

## Target architecture (Go)

```text
cmd/server/main.go            # Lambda entry (bootstrap) or local HTTP (--listen :8000)
internal/settings/            # env → Settings (same variable names as today); SSM origin-secret resolver
internal/auth/
  origin.go                   # constant-time X-Origin-Secret check
  breakglass.go               # sha256 + constant-time compare
  jwt.go                      # lestrrat-go/jwx v3: JWKS cache (≤1 h), RS256, iss/exp, token_use, client_id, scope
  scopes.go                   # RequireScope → tool error
  principal.go                # Principal{Subject, ClientID, Scopes, Method}
internal/httpapi/
  server.go                   # net/http mux: /healthz, /.well-known/*, /mcp (+ trailing slash), middleware chain
  metadata.go                 # protected-resource + authorization-server documents (direct | fronted)
  middleware.go               # origin → bearer → principal in context; 401 with WWW-Authenticate
internal/mcpserver/
  server.go                   # go-sdk v1.7: mcp.NewServer, AddTool(hello), NewStreamableHTTPHandler{Stateless, JSONResponse}
  hello.go                    # hello tool: RequireScope(msg/read), returns message/stage/caller/auth_method
internal/lambdaadapter/       # ~100 lines: LambdaFunctionURLRequest ⇄ http.Handler (BUFFERED mode, base64 bodies, multi-value headers)
scripts/check_coverage.sh     # go tool cover -func thresholds: total ≥ 90 %, internal/auth (+ guardrails later) 100 %
```

Decisions baked in:

- **Buffered Function URL stays** (R3 was verified buffered; `lambdaurl` only supports `RESPONSE_STREAM`). A tiny in-house adapter avoids a third-party proxy dependency; it is unit-tested against recorded Function-URL event shapes.
- **No Lambda Web Adapter, no layer**: single static `bootstrap` binary (`CGO_ENABLED=0 GOOS=linux GOARCH=arm64`), runtime `provided.al2023`, handler `bootstrap`, `MemorySize` 256 (measure; raise if init > 150 ms).
- **JWT**: `github.com/lestrrat-go/jwx/v3` (`jwk.Cache` auto-refresh, `jwt.Parse` with `WithKeySet`, custom claim checks). Tests inject a local RSA JWKS via an `httptest.Server` or a key-set provider interface.
- **Contract tests (M2+)** move from botocore to reflection over AWS SDK Go v2 input structs (`sesv2.SendEmailInput` etc.) — field names are the API's PascalCase, so PRD G5 holds.
- **Python stays only as a CI tool runner**: `uvx cfn-lint`, `uvx checkov`, `uvx pre-commit` — no `pyproject.toml`, no `uv.lock`. `scripts/rotate-secret.py` and `bootstrap-user.sh` are replaced by a Go `cmd/ops` CLI (`ops rotate-secret`, `ops bootstrap-user`) so the repo has one toolchain.

## Implementation steps

### Step 0 — local toolchain (owner machine)

`brew install go golangci-lint` (Go 1.25+), `brew upgrade awscli`. No AWS or GitHub changes.

### Step 1 — PR-G1 `feat: port the server to go` (replaces Python in one PR)

- `go.mod` (module `github.com/gesparza8138/aws-messaging-mcp`), deps: `github.com/modelcontextprotocol/go-sdk`, `github.com/aws/aws-lambda-go`, `github.com/aws/aws-sdk-go-v2` (+`service/ssm`), `github.com/lestrrat-go/jwx/v3`.
- Port every module listed above with the same behaviour; port the test matrix:
  - unit: settings (env parsing, defaults, SSM resolver with a fake), auth (PRD §11.1 matrix: bad sig, expired, wrong iss, missing sub, id-token, unknown client, empty scope, resolver failure), origin, break-glass, scopes, lambda adapter (headers/base64/query/multi-value), metadata documents (byte-for-byte, both modes).
  - integration (`httptest.Server` + go-sdk client): OAuth round-trip `initialize → tools/list → tools/call hello` at `/mcp/` **and** `/mcp`, break-glass round-trip, missing-scope → tool error (not 401), 401 contract, origin-secret 403, exempt paths, no redirects anywhere (`/healthz/` → 404).
- Delete `src/`, `tests/`, `pyproject.toml`, `uv.lock`, `.pre-commit-config.yaml` Python hooks, `scripts/check_coverage.py`, `scripts/lambda-run.sh`; update `.gitignore`, `Makefile` (`dev`, `test`, `lint`, `typecheck`→`vet`, `build`), `.pre-commit-config.yaml` (gofmt/goimports/golangci-lint local hooks + basics).
- `ci.yml` keeps the six job **names**: `quality` = `gofmt -l`, `go vet`, `golangci-lint`, `uvx pre-commit`; `unit-tests` = `go test -race -coverprofile ./...` + `scripts/check_coverage.sh`; `security-scans` = `gosec`, `govulncheck`, gitleaks, dependency-review; `iac-scans` unchanged (uvx tools); `commitlint` unchanged. `docs.yml` unchanged. CodeQL default setup switched to `go` via `scripts/setup-github.sh` (repo API call).
- `.gitleaks.toml`, `CODEOWNERS` path (`/internal/auth/`) updated.

### Step 2 — PR-G2 `infra: go runtime packaging` (dev deploy, then prod release)

- `infra/app.yaml`: `Runtime: provided.al2023`, `Handler: bootstrap`, remove `Layers`, `AWS_LAMBDA_EXEC_WRAPPER`, `PORT`; `MemorySize` 256; drop `LwaLayerArn` parameter. Nothing else in the stack changes (env names identical).
- `deploy-dev.yml` / `release.yml`: `actions/setup-go` (pinned SHA) + `go build -trimpath -ldflags="-s -w"` → `zip app.zip bootstrap`; artifact key stays content-hashed. `uv` steps removed from these two workflows.
- `docs/setup/deploy.md` build section updated.
- Deploy dev via the workflow; verify; release `v0.1.3` → owner approval → prod.

### Step 3 — PR-G3 `docs: go toolchain`

`CLAUDE.md` (toolchain, layout, hard rules: "Tool schemas mirror the AWS API — `tests/contract` enforces this against the AWS SDK Go v2 input types"), `docs/PRD.md` (§10.1 jobs table, §11 test tiers, Appendix A layout, decision-log row with the measured numbers), `README.md`, `CONTRIBUTING.md`, `docs/architecture.md`, `docs/testing.md` (new, short). Then revise `docs/plans/m2.md` for Go and stop for M2 approval.

## Verification

1. `make test` locally: all Go tests green; `scripts/check_coverage.sh` reports total ≥ 90 % and `internal/auth` 100 %.
2. `make dev` (`go run ./cmd/server --listen :8000`) and the integration suite against it — same contracts the Python version passed.
3. After the dev deploy: live check (Go test with `-tags live`, `BASE_URL` + `BREAK_GLASS_TOKEN` env): healthz 200, 401 + remapped `resource_metadata` pointer, both well-known documents, garbage token 401, `tools/list` = `[hello]`, `tools/call hello` via break-glass → `auth_method: break_glass`; then Claude Code `hello` call (`auth_method: oauth`) and a Desktop call through the hosted bridge.
4. **Cold-start measurement** from CloudWatch `REPORT` lines: target `Init Duration` < 150 ms (record in the decision log next to the Python 2.9–3.0 s baseline).
5. Release `v0.1.3`: smoke test green (edge 403 + origin-path contracts); prod `hello` from Claude Code.
6. Rollback path: tag `v0.1.2` still builds the Python artifact through the old workflow definition at that tag; `infra/app.yaml` at that tag restores the LWA runtime config.

## Out of scope

M2 tools, guardrails, DynamoDB, SES — resume from `docs/plans/m2.md` after PR-G3.
