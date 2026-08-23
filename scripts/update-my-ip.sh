#!/usr/bin/env bash
# Refresh the owner-IP entry in the edge WAF allow-list (PRD R7).
# Keeps every other entry (Anthropic egress, temporary CI entries) intact:
# removes any previous entry tagged as "home" via the state parameter and
# adds the current public IP.
#
# Usage: ./scripts/update-my-ip.sh [--ip 1.2.3.4]
set -euo pipefail

REGION=us-east-1
NEW_IP="${2:-}"
[[ "${1:-}" == "--ip" && -n "$NEW_IP" ]] || NEW_IP=$(curl -s https://checkip.amazonaws.com)
NEW_CIDR="${NEW_IP}/32"

IPSET_ARN=$(aws ssm get-parameter --region us-west-2 --name /messaging-mcp/edge/ip-set-arn \
  --query Parameter.Value --output text)
IPSET_ID=$(awk -F/ '{print $NF}' <<<"$IPSET_ARN")
IPSET_NAME=$(awk -F/ '{print $(NF-1)}' <<<"$IPSET_ARN")

OLD_HOME=$(aws ssm get-parameter --region us-west-2 --name /messaging-mcp/edge/home-ip \
  --query Parameter.Value --output text 2>/dev/null || echo "")

read -r LOCK_TOKEN ADDRESSES < <(aws wafv2 get-ip-set --region "$REGION" --scope CLOUDFRONT \
  --id "$IPSET_ID" --name "$IPSET_NAME" \
  --query '[LockToken, join(`,`, IPSet.Addresses)]' --output text)

NEW_LIST=()
IFS=',' read -ra CURRENT <<<"$ADDRESSES"
for addr in "${CURRENT[@]}"; do
  [[ "$addr" == "$OLD_HOME" || "$addr" == "$NEW_CIDR" ]] && continue
  NEW_LIST+=("$addr")
done
NEW_LIST+=("$NEW_CIDR")

aws wafv2 update-ip-set --region "$REGION" --scope CLOUDFRONT \
  --id "$IPSET_ID" --name "$IPSET_NAME" --lock-token "$LOCK_TOKEN" \
  --addresses "${NEW_LIST[@]}" >/dev/null

aws ssm put-parameter --region us-west-2 --name /messaging-mcp/edge/home-ip \
  --type String --value "$NEW_CIDR" --overwrite >/dev/null

echo "Allow-list now: ${NEW_LIST[*]}"
