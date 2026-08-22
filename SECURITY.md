# Security policy

This is a single-owner personal service; there is no bug-bounty programme.

## Reporting a vulnerability

Use GitHub's **private vulnerability reporting** on this repository
(Security → Report a vulnerability). Reports are read by the owner
([@gesparza8138](https://github.com/gesparza8138)); expect an initial response
within a week.

Please do not open public issues for suspected vulnerabilities.

## Scope notes

- The server's threat model and security controls are described in
  [docs/PRD.md](docs/PRD.md) §7.
- Credentials never live in this repository; secrets are stored in AWS SSM
  Parameter Store and GitHub environment secrets.
