# Contributing

Single-owner project; these conventions exist so CI can enforce them and so
future contributors have one page to read. `docs/PRD.md` is the source of
truth for scope and architecture.

## Ground rules

- **Conventional Commits**, enforced by commitlint in CI: `feat:`, `fix:`,
  `docs:`, `chore:`, `ci:`, `infra:`, `test:`. The PR title becomes the squash
  commit on `main`, so the title must comply too.
- **Signed commits** are required by the `protect-main` ruleset.
- **Squash-only merges**; `main` requires a PR with six green checks
  (`quality`, `unit-tests`, `security-scans`, `iac-scans`, `docs`,
  `commitlint`). Never push directly to `main`.
- **No secrets in the repo**, ever — and because this repository is public,
  that extends to the AWS account ID and personal phone numbers (they live in
  GitHub secrets, which are masked in Actions logs; variables are not).
  Tests generate the values they need.
- Every PR updates tests and docs alongside code.

## Toolchain

Go (see `go.mod`) plus `golangci-lint`; `uv` only runs the IaC scanners and pre-commit:

```bash
uvx pre-commit install      # optional: run hooks on every commit
make test                   # go test -race with coverage gates (>= 90 %, auth 100 %)
make lint                   # gofmt + golangci-lint
make vet                    # go vet
make iac-lint               # cfn-lint + checkov + cfn_nag over infra/
```

E2E tests (`make e2e`, from M2) need a deployed dev stack and environment
variables; see `docs/testing.md` when it lands.

## Infrastructure changes

Pure CloudFormation YAML in `infra/` — no SAM, no CDK. Templates must pass
`make iac-lint`. Deploys happen through GitHub Actions only (the bootstrap
stack is the documented one-time exception).
