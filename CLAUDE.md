# CLAUDE.md — aws-messaging-mcp

Project conventions for Claude Code. Read `docs/PRD.md` before planning anything; it is the source of truth for scope, architecture, auth, guardrails, and milestones.

## What this is

A stateless MCP server (Streamable HTTP) on AWS Lambda that exposes tools for Amazon SES email, AWS End User Messaging SMS/MMS (RCS descoped - PRD Appendix C), and CloudFront-signed file sharing. Single owner (Gabe), clients are Claude Code and Claude Desktop/Routines. Single AWS account (ID not recorded in this public repo — `aws sts get-caller-identity` locally, GitHub secret `AWS_ACCOUNT_ID` in CI), region `us-west-2`, hostnames `mcp.gabriel-esparza.com` (prod) and `dev.mcp.gabriel-esparza.com` (dev).

## Hard rules

- **Ask before any AWS deploy or any action that creates billable resources** (`aws cloudformation deploy`, `make deploy-dev`, requesting phone numbers, registrations, domain changes). Show the change set or the command first and wait for an explicit yes. This applies to `dev` as well as `prod`.
- **Never deploy to `prod` from a local machine.** Prod is reached only via the GitHub release workflow after manual approval.
- **No secrets in the repo, ever.** Not in code, templates, params files, tests, docs, or commit messages. Secrets live in SSM Parameter Store (AWS) or GitHub environment secrets. If you need a value for a test, generate it in the test.
- **Pure CloudFormation.** No SAM, no CDK, no Terraform. Templates live in `infra/`, are YAML, and must pass `cfn-lint`, `checkov`, and `cfn_nag`.
- **Tool schemas mirror the AWS API.** Property names and nesting are the PascalCase shapes of the underlying `sesv2` / `pinpoint-sms-voice-v2` / `s3` request (what `--cli-input-json` accepts). Server-controlled fields are injected or validated, not exposed. Contract tests enforce this by reflection over the AWS SDK for Go v2 input types (`sesv2.SendEmailInput` etc.).
- **Every send tool supports `DryRun`** and runs the guardrails in `src/aws_messaging_mcp/guardrails/` before calling boto3.
- **Files bucket and media bucket stay separate.** Only the files bucket is behind CloudFront.
- **Docs are GitHub-flavored Markdown** in `docs/`; keep Mermaid, `[!NOTE]` admonitions, and relative links working. `markdownlint-cli2` and `lychee` run in CI.

## Toolchain

- **Go** (version in `go.mod`), `gofmt`, `go vet`, `golangci-lint` (`.golangci.yml`), `go test -race` with coverage floors enforced by `scripts/check_coverage.sh` (≥ 90 % over `internal/`, 100 % for `internal/auth/` and `internal/guardrails/`). AWS calls go through interfaces with fakes in tests; contract tests reflect over AWS SDK for Go v2 input structs.
- Lambda: `provided.al2023` on arm64, one static `bootstrap` binary (`cmd/server`), no layers; Function URL events are adapted in `internal/lambdaadapter` (buffered mode). Measured cold start 217 ms (the Python predecessor was ~2.9 s - see the decision log).
- MCP: official `github.com/modelcontextprotocol/go-sdk`, stateless Streamable HTTP with JSON responses.
- Python exists only as a CI tool runner (`uvx cfn-lint`, `uvx checkov`, `uvx pre-commit`); there is no `pyproject.toml`.
- IaC checks: `cfn-lint`, `checkov -d infra/`, `cfn_nag_scan`.
- Security: `gosec`, `govulncheck`, `gitleaks`, CodeQL (Go).
- Owner operations: `go run ./cmd/ops rotate-secret|bootstrap-user`.
- Git: Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `ci:`, `infra:`, `test:`), signed commits, squash-merge PRs only, never push directly to `main`.

## Make targets

`make dev` (local HTTP server on :8000), `make build` (arm64 `dist/bootstrap`), `make test`, `make lint`, `make vet`, `make iac-lint`, `make e2e` (M2+, needs dev stack + env vars). Deploys run only through GitHub Actions (`deploy-dev.yml` preview then `execute=true`; prod via a `v*` tag and the approval gate).

## Layout

See `docs/PRD.md` Appendix A. Key dirs: `cmd/{server,ops}`, `internal/{settings,auth,httpapi,mcpserver,lambdaadapter,testkeys}` (M2+ adds `guardrails`, `schemas`, `tools`), `infra/`, `docs/`, `scripts/`. Tests live beside the code (`*_test.go`); integration tests use `httptest` plus the go-sdk client.

## Working style

- Use plan mode for anything touching `infra/` or `auth/`; present the plan, get approval, then implement.
- Prefer small PRs per milestone task; each PR updates docs and tests alongside code.
- When an AWS API shape is uncertain, inspect the SDK's input struct (`go doc github.com/aws/aws-sdk-go-v2/service/pinpointsmsvoicev2.SendTextMessageInput`) rather than guessing from docs.
- Record decisions in `docs/PRD.md` Appendix C (decision log) with the date.
- If something in the PRD turns out to be wrong or impossible, stop and say so; don't silently work around it.

## Milestones

M0 scaffold + CI → M1 auth spike → M2 email → M3 SMS/MMS → M4b files → M5 hardening (M4 RCS removed). Details and exit criteria in `docs/PRD.md` §16. Manual prerequisites (registrations, DNS, Cognito user) are in `docs/setup/`.
