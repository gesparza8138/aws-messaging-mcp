# Runbook: runaway sends

For a compromised token, a misbehaving client, or any situation where the
server is sending messages it shouldn't. Steps are ordered by
blast-radius-per-second; do them top-down and stop when the bleeding stops.
Everything runs from the workstation with owner credentials.

## 1. Cut client access at the edge (~30 s)

Replace the allow-list with a loopback entry — every MCP call (including
break-glass) dies at CloudFront; `/files/*` links keep working:

```bash
./scripts/update-my-ip.sh --ip 127.0.0.1 --stages "dev prod"
```

## 2. Stop the Lambda outright (~30 s)

Reserved concurrency zero refuses every invocation, scheduler included:

```bash
aws lambda put-function-concurrency --function-name aws-messaging-mcp-prod --reserved-concurrent-executions 0
```

(Repeat for `-dev` if both stages are suspect.)

## 3. Kill credentials (~2 min)

```bash
# Revoke every refresh token for the owner user (access tokens die ≤15 min later)
aws cognito-idp admin-user-global-sign-out --user-pool-id <prod-pool-id> --username <owner-sub>
# If break-glass may be exposed: rotate it (also rotates the hash in SSM)
go run ./cmd/ops rotate-secret --stage prod --break-glass-only
```

Pool id: `aws cloudformation describe-stacks --stack-name aws-messaging-mcp-prod --query 'Stacks[0].Outputs[?OutputKey==\`UserPoolId\`].OutputValue' --output text`.

## 4. Verify sends have stopped

Watch both event trails for two quiet minutes:

```bash
aws logs tail /aws-messaging-mcp/prod/ses-events --since 5m --follow
aws logs tail /aws-messaging-mcp/prod/eum-events --since 5m --follow
```

## 5. Restore, in reverse

```bash
aws lambda delete-function-concurrency --function-name aws-messaging-mcp-prod
./scripts/update-my-ip.sh   # restores your real IP; re-adds nothing else
```

Re-authenticate clients (step 3 signed everything out), then send one
`DryRun` from Claude Code to confirm the pipeline before trusting it again.

> [!NOTE]
> Step 2 conflicts with the stack's `ReservedConcurrency` parameter: the
> next deploy resets whatever you set by hand. That is a feature — restoring
> service is just a redeploy — but don't be surprised by it.

## Drill log

| Date | Stage | Time to quiet (steps 1–2) | Notes |
| --- | --- | --- | --- |
| _pending first drill (M5 PR C)_ | dev | — | — |
