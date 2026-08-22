# Claude Code kickoff prompt

Copy everything below the line into Claude Code from the scaffold directory (the one containing `CLAUDE.md`, `docs/`, and `scripts/`). Use plan mode (`Shift+Tab` or ask it to plan) so you approve the plan before any code is written.

---

Read `CLAUDE.md`, then `docs/PRD.md` in full, then `docs/setup/github.md`, `docs/setup/dns.md`, and `docs/setup/aws-prerequisites.md`.

Produce an implementation plan for **milestones M0 and M1 only** (PRD §16). Do not plan M2+ yet; M1's findings will shape them.

Constraints to honor in the plan:

1. Step 1 of M0 is creating the GitHub repository and applying its configuration by running `scripts/setup-github.sh --create` with `GH_OWNER=gesparza8138 AWS_ACCOUNT_ID=585008049602`. Before running it, make the initial scaffold commit (signed) so the push is not blocked by the rulesets the script applies afterward. Verify with the commands in `docs/setup/github.md` §9 and report the output.
2. M0 ends when `ci.yml` is green on a PR with an empty app: `quality`, `unit-tests`, `security-scans`, `iac-scans`, `docs`, `commitlint` all passing, and the `bootstrap` and `edge` stacks are deployed. Every AWS deploy requires my explicit approval first, dev included — show me the change set summary.
3. M1 is the auth spike. Exit criteria: Cognito user pool + two app clients, `app.yaml` with a `hello` tool behind the full auth middleware, CloudFront + WAF with the IP allow-list and the `/files/*` exemption, OAuth round-trip succeeding from **both** Claude Code (`claude mcp add ... --client-id ... --callback-port 8765`) and Claude Desktop (custom connector), and a claude.ai Routine calling the `hello` tool after the access token has expired (proves refresh). Resolve risks R2, R3, R7, and R8 from PRD §15 and write the outcome into the decision log.
4. Include in the plan the exact points where I have to do something manually (GoDaddy NS records, Cognito user creation and TOTP enrolment, Claude Desktop connector setup, CloudFront plan enrolment if CloudFormation can't set it) and what you need back from me at each point.
5. For each PR in the plan, list: files touched, tests added, docs updated, and the CI checks it must pass.
6. Call out anything in the PRD you believe is wrong, out of date, or infeasible before we start — especially Cognito's lack of `code_challenge_methods_supported`, the CloudFront Free-plan/CloudFormation question, and the `SendRcsMessage` API shape in the installed boto3.

Output the plan as `docs/plans/m0-m1.md` (GitHub-flavored Markdown) and stop for my review before implementing anything.
