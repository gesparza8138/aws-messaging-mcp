"""Break-glass static bearer token (PRD A7).

The token itself is never stored: only its SHA-256 hex digest lives in SSM.
Comparison is constant-time. Disabled by default in prod (PRD S10).
"""

from __future__ import annotations

import hashlib
import hmac

from aws_messaging_mcp.auth.principal import Principal


def verify_break_glass(
    token: str,
    expected_sha256_hex: str,
    scopes: tuple[str, ...],
) -> Principal | None:
    """Check ``token`` against the stored digest in constant time.

    Args:
        token: The presented bearer token.
        expected_sha256_hex: Hex SHA-256 digest of the real break-glass token.
        scopes: Scopes granted on a match.

    Returns:
        A break-glass ``Principal`` on match, else ``None`` so the caller can
        fall through to JWT verification.
    """
    digest = hashlib.sha256(token.encode()).hexdigest()
    if not hmac.compare_digest(digest, expected_sha256_hex.lower()):
        return None
    return Principal(
        subject="break-glass",
        client_id=None,
        scopes=frozenset(scopes),
        method="break_glass",
    )
