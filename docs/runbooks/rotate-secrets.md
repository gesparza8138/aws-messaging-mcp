# Runbook: rotating secrets

All rotations run from the workstation with owner credentials; nothing here
needs a code change. Rotate on suspicion of exposure, or annually.

| Secret | Rotate with | Then |
| --- | --- | --- |
| Origin secret (`X-Origin-Secret`) | `go run ./cmd/ops rotate-secret --stage <s> --origin-only` | Redeploy the stage (the parameter feeds CloudFront and the Lambda env); until then the old value keeps working |
| Break-glass token | `go run ./cmd/ops rotate-secret --stage <s> --break-glass-only` | Store the printed token (shown once); redeploy the stage |
| Files signing key | `go run ./cmd/ops rotate-signing-key --stage <s>` | Redeploy the stage. **Every outstanding signed URL dies** on the key-group swap — this is the emergency lever for a leaked link (PRD R9), so on a routine rotation warn anyone holding live links first |
| Cognito app clients | Recreate via a stack change (clients are template-managed) | Claude clients re-authenticate on next use |

After any rotation: `make e2e` against dev (or wait for the next deploy's
e2e job) confirms the auth chain and, for the signing key, that fresh links
sign and download.
