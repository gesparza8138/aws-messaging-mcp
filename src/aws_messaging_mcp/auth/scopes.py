"""Per-tool scope enforcement (PRD A6)."""

from __future__ import annotations

from aws_messaging_mcp.auth.principal import Principal


class ScopeError(Exception):
    """The caller lacks a required scope. The message is safe to return."""


def require_scope(principal: Principal, scope: str) -> None:
    """Raise unless ``principal`` holds ``scope``.

    Args:
        principal: The authenticated caller.
        scope: Required scope, e.g. ``msg/read``.

    Raises:
        ScopeError: When the scope is missing; the message names the scope so
            the model can explain the refusal instead of retrying blindly.
    """
    if scope not in principal.scopes:
        raise ScopeError(f"caller lacks required scope '{scope}'")
