#!/usr/bin/env bash
# Create the single Cognito owner user for a stage (self-signup is disabled).
# The user gets a generated temporary password, must change it at first
# hosted-UI login, and must then enrol TOTP (MFA is required by the pool).
#
# Usage:
#   ./scripts/bootstrap-user.sh --stage dev --email you@example.com
set -euo pipefail

STAGE="" EMAIL="" REGION="us-west-2"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --stage) STAGE="$2"; shift 2 ;;
    --email) EMAIL="$2"; shift 2 ;;
    --region) REGION="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done
[[ -n "$STAGE" && -n "$EMAIL" ]] || { echo "usage: $0 --stage dev|prod --email addr" >&2; exit 1; }

POOL_ID=$(aws cloudformation describe-stacks --stack-name "aws-messaging-mcp-${STAGE}" \
  --region "$REGION" --query 'Stacks[0].Outputs[?OutputKey==`UserPoolId`].OutputValue' --output text)
HOSTED_UI=$(aws cloudformation describe-stacks --stack-name "aws-messaging-mcp-${STAGE}" \
  --region "$REGION" --query 'Stacks[0].Outputs[?OutputKey==`HostedUiUrl`].OutputValue' --output text)

TEMP_PASSWORD=$(python3 -c "import secrets,string; a=string.ascii_letters+string.digits; print('Aa1!'+''.join(secrets.choice(a) for _ in range(16)))")

aws cognito-idp admin-create-user \
  --region "$REGION" \
  --user-pool-id "$POOL_ID" \
  --username "$EMAIL" \
  --user-attributes "Name=email,Value=${EMAIL}" "Name=email_verified,Value=true" \
  --temporary-password "$TEMP_PASSWORD" \
  --message-action SUPPRESS >/dev/null

echo "Created ${EMAIL} in ${POOL_ID}."
echo "Temporary password (change at first login): ${TEMP_PASSWORD}"
echo "Sign in once at ${HOSTED_UI} via a client OAuth flow to set a real"
echo "password and enrol TOTP - MFA enrolment is forced by the pool."
