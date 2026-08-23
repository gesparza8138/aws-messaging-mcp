#!/usr/bin/env bash
# Temporarily admit this machine's egress IP to the DEV edge allow-list (R7),
# for the e2e job: `add` before the tests, `remove` in an always() step after.
# The entry lives only in SSM /messaging-mcp/edge/allowed-cidrs plus the live
# CloudFront Function, so no stack deploy is involved; `remove` recomputes the
# same egress IP, which holds because both run on the same runner and the
# deploy-dev concurrency group serializes runs.
set -euo pipefail

ACTION=${1:?usage: e2e-allowlist.sh add|remove}
REGION=${AWS_REGION:-us-west-2}
PARAM=/messaging-mcp/edge/allowed-cidrs
FN=aws-messaging-mcp-dev-ip-allowlist
CIDR="$(curl -s https://checkip.amazonaws.com)/32"

CURRENT=$(aws ssm get-parameter --region "$REGION" --name "$PARAM" --query Parameter.Value --output text)
NEW_LIST=()
IFS=',' read -ra ENTRIES <<<"$CURRENT"
for e in "${ENTRIES[@]}"; do
  e=$(echo "$e" | tr -d ' ')
  [[ -z "$e" || "$e" == "$CIDR" ]] && continue
  NEW_LIST+=("$e")
done
[[ "$ACTION" == "add" ]] && NEW_LIST+=("$CIDR")
JOINED=$(IFS=','; echo "${NEW_LIST[*]}")

aws ssm put-parameter --region "$REGION" --name "$PARAM" --type String --value "$JOINED" --overwrite >/dev/null

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
aws cloudfront get-function --name "$FN" --stage LIVE "$TMP/fn.js" >/dev/null
sed -E "s|^var ALLOW = '[^']*'|var ALLOW = '${JOINED}'|" "$TMP/fn.js" > "$TMP/fn.new.js"
ETAG=$(aws cloudfront describe-function --name "$FN" --stage DEVELOPMENT --query ETag --output text)
ETAG=$(aws cloudfront update-function --name "$FN" --if-match "$ETAG" \
  --function-code "fileb://$TMP/fn.new.js" \
  --function-config "Comment=Allow listed IPv4 CIDRs everywhere; allow everyone under /files/.,Runtime=cloudfront-js-2.0" \
  --query ETag --output text)
aws cloudfront publish-function --name "$FN" --if-match "$ETAG" >/dev/null
echo "$ACTION $CIDR done; allow-list now has $(echo "$JOINED" | awk -F, '{print NF}') entries"
