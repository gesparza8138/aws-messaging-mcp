# Testing

Four tiers, mirroring [PRD §11](PRD.md#11-testing-strategy). The first three
run in CI on every pull request; e2e runs against the deployed dev stack.

| Tier | Where | Command | What it proves |
| --- | --- | --- | --- |
| Unit | `internal/*/…_test.go` | `make test` | Auth matrix, guardrails, adapters, settings — coverage ≥ 90 % overall, 100 % for `internal/auth` and `internal/guardrails` (`scripts/check_coverage.sh`) |
| Integration | `internal/httpapi/server_test.go` | `make test` | Full HTTP stack in-process: OAuth and break-glass round trips, 401/403 contracts, both `/mcp` paths, metadata documents |
| Contract | `internal/schemas/contract_test.go` | `make test` | Tool schemas mirror the AWS SDK Go v2 request shapes field-for-field (PRD G5) |
| E2E | `e2e/` (build tag `e2e`) | `make e2e` | The deployed dev stack through the public edge: allow-list, Cognito `client_credentials` token, every tool, one real email, its `Delivery` event in the SES trail |

## Running e2e

CI runs the suite automatically as the `e2e-dev` job of every executed
`deploy-dev` run, self-registering the runner's IP on the edge allow-list for
the duration (PRD R7) and reading the client credentials from SSM.

Locally (needs AWS credentials for the dev account and your IP on the
allow-list — `scripts/update-my-ip.sh`):

```sh
export E2E_BASE_URL=https://dev.mcp.gabriel-esparza.com
export E2E_TOKEN_URL=https://messaging-mcp-dev.auth.us-west-2.amazoncognito.com/oauth2/token
export E2E_CLIENT_ID=$(aws ssm get-parameter --name /messaging-mcp/dev/ci-client-id --query Parameter.Value --output text)
export E2E_CLIENT_SECRET=$(aws ssm get-parameter --name /messaging-mcp/dev/ci-client-secret --with-decryption --query Parameter.Value --output text)
export E2E_SENDER=mcp-dev@gabriel-esparza.com
export E2E_RECIPIENT=esparza.gabriel@gmail.com
export E2E_CONFIG_SET=aws-messaging-mcp-dev
export E2E_EVENTS_LOG_GROUP=/aws-messaging-mcp/dev/ses-events
# SMS/MMS (M3): skipped while unset
export E2E_ORIGINATION=$(aws ssm get-parameter --name /messaging-mcp/eum/phone-number --query Parameter.Value --output text)
export E2E_SMS_CONFIG_SET=aws-messaging-mcp-dev-sms
export E2E_EUM_LOG_GROUP=/aws-messaging-mcp/dev/eum-events
export E2E_TEST_PHONE=+1XXXXXXXXXX   # your mobile; also a GitHub dev secret
make e2e
```

> [!NOTE]
> The suite sends one real email per run to `E2E_RECIPIENT` — and, when
> `E2E_TEST_PHONE` is set, one SMS and one MMS — consuming a few rate-limit
> slots (guardrails run for dry-run and blocked attempts too). Delivery/event
> checks skip — never fail — on unreadable log groups or provider delay; the
> send is the assertion. While the EUM account is in the **SMS sandbox**,
> real sends reach only verified destination numbers; the suite turns the
> sandbox refusal into a skip, so verify your phone
> (`aws pinpoint-sms-voice-v2 create-verified-destination-number …`) or wait
> for production access to exercise the full path.

## Why `client_credentials`

The CI client authenticates machine-to-machine: Cognito's `InitiateAuth`
(`USER_PASSWORD_AUTH`) access tokens carry only the fixed
`aws.cognito.signin.user.admin` scope — never resource-server scopes — and the
pool's required TOTP would block a non-interactive user regardless. The
`client_credentials` grant issues access tokens with real `msg/*` scopes and no
user, and since November 2025 Cognito bills M2M per successful token request
(no flat per-client fee), which keeps the account inside budget (PRD S1).
