#!/usr/bin/env bash
# Refresh the owner-IP entry in the edge allow-list (PRD R7).
# The allow-list lives in SSM /messaging-mcp/edge/allowed-cidrs (read by the
# deploy workflows into the CloudFront Function) and is applied immediately by
# rewriting and republishing each stage's live function, so no stack deploy
# is needed for an IP change. Other entries (Anthropic egress, temporary CI
# entries) are preserved; the previous owner entry is tracked in
# /messaging-mcp/edge/home-ip.
#
# Usage: ./scripts/update-my-ip.sh [--ip 1.2.3.4] [--stages "dev prod"]
set -euo pipefail

REGION=us-west-2
NEW_IP="" STAGES="dev prod"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --ip) NEW_IP="$2"; shift 2 ;;
    --stages) STAGES="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 1 ;;
  esac
done
[[ -n "$NEW_IP" ]] || NEW_IP=$(curl -s https://checkip.amazonaws.com)
NEW_CIDR="${NEW_IP}/32"

CURRENT=$(aws ssm get-parameter --region "$REGION" --name /messaging-mcp/edge/allowed-cidrs --query Parameter.Value --output text)
OLD_HOME=$(aws ssm get-parameter --region "$REGION" --name /messaging-mcp/edge/home-ip --query Parameter.Value --output text 2>/dev/null || echo "")

NEW_LIST=()
IFS=',' read -ra ENTRIES <<<"$CURRENT"
for e in "${ENTRIES[@]}"; do
  e=$(echo "$e" | tr -d ' ')
  [[ -z "$e" || "$e" == "$OLD_HOME" || "$e" == "$NEW_CIDR" ]] && continue
  NEW_LIST+=("$e")
done
NEW_LIST+=("$NEW_CIDR")
JOINED=$(IFS=','; echo "${NEW_LIST[*]}")

aws ssm put-parameter --region "$REGION" --name /messaging-mcp/edge/allowed-cidrs --type String --value "$JOINED" --overwrite >/dev/null
aws ssm put-parameter --region "$REGION" --name /messaging-mcp/edge/home-ip --type String --value "$NEW_CIDR" --overwrite >/dev/null

TMP=$(mktemp -d)
for stage in $STAGES; do
  fn="aws-messaging-mcp-${stage}-ip-allowlist"
  if ! aws cloudfront get-function --name "$fn" --stage LIVE "$TMP/$fn.js" >/dev/null 2>&1; then
    echo "  $stage: function $fn not deployed yet; skipped"; continue
  fi
  sed -E "s|^var ALLOW = '[^']*'|var ALLOW = '${JOINED}'|" "$TMP/$fn.js" > "$TMP/$fn.new.js"
  ETAG=$(aws cloudfront describe-function --name "$fn" --stage DEVELOPMENT --query ETag --output text)
  ETAG=$(aws cloudfront update-function --name "$fn" --if-match "$ETAG" \
    --function-code "fileb://$TMP/$fn.new.js" \
    --function-config "Comment=Allow listed IPv4 CIDRs everywhere; allow everyone under /files/.,Runtime=cloudfront-js-2.0" \
    --query ETag --output text)
  aws cloudfront publish-function --name "$fn" --if-match "$ETAG" >/dev/null
  echo "  $stage: published"
done
rm -rf "$TMP"
echo "Allow-list now: $JOINED"
