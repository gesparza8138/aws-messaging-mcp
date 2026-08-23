"""Unit tests for aws_messaging_mcp.auth.scopes."""

import pytest

from aws_messaging_mcp.auth.principal import Principal
from aws_messaging_mcp.auth.scopes import ScopeError, require_scope


def _principal(*scopes: str) -> Principal:
    return Principal(subject="sub-1", client_id="c1", scopes=frozenset(scopes), method="oauth")


def test_present_scope_passes() -> None:
    require_scope(_principal("msg/read", "msg/sms:send"), "msg/read")


def test_missing_scope_raises_with_scope_name() -> None:
    with pytest.raises(ScopeError, match="msg/email:send"):
        require_scope(_principal("msg/read"), "msg/email:send")


def test_no_scopes_at_all_raises() -> None:
    with pytest.raises(ScopeError):
        require_scope(_principal(), "msg/read")
