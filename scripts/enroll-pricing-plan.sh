#!/usr/bin/env bash
# Enrol a CloudFront distribution in a flat-rate pricing plan (PRD R8).
# CloudFormation cannot express plans (verified 2026-08-22), but the
# pricing-plan-manager API can. The Free plan covers ONE distribution;
# decide which stage gets it (PRD decision gate G12) before running.
#
# Usage:
#   ./scripts/enroll-pricing-plan.sh --distribution-id EXXXX [--plan FREE]
set -euo pipefail

PLAN="FREE" DIST_ID=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --distribution-id) DIST_ID="$2"; shift 2 ;;
    --plan) PLAN="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done
[[ -n "$DIST_ID" ]] || { echo "usage: $0 --distribution-id EXXXX [--plan FREE]" >&2; exit 1; }

ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
DIST_ARN="arn:aws:cloudfront::${ACCOUNT_ID}:distribution/${DIST_ID}"

echo "Existing subscriptions:"
aws pricing-plan-manager list-subscriptions --output table || true

SUB_ARN=$(aws pricing-plan-manager create-subscription \
  --pricing-plan "$PLAN" \
  --query 'Subscription.SubscriptionArn' --output text)
echo "Created subscription: ${SUB_ARN}"

aws pricing-plan-manager associate-resources-to-subscription \
  --subscription-arn "$SUB_ARN" \
  --resource-arns "$DIST_ARN"
echo "Associated ${DIST_ID}. Verify plan status and WAF-included billing in the console."
