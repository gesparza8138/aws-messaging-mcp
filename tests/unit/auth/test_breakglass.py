"""Unit tests for aws_messaging_mcp.auth.breakglass."""

import hashlib

from aws_messaging_mcp.auth.breakglass import verify_break_glass

TOKEN = "correct-horse-battery-staple"
DIGEST = hashlib.sha256(TOKEN.encode()).hexdigest()


def test_matching_token_yields_principal() -> None:
    principal = verify_break_glass(TOKEN, DIGEST, ("msg/read", "msg/email:send"))
    assert principal is not None
    assert principal.subject == "break-glass"
    assert principal.client_id is None
    assert principal.method == "break_glass"
    assert principal.scopes == {"msg/read", "msg/email:send"}


def test_digest_comparison_is_case_insensitive() -> None:
    assert verify_break_glass(TOKEN, DIGEST.upper(), ("msg/read",)) is not None


def test_wrong_token_returns_none() -> None:
    assert verify_break_glass("wrong-token", DIGEST, ("msg/read",)) is None
