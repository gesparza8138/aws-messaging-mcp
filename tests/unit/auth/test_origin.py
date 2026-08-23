"""Unit tests for aws_messaging_mcp.auth.origin."""

from aws_messaging_mcp.auth.origin import origin_secret_ok


def test_not_required_always_passes() -> None:
    assert origin_secret_ok(None, None, required=False)
    assert origin_secret_ok("anything", None, required=False)


def test_required_matching_secret_passes() -> None:
    assert origin_secret_ok("s3cret", "s3cret", required=True)


def test_required_mismatch_fails() -> None:
    assert not origin_secret_ok("wrong", "s3cret", required=True)


def test_required_missing_header_fails() -> None:
    assert not origin_secret_ok(None, "s3cret", required=True)


def test_required_unconfigured_secret_fails_closed() -> None:
    assert not origin_secret_ok("anything", None, required=True)
