#!/usr/bin/env python3
"""Enforce per-path coverage floors beyond pytest-cov's global gate.

PRD 11.1: >= 90 % overall (enforced by ``--cov-fail-under``), and 100 % for
``auth/`` and ``guardrails/``. Reads ``coverage.json`` produced by
``--cov-report=json``.

Usage:
    check_coverage.py coverage.json --require-100 src/aws_messaging_mcp/auth
"""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path


def main() -> int:
    """Check the report; return a process exit code."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("report", type=Path, help="coverage.json path")
    parser.add_argument(
        "--require-100",
        action="append",
        default=[],
        metavar="PATH_PREFIX",
        help="path prefix that must be fully covered (repeatable)",
    )
    args = parser.parse_args()

    data = json.loads(args.report.read_text())
    failures: list[str] = []
    for filename, entry in sorted(data["files"].items()):
        if not any(filename.startswith(prefix) for prefix in args.require_100):
            continue
        summary = entry["summary"]
        missed = summary["missing_lines"] + summary.get("missing_branches", 0)
        if missed:
            failures.append(f"{filename}: {summary['percent_covered_display']}% covered")
    if failures:
        print("Paths requiring 100% coverage fall short:")
        for failure in failures:
            print(f"  {failure}")
        return 1
    print(f"100% coverage confirmed for: {', '.join(args.require_100)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
