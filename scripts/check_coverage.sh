#!/usr/bin/env bash
# Enforce coverage floors from a Go cover profile (PRD 11.1): total >= 90 %,
# and 100 % for every package listed as an argument (auth/, guardrails/).
#
# Usage: scripts/check_coverage.sh coverage.out [pkg-path-prefix ...]
set -euo pipefail

PROFILE="$1"; shift
TOTAL=$(go tool cover -func="$PROFILE" | awk '/^total:/ {gsub("%","",$3); print $3}')
echo "total coverage: ${TOTAL}%"
awk -v t="$TOTAL" 'BEGIN { if (t + 0 < 90) { print "FAIL: total coverage below 90%"; exit 1 } }'

status=0
for prefix in "$@"; do
  # Any function in a 100 %-required package that is not fully covered fails.
  short=$(go tool cover -func="$PROFILE" | awk -v p="$prefix" '$1 ~ p && $3+0 < 100 { print }')
  if [[ -n "$short" ]]; then
    echo "FAIL: ${prefix} must be 100% covered; uncovered functions:"
    echo "$short"
    status=1
  else
    echo "100% coverage confirmed for ${prefix}"
  fi
done
exit $status
