# aws-messaging-mcp

[![ci](https://github.com/gesparza8138/aws-messaging-mcp/actions/workflows/ci.yml/badge.svg)](https://github.com/gesparza8138/aws-messaging-mcp/actions/workflows/ci.yml)
[![docs](https://github.com/gesparza8138/aws-messaging-mcp/actions/workflows/docs.yml/badge.svg)](https://github.com/gesparza8138/aws-messaging-mcp/actions/workflows/docs.yml)

A serverless [Model Context Protocol](https://modelcontextprotocol.io) server on AWS Lambda that
lets Claude Code, Claude Desktop, and claude.ai Routines send email (Amazon SES), SMS/MMS/RCS
(AWS End User Messaging), and share files via CloudFront-signed download links.

> [!NOTE]
> **Status: M0 (bootstrap).** The application arrives milestone by milestone; see the
> [PRD](docs/PRD.md) §16 and the current [implementation plan](docs/plans/m0-m1.md).

## Architecture (target)

```mermaid
flowchart LR
    C[Claude clients] -- Bearer JWT --> W[WAF + CloudFront]
    W --> L[Lambda: FastAPI + FastMCP]
    L -.JWKS.-> K[Cognito]
    L --> SES[SES v2]
    L --> EUM[End User Messaging]
    L --> S3[(S3 + signed URLs)]
```

Full detail: [docs/PRD.md](docs/PRD.md) §4.

## Documentation

| Doc | Purpose |
| --- | --- |
| [docs/PRD.md](docs/PRD.md) | Source of truth: scope, architecture, auth, guardrails, milestones |
| [docs/setup/github.md](docs/setup/github.md) | Repository and CI/CD configuration |
| [docs/setup/dns.md](docs/setup/dns.md) | GoDaddy → Route 53 subdomain delegation |
| [docs/setup/aws-prerequisites.md](docs/setup/aws-prerequisites.md) | SES, toll-free, RCS, Cognito prerequisites |
| [docs/plans/m0-m1.md](docs/plans/m0-m1.md) | Current implementation plan |

## Development

Requires Python 3.13 and [uv](https://docs.astral.sh/uv/).

```bash
uv sync            # create the venv and install locked dependencies
make test          # unit tests with coverage gate
make lint          # ruff check + format check
make typecheck     # mypy --strict
make iac-lint      # cfn-lint + checkov + cfn_nag over infra/
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for conventions (Conventional Commits, signed commits,
squash-only merges).

## License

[MIT](LICENSE)
