"""CloudFront origin-secret verification.

CloudFront injects ``X-Origin-Secret`` on every forwarded request (PRD A9).
Traffic that reaches the Function URL directly lacks the header and is
rejected before any token work, keeping the bypass path cheap.
"""

from __future__ import annotations

import hmac


def origin_secret_ok(provided: str | None, expected: str | None, required: bool) -> bool:
    """Check the origin-secret header in constant time.

    Args:
        provided: Header value from the request, or ``None`` if absent.
        expected: Configured secret, or ``None`` if not provisioned.
        required: Whether enforcement is on (off only for local development).

    Returns:
        True when the request may proceed. When enforcement is on, a missing
        configured secret fails closed.
    """
    if not required:
        return True
    if expected is None or provided is None:
        return False
    return hmac.compare_digest(provided.encode(), expected.encode())
