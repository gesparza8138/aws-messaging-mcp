# GitHub repository setup

Step-by-step instructions for creating and configuring the `aws-messaging-mcp` repository so that the CI/CD pipeline in the [PRD §10](../PRD.md#10-cicd-pipeline) is enforced from day one. Everything below is done with the [GitHub CLI](https://cli.github.com/) (`gh` ≥ 2.60); the few things the CLI has no subcommand for use `gh api` against the REST API. A runnable version of this page lives in [`scripts/setup-github.sh`](../../scripts/setup-github.sh).

> [!IMPORTANT]
> Run these once, in order, **before** the first push of application code. Several settings (rulesets, environments) are what make "no deploy to prod without approval" true.

## 0. Prerequisites

```bash
gh --version            # ≥ 2.60
gh auth login           # choose GitHub.com, HTTPS, authenticate via browser
gh auth status
gh auth refresh -s repo,workflow,admin:org,admin:repo_hook,read:org   # scopes used below
```

Set the variables the rest of this page uses:

```bash
export GH_OWNER="gesparza8138"
export GH_REPO="aws-messaging-mcp"
export AWS_ACCOUNT_ID="<your-aws-account-id>"   # never committed; stored as a GitHub secret
export AWS_REGION="us-west-2"
```

## 1. Create the repository

```bash
gh repo create "$GH_OWNER/$GH_REPO" \
  --private \
  --description "Serverless MCP server for SES email, SMS/MMS/RCS, and CloudFront-signed file sharing" \
  --gitignore Python \
  --license MIT \
  --clone
cd "$GH_REPO"
git switch -c main 2>/dev/null || git switch main
```

Repository-level settings:

```bash
# Merge strategy: squash only, keep history linear, auto-delete merged branches
gh repo edit "$GH_OWNER/$GH_REPO" \
  --enable-squash-merge --enable-merge-commit=false --enable-rebase-merge=false \
  --delete-branch-on-merge \
  --enable-issues --enable-projects=false --enable-wiki=false \
  --default-branch main

# Squash-merge commit title = PR title (keeps Conventional Commits on main)
gh api -X PATCH "repos/$GH_OWNER/$GH_REPO" \
  -f squash_merge_commit_title=PR_TITLE \
  -f squash_merge_commit_message=PR_BODY \
  -F allow_update_branch=true
```

## 2. Security and analysis features

```bash
# Dependabot alerts + automated security fixes
gh api -X PUT "repos/$GH_OWNER/$GH_REPO/vulnerability-alerts"
gh api -X PUT "repos/$GH_OWNER/$GH_REPO/automated-security-fixes"

# Secret scanning + push protection (blocks commits that contain credentials)
gh api -X PATCH "repos/$GH_OWNER/$GH_REPO" --input - <<'JSON'
{
  "security_and_analysis": {
    "secret_scanning": { "status": "enabled" },
    "secret_scanning_push_protection": { "status": "enabled" },
    "dependency_graph": { "status": "enabled" }
  }
}
JSON

# CodeQL default setup for Python (runs on PRs and pushes to main)
gh api -X PATCH "repos/$GH_OWNER/$GH_REPO/code-scanning/default-setup" \
  -f state=configured -f query_suite=extended -f 'languages[]=python'

# Private vulnerability reporting
gh api -X PUT "repos/$GH_OWNER/$GH_REPO/private-vulnerability-reporting"
```

> [!NOTE]
> On a **private** repo under a personal account, secret scanning, push protection, and CodeQL default setup require GitHub Advanced Security, which is free for public repos but may be unavailable or paid for private ones. If the calls above return `403`/`422`, either make the repo public or rely on the in-workflow equivalents (`gitleaks` and the CodeQL action) that `ci.yml` already runs. Dependabot alerts and the dependency graph work on every plan.

Dependabot version updates are configured by the checked-in file `.github/dependabot.yml`:

```yaml
version: 2
updates:
  - package-ecosystem: "pip"
    directory: "/"
    schedule: { interval: "weekly", day: "monday" }
    groups:
      minor-and-patch: { update-types: ["minor", "patch"] }
    labels: ["dependencies"]
  - package-ecosystem: "github-actions"
    directory: "/"
    schedule: { interval: "weekly", day: "monday" }
    labels: ["dependencies", "ci"]
```

## 3. Environments (`dev` and `prod`)

Environments carry the OIDC role ARNs as **variables** and the approval gate for `prod`. Required reviewers must be set by user ID; look yours up first.

```bash
MY_ID=$(gh api user --jq .id)

# dev: auto-deploys from main, no approval
gh api -X PUT "repos/$GH_OWNER/$GH_REPO/environments/dev" --input - <<JSON
{
  "wait_timer": 0,
  "prevent_self_review": false,
  "reviewers": [],
  "deployment_branch_policy": { "protected_branches": true, "custom_branch_policies": false }
}
JSON

# prod: requires the owner's approval, 5-minute wait timer, only from release tags
gh api -X PUT "repos/$GH_OWNER/$GH_REPO/environments/prod" --input - <<JSON
{
  "wait_timer": 5,
  "prevent_self_review": false,
  "reviewers": [ { "type": "User", "id": $MY_ID } ],
  "deployment_branch_policy": { "protected_branches": false, "custom_branch_policies": true }
}
JSON
# Allow prod deployments only from tags matching v*
gh api -X POST "repos/$GH_OWNER/$GH_REPO/environments/prod/deployment-branch-policies" \
  -f name='v*' -f type=tag
```

> [!NOTE]
> `prevent_self_review: false` is deliberate — as the sole maintainer you must be able to approve your own prod deploys. Flip it to `true` if a second maintainer joins.

Environment variables (non-secret) and secrets:

> [!IMPORTANT]
> This is a **public** repository, and Actions logs on public repos are public. GitHub masks
> **secrets** in logs but does **not** mask variables — so every value that embeds the AWS
> account ID (role ARNs, artifact bucket) and the owner's phone number is stored as a secret,
> not a variable, even though none of them is a credential in the classic sense.

```bash
# Role ARNs are outputs of infra/bootstrap.yaml. Secrets, not variables:
# they embed the AWS account ID, which is kept out of the public repo and logs.
gh secret set AWS_ACCOUNT_ID       --body "$AWS_ACCOUNT_ID"
gh secret set ARTIFACT_BUCKET      --body "aws-messaging-mcp-artifacts-$AWS_ACCOUNT_ID"
gh secret set AWS_DEPLOY_ROLE_ARN --env dev  --body "arn:aws:iam::$AWS_ACCOUNT_ID:role/aws-messaging-mcp-deploy-dev"
gh secret set AWS_DEPLOY_ROLE_ARN --env prod --body "arn:aws:iam::$AWS_ACCOUNT_ID:role/aws-messaging-mcp-deploy-prod"
gh variable set AWS_REGION --body "$AWS_REGION"

# E2E test inputs (dev only). Email e2e needs nothing here: URLs and client
# ids come from stack outputs, the client secret from SSM. Only the owner's
# phone number (M3, kept out of the repo) is a GitHub secret.
gh secret set E2E_TEST_PHONE          --env dev   # prompts; owner's mobile in E.164
```

## 4. Rulesets (branch and tag protection)

Rulesets are the current replacement for classic branch protection and are fully configurable via the API.

### 4.1 `main`

```bash
gh api -X POST "repos/$GH_OWNER/$GH_REPO/rulesets" --input - <<'JSON'
{
  "name": "protect-main",
  "target": "branch",
  "enforcement": "active",
  "conditions": { "ref_name": { "include": ["~DEFAULT_BRANCH"], "exclude": [] } },
  "rules": [
    { "type": "deletion" },
    { "type": "non_fast_forward" },
    { "type": "required_linear_history" },
    { "type": "required_signatures" },
    {
      "type": "pull_request",
      "parameters": {
        "required_approving_review_count": 0,
        "dismiss_stale_reviews_on_push": true,
        "require_code_owner_review": false,
        "require_last_push_approval": false,
        "required_review_thread_resolution": true,
        "allowed_merge_methods": ["squash"]
      }
    },
    {
      "type": "required_status_checks",
      "parameters": {
        "strict_required_status_checks_policy": true,
        "do_not_enforce_on_create": false,
        "required_status_checks": [
          { "context": "quality" },
          { "context": "unit-tests" },
          { "context": "security-scans" },
          { "context": "iac-scans" },
          { "context": "docs" },
          { "context": "commitlint" }
        ]
      }
    }
  ],
  "bypass_actors": []
}
JSON
```

Notes:

- `required_approving_review_count` is `0` because you are the only maintainer; a PR is still **required**, so every change goes through CI. Raise to `1` and set `require_code_owner_review: true` when a second person joins.
- The `context` names must match the `name:` of the jobs in `.github/workflows/ci.yml` and `docs.yml` exactly. Until those workflows have run at least once, GitHub shows the checks as "expected" and blocks merging — that is intended.
- `required_signatures` requires you to sign commits. Set it up once: `gh ssh-key add ~/.ssh/id_ed25519.pub --type signing` then `git config --global gpg.format ssh && git config --global user.signingkey ~/.ssh/id_ed25519.pub && git config --global commit.gpgsign true`. Drop the rule if you'd rather not.

### 4.2 Release tags

```bash
gh api -X POST "repos/$GH_OWNER/$GH_REPO/rulesets" --input - <<'JSON'
{
  "name": "protect-release-tags",
  "target": "tag",
  "enforcement": "active",
  "conditions": { "ref_name": { "include": ["refs/tags/v*"], "exclude": [] } },
  "rules": [
    { "type": "deletion" },
    { "type": "non_fast_forward" },
    { "type": "update" },
    { "type": "required_signatures" }
  ],
  "bypass_actors": [ { "actor_id": 5, "actor_type": "RepositoryRole", "bypass_mode": "always" } ]
}
JSON
```

`actor_id: 5` is the built-in **Admin** repository role, so you can delete a bad tag if needed. Releases are cut with `gh release create vX.Y.Z --generate-notes` (or by the `release.yml` workflow on tag push).

## 5. Actions settings

```bash
# Only allow actions from GitHub, verified creators, and this repo; pin everything else by SHA in workflows
gh api -X PUT "repos/$GH_OWNER/$GH_REPO/actions/permissions" \
  -F enabled=true -f allowed_actions=selected
gh api -X PUT "repos/$GH_OWNER/$GH_REPO/actions/permissions/selected-actions" --input - <<'JSON'
{
  "github_owned_allowed": true,
  "verified_allowed": true,
  "patterns_allowed": ["aws-actions/*", "astral-sh/setup-uv@*", "gitleaks/gitleaks-action@*", "bridgecrewio/checkov-action@*"]
}
JSON

# Default GITHUB_TOKEN is read-only; workflows request write scopes explicitly
gh api -X PUT "repos/$GH_OWNER/$GH_REPO/actions/permissions/workflow" \
  -f default_workflow_permissions=read -F can_approve_pull_request_reviews=false

# Require approval for workflows from first-time outside contributors (if the repo ever goes public)
gh api -X PUT "repos/$GH_OWNER/$GH_REPO/actions/permissions/fork-pr-contributor-approval" \
  -f approval_policy=all_external_contributors 2>/dev/null || true
```

## 6. OIDC trust between GitHub and AWS

The AWS side (`AWS::IAM::OIDCProvider` and two roles) is created by `infra/bootstrap.yaml`. What must line up is the **`sub` claim condition** in each role's trust policy; the values GitHub sends are:

| Role | Trust condition (`token.actions.githubusercontent.com:sub`) | Used by |
| --- | --- | --- |
| `aws-messaging-mcp-deploy-dev` | `repo:<owner>@<owner-id>/aws-messaging-mcp@<repo-id>:environment:dev` | `deploy-dev.yml`, `e2e-tests` |
| `aws-messaging-mcp-deploy-prod` | `repo:<owner>@<owner-id>/aws-messaging-mcp@<repo-id>:environment:prod` | `release.yml` (after approval) |
| *(neither)* | `repo:...:pull_request` | PR jobs **do not** assume any AWS role |

> [!NOTE]
> GitHub embeds the immutable numeric owner and repository IDs in the `sub` claim
> (`owner@id/repo@id`), which protects the trust policy against account/repo name
> reuse. Confirm the IDs with `gh api repos/<owner>/<repo> --jq '{owner: .owner.id, repo: .id}'`
> — they are the `GitHubOwnerId` / `GitHubRepoId` parameters of `infra/bootstrap.yaml`.
> Verify the exact claim your tokens carry via a failed-assume event in CloudTrail
> (`userIdentity.userName`) if in doubt.

Both roles also require `token.actions.githubusercontent.com:aud = sts.amazonaws.com`. Because the condition is on the *environment*, the only way to obtain the prod role is a workflow run that passed the `prod` environment's approval gate — the GitHub configuration in §3 is therefore part of the AWS security boundary.

Deploy the bootstrap stack once from your workstation (this is the only time human AWS credentials are used for deployment):

```bash
aws cloudformation deploy \
  --stack-name aws-messaging-mcp-bootstrap \
  --template-file infra/bootstrap.yaml \
  --capabilities CAPABILITY_NAMED_IAM \
  --parameter-overrides GitHubOwner="$GH_OWNER" GitHubRepo="$GH_REPO"
aws cloudformation describe-stacks --stack-name aws-messaging-mcp-bootstrap \
  --query 'Stacks[0].Outputs' --output table   # copy role ARNs into the gh variable commands in §3
```

In workflows, the role is assumed with:

```yaml
permissions:
  id-token: write   # required for OIDC
  contents: read
steps:
  - uses: aws-actions/configure-aws-credentials@<pinned-sha>
    with:
      role-to-assume: ${{ vars.AWS_DEPLOY_ROLE_ARN }}
      aws-region: ${{ vars.AWS_REGION }}
      role-session-name: gha-${{ github.run_id }}
```

## 7. Repository files GitHub reads

| File | Purpose |
| --- | --- |
| `.github/CODEOWNERS` | `/infra/ @<owner>` and `/src/aws_messaging_mcp/auth/ @<owner>` — review routing, and enforced if `require_code_owner_review` is turned on |
| `.github/dependabot.yml` | See §2 |
| `.github/pull_request_template.md` | Checklist: tests added, docs updated, `DryRun` verified, no secrets |
| `.github/ISSUE_TEMPLATE/{bug,feature}.yml` | Structured issue forms |
| `SECURITY.md` | How to report a vulnerability (private reporting is enabled in §2) |
| `CONTRIBUTING.md` | Conventional Commits, `make` targets, how to run E2E |
| `.gitleaks.toml` | Allow-list for known false positives (e.g. example key IDs in docs) |
| `.markdownlint-cli2.yaml` | `MD013: false` (tables), otherwise defaults |
| `CLAUDE.md` | Project conventions for Claude Code |

## 8. Labels

```bash
for l in "dependencies:0366d6" "ci:fbca04" "security:d73a4a" "docs:0075ca" "tool:email:c2e0c6" "tool:sms:c2e0c6" "tool:files:c2e0c6" "infra:bfd4f2" "good first issue:7057ff"; do
  gh label create "${l%%:*}" --color "${l##*:}" --force
done
```

## 9. Verify

```bash
gh repo view "$GH_OWNER/$GH_REPO" --json visibility,defaultBranchRef,squashMergeAllowed,deleteBranchOnMerge
gh api "repos/$GH_OWNER/$GH_REPO/rulesets" --jq '.[] | {name, enforcement}'
gh api "repos/$GH_OWNER/$GH_REPO/environments" --jq '.environments[] | {name, protection_rules: [.protection_rules[].type]}'
gh variable list --env dev
gh variable list --env prod
gh secret list --env dev
gh api "repos/$GH_OWNER/$GH_REPO/actions/permissions"
```

Expected: two active rulesets; `prod` shows `required_reviewers` and `wait_timer`; `dev` has the `AWS_DEPLOY_ROLE_ARN` and `E2E_TEST_PHONE` secrets; Actions permissions are `selected`.

## 10. First push

```bash
git add -A
git commit -S -m "chore: scaffold repository"
git push -u origin main
```

> [!WARNING]
> The `protect-main` ruleset blocks direct pushes to `main` immediately, including the very first one. Either push the scaffold **before** running §4, or push to a branch and open the first PR (`gh pr create --fill`). The `scripts/setup-github.sh` script orders this correctly: it pushes the scaffold, then applies rulesets.

## Reference: what each control buys you

| Control | Requirement it satisfies |
| --- | --- |
| PR required + required status checks on `main` | PRD G6 — nothing reaches `dev` without passing every gate |
| `prod` environment with required reviewer + tag-only deployments | PRD G6 — manual approval before production |
| OIDC trust scoped to environment | PRD S3 — no static AWS keys; prod role unreachable without the approval gate |
| Secret scanning + push protection + gitleaks | PRD S2 — no secrets in the repo |
| Dependabot + `pip-audit` + dependency review | PRD S7 — CVE gate |
| Selected actions + SHA pinning + read-only token | Supply-chain hardening of the pipeline itself |
| Linear history + squash + signed commits + Conventional Commits | Auditable history; automated changelog |
