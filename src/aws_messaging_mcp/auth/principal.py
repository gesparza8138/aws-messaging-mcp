"""The authenticated caller identity attached to a request."""

from __future__ import annotations

from dataclasses import dataclass


@dataclass(frozen=True, slots=True)
class Principal:
    """An authenticated caller.

    Attributes:
        subject: Stable caller id - the Cognito ``sub`` claim, or
            ``break-glass`` for the static-token path.
        client_id: OAuth client id the token was issued to, or ``None`` for
            break-glass.
        scopes: Scopes the caller holds, e.g. ``{"msg/read"}``.
        method: ``oauth`` or ``break_glass``.
    """

    subject: str
    client_id: str | None
    scopes: frozenset[str]
    method: str
