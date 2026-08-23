#!/usr/bin/env python3
"""Provision or rotate the per-stage shared secrets in SSM (PRD S2).

Creates/updates:
  /messaging-mcp/<stage>/origin-secret       SecureString - CloudFront origin header
  /messaging-mcp/<stage>/break-glass-sha256  String - SHA-256 of the break-glass token

The break-glass token itself is printed exactly once; store it in a password
manager. Rotating the origin secret requires a stack redeploy so CloudFront
starts injecting the new value (the old one keeps working until then because
the Lambda re-reads SSM only at cold start - rotate, then deploy).

Usage:
    uv run python scripts/rotate-secret.py --stage dev [--origin-only|--break-glass-only]
"""

from __future__ import annotations

import argparse
import hashlib
import secrets
import sys

import boto3


def main() -> int:
    """Rotate the requested secrets; return a process exit code."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--stage", required=True, choices=["dev", "prod"])
    parser.add_argument("--region", default="us-west-2")
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--origin-only", action="store_true")
    group.add_argument("--break-glass-only", action="store_true")
    args = parser.parse_args()

    ssm = boto3.client("ssm", region_name=args.region)
    prefix = f"/messaging-mcp/{args.stage}"

    if not args.break_glass_only:
        origin_secret = secrets.token_urlsafe(32)
        ssm.put_parameter(
            Name=f"{prefix}/origin-secret",
            Value=origin_secret,
            Type="SecureString",
            Overwrite=True,
        )
        print(f"rotated {prefix}/origin-secret (redeploy the {args.stage} stack to apply)")

    if not args.origin_only:
        token = secrets.token_urlsafe(32)
        digest = hashlib.sha256(token.encode()).hexdigest()
        ssm.put_parameter(
            Name=f"{prefix}/break-glass-sha256",
            Value=digest,
            Type="String",
            Overwrite=True,
        )
        print(f"rotated {prefix}/break-glass-sha256")
        print("break-glass token (shown once, store it now):")
        print(f"  {token}")

    return 0


if __name__ == "__main__":
    sys.exit(main())
