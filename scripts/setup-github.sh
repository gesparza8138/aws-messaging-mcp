#!/usr/bin/env bash
# One-shot GitHub configuration for aws-messaging-mcp.
# Mirrors docs/setup/github.md. Idempotent where the API allows it.
#
# Usage:
#   GH_OWNER=you AWS_ACCOUNT_ID=123456789012 ./scripts/setup-github.sh [--create]
#
#   --create   also create the repo and push the current directory as the scaffold
#              (run from the repo root). Without it, the script only configures an
#              existing repo.
set -euo pipefail

: "${GH_OWNER:?set GH_OWNER}"
: "${AWS_ACCOUNT_ID:?set AWS_ACCOUNT_ID}"
GH_REPO="${GH_REPO:-aws-messaging-mcp}"
AWS_REGION="${AWS_REGION:-us-west-2}"
FULL="$GH_OWNER/$GH_REPO"
CREATE=false; [[ "${1:-}" == "--create" ]] && CREATE=true

need() { command -v "$1" >/dev/null || { echo "missing: $1" >&2; exit 1; }; }
need gh; need jq
gh auth status >/dev/null

step() { printf '\n\033[1;34m==> %s\033[0m\n' "$*"; }
api() { gh api "$@" >/dev/null; }
try() { "$@" 2>/dev/null || echo "   (skipped: not available on this plan/visibility)"; }

if $CREATE; then
  step "Creating repository $FULL"
  # NOTE: gh rejects --license/--gitignore combined with --source; the scaffold
  # commit carries LICENSE and .gitignore as ordinary files instead.
  gh repo create "$FULL" --private \
    --description "Serverless MCP server for SES email, SMS/MMS/RCS, and CloudFront-signed file sharing" \
    --source . --remote origin --push
fi

step "Repository settings"
gh repo edit "$FULL" \
  --enable-squash-merge --enable-merge-commit=false --enable-rebase-merge=false \
  --delete-branch-on-merge --enable-issues --enable-projects=false --enable-wiki=false \
  --default-branch main
api -X PATCH "repos/$FULL" -f squash_merge_commit_title=PR_TITLE -f squash_merge_commit_message=PR_BODY -F allow_update_branch=true

step "Security & analysis"
api -X PUT "repos/$FULL/vulnerability-alerts"
api -X PUT "repos/$FULL/automated-security-fixes"
try api -X PATCH "repos/$FULL" --input - <<'JSON'
{"security_and_analysis":{"secret_scanning":{"status":"enabled"},"secret_scanning_push_protection":{"status":"enabled"},"dependency_graph":{"status":"enabled"}}}
JSON
try api -X PATCH "repos/$FULL/code-scanning/default-setup" -f state=configured -f query_suite=extended -f 'languages[]=python'
try api -X PUT "repos/$FULL/private-vulnerability-reporting"

step "Environments"
MY_ID=$(gh api user --jq .id)
api -X PUT "repos/$FULL/environments/dev" --input - <<'JSON'
{"wait_timer":0,"prevent_self_review":false,"reviewers":[],"deployment_branch_policy":{"protected_branches":true,"custom_branch_policies":false}}
JSON
api -X PUT "repos/$FULL/environments/prod" --input - <<JSON
{"wait_timer":5,"prevent_self_review":false,"reviewers":[{"type":"User","id":$MY_ID}],"deployment_branch_policy":{"protected_branches":false,"custom_branch_policies":true}}
JSON
if ! gh api "repos/$FULL/environments/prod/deployment-branch-policies" --jq '.branch_policies[].name' | grep -qx 'v\*'; then
  api -X POST "repos/$FULL/environments/prod/deployment-branch-policies" -f name='v*' -f type=tag
fi

