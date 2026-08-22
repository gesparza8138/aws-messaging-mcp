# CLAUDE.md — aws-messaging-mcp

Project conventions for Claude Code. Read `docs/PRD.md` before planning anything; it is the source of truth for scope, architecture, auth, guardrails, and milestones.

## What this is

A stateless MCP server (Streamable HTTP) on AWS Lambda that exposes tools for Amazon SES email, AWS End User Messaging SMS/MMS/RCS, and CloudFront-signed file sharing. Single owner (Gabe), clients are Claude Code and Claude Desktop/Routines. Account `585008049602`, region `us-west-2`, hostnames `mcp.gabriel-esparza.com` (prod) and `dev.mcp.gabriel-esparza.com` (dev).

## Hard rules

- **Ask before any AWS deploy or any action that creates billable resources** (`aws cloudformation deploy`, `make deploy-dev`, requesting phone numbers, registrations, domain changes). Show the change set or the command first and wait for an explicit yes. This applies to `dev` as well as `prod`.
- **Never deploy to `prod` from a local machine.** Prod is reached only via the GitHub release workflow after manual approval.
- **No secrets in the repo, ever.** Not in code, templates, params files, tests, docs, or commit messages. Secrets live in SSM Parameter Store (AWS) or GitHub environment secrets. If you need a value for a test, generate it in the test.
- **Pure CloudFormation.** No SAM, no CDK, no Terraform. Templates live in `infra/`, are YAML, and must pass `cfn-lint`, `checkov`, and `cfn_nag`.
- **Tool schemas mirror the AWS API.** Property names and nesting are the PascalCase shapes of the underlying `sesv2` / `pinpoint-sms-voice-v2` / `s3` request (what `--cli-input-json` accepts). Server-controlled fields are injected or validated, not exposed. `tests/contract` enforces this against the live botocore model.
- **Every send tool supports `DryRun`** and runs the guardrails in `src/aws_messaging_mcp/guardrails/` before calling boto3.
- **Files bucket and media bucket stay separate.** Only the files bucket is behind CloudFront.
- **Docs are GitHub-flavored Markdown** in `docs/`; keep Mermaid, `[!NOTE]` admonitions, and relative links working. `markdownlint-cli2` and `lychee` run in CI.

## Toolchain

- Python 3.13, `uv` for envs and locking (`uv sync`, `uv run`), `ruff` (lint + format), `mypy --strict`, `pytest` + `pytest-cov` (≥ 90 %, 100 % for `auth/` and `guardrails/`), `moto` / `botocore.stub` for AWS mocks.
- Lambda: arm64, Lambda Web Adapter, FastAPI + FastMCP (`mcp` Python SDK), `aws-lambda-powertools` for logging/metrics.
- IaC checks: `cfn-lint`, `checkov -d infra/`, `cfn_nag_scan`.
- Security: `bandit`, `pip-audit`, `gitleaks`, CodeQL.
- Git: Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, `ci:`, `infra:`, `test:`), signed commits, squash-merge PRs only, never push directly to `main`.

## Make targets

`make dev` (local STDIO + HTTP server), `make test`, `make lint`, `make typecheck`, `make iac-lint`, `make e2e` (needs dev stack + env vars), `make deploy-dev` (asks for confirmation).

## Layout

See `docs/PRD.md` Appendix A. Key dirs: `src/aws_messaging_mcp/{auth,guardrails,tools,schemas,tasks}`, `infra/`, `tests/{unit,integration,e2e,contract}`, `docs/`, `scripts/`.

## Working style

- Use plan mode for anything touching `infra/` or `auth/`; present the plan, get approval, then implement.
- Prefer small PRs per milestone task; each PR updates docs and tests alongside code.
- When an AWS API shape is uncertain (especially `SendRcsMessage`), inspect the installed botocore model (`uv run python -c "import boto3; print(boto3.client('pinpoint-sms-voice-v2').meta.service_model.operation_model('SendRcsMessage').input_shape.members.keys())"`) rather than guessing from docs.
- Record decisions in `docs/PRD.md` Appendix C (decision log) with the date.
- If something in the PRD turns out to be wrong or impossible, stop and say so; don't silently work around it.

## Milestones

M0 scaffold + CI → M1 auth spike → M2 email → M3 SMS/MMS → M4 RCS → M4b files → M5 hardening. Details and exit criteria in `docs/PRD.md` §16. Manual prerequisites (registrations, DNS, Cognito user) are in `docs/setup/`.