step "Variables"
gh variable set AWS_REGION --repo "$FULL" --body "$AWS_REGION"
gh variable set ARTIFACT_BUCKET --repo "$FULL" --body "aws-messaging-mcp-artifacts-$AWS_ACCOUNT_ID"
gh variable set AWS_DEPLOY_ROLE_ARN --repo "$FULL" --env dev  --body "arn:aws:iam::$AWS_ACCOUNT_ID:role/aws-messaging-mcp-deploy-dev"
gh variable set AWS_DEPLOY_ROLE_ARN --repo "$FULL" --env prod --body "arn:aws:iam::$AWS_ACCOUNT_ID:role/aws-messaging-mcp-deploy-prod"
for v in E2E_MCP_URL E2E_COGNITO_CLIENT_ID E2E_TEST_EMAIL E2E_TEST_PHONE; do
  if [[ -n "${!v:-}" ]]; then gh variable set "$v" --repo "$FULL" --env dev --body "${!v}"; else echo "   $v not set in env; set later with: gh variable set $v --env dev"; fi
done
echo "   Secret E2E_TEST_USER_PASSWORD: set manually with  gh secret set E2E_TEST_USER_PASSWORD --repo $FULL --env dev"

step "Actions permissions"
api -X PUT "repos/$FULL/actions/permissions" -F enabled=true -f allowed_actions=selected
api -X PUT "repos/$FULL/actions/permissions/selected-actions" --input - <<'JSON'
{"github_owned_allowed":true,"verified_allowed":true,"patterns_allowed":["aws-actions/*","astral-sh/setup-uv@*","gitleaks/gitleaks-action@*","bridgecrewio/checkov-action@*"]}
JSON
api -X PUT "repos/$FULL/actions/permissions/workflow" -f default_workflow_permissions=read -F can_approve_pull_request_reviews=false

step "Labels"
for l in "dependencies:0366d6" "ci:fbca04" "security:d73a4a" "docs:0075ca" "tool:email:c2e0c6" "tool:sms:c2e0c6" "tool:rcs:c2e0c6" "tool:files:c2e0c6" "infra:bfd4f2"; do
  gh label create "${l%:*}" --repo "$FULL" --color "${l##*:}" --force >/dev/null
done

step "Rulesets (applied last so the scaffold push above is not blocked)"
existing=$(gh api "repos/$FULL/rulesets" --jq '.[].name')
if ! grep -qx protect-main <<<"$existing"; then
api -X POST "repos/$FULL/rulesets" --input - <<'JSON'
{"name":"protect-main","target":"branch","enforcement":"active",
 "conditions":{"ref_name":{"include":["~DEFAULT_BRANCH"],"exclude":[]}},
 "rules":[{"type":"deletion"},{"type":"non_fast_forward"},{"type":"required_linear_history"},{"type":"required_signatures"},
  {"type":"pull_request","parameters":{"required_approving_review_count":0,"dismiss_stale_reviews_on_push":true,"require_code_owner_review":false,"require_last_push_approval":false,"required_review_thread_resolution":true,"allowed_merge_methods":["squash"]}},
  {"type":"required_status_checks","parameters":{"strict_required_status_checks_policy":true,"do_not_enforce_on_create":false,
   "required_status_checks":[{"context":"quality"},{"context":"unit-tests"},{"context":"security-scans"},{"context":"iac-scans"},{"context":"docs"},{"context":"commitlint"}]}}],
 "bypass_actors":[]}
JSON
fi
if ! grep -qx protect-release-tags <<<"$existing"; then
api -X POST "repos/$FULL/rulesets" --input - <<'JSON'
{"name":"protect-release-tags","target":"tag","enforcement":"active",
 "conditions":{"ref_name":{"include":["refs/tags/v*"],"exclude":[]}},
 "rules":[{"type":"deletion"},{"type":"non_fast_forward"},{"type":"update"},{"type":"required_signatures"}],
 "bypass_actors":[{"actor_id":5,"actor_type":"RepositoryRole","bypass_mode":"always"}]}
JSON
fi

step "Verify"
gh api "repos/$FULL/rulesets" --jq '.[] | "  ruleset: \(.name) [\(.enforcement)]"'
gh api "repos/$FULL/environments" --jq '.environments[] | "  env: \(.name) rules=\([.protection_rules[].type] | join(","))"'
gh api "repos/$FULL/actions/permissions" --jq '"  actions: \(.allowed_actions)"'
echo
echo "Done. Next: deploy infra/bootstrap.yaml (see docs/setup/github.md §6) and confirm the role ARNs match the variables above."
