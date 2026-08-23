"""Cognito access-token verification (PRD A8).

Tokens are validated for signature (RS256 against the pool's JWKS, cached at
most one hour), issuer, expiry, ``token_use == "access"``, and an allow-listed
``client_id``. Cognito access tokens carry no ``aud`` claim; the ``client_id``
claim plays that role.
"""

from __future__ import annotations

from collections.abc import Callable

import jwt as pyjwt

from aws_messaging_mcp.auth.principal import Principal

#: Resolves the signing key for a token. Production uses ``PyJWKClient``;
#: tests inject a resolver returning a PEM public key.
KeyResolver = Callable[[str], "pyjwt.PyJWK | str | bytes"]

_JWKS_CACHE_SECONDS = 3600


class AuthError(Exception):
    """A bearer token failed verification.

    Attributes:
        reason: Machine-readable failure reason for structured logs. Never
            echoed back to the caller beyond a generic 401.
    """

    def __init__(self, reason: str) -> None:
        """Store the failure reason.

        Args:
            reason: Machine-readable failure reason.
        """
        super().__init__(reason)
        self.reason = reason


class TokenVerifier:
    """Verifies Cognito access tokens and produces a ``Principal``."""

    def __init__(
        self,
        issuer: str,
        allowed_client_ids: tuple[str, ...],
        key_resolver: KeyResolver | None = None,
    ) -> None:
        """Configure the verifier.

        Args:
            issuer: Expected ``iss`` claim (the Cognito user-pool issuer URL).
            allowed_client_ids: App-client ids accepted in ``client_id``.
            key_resolver: Signing-key resolver; defaults to a caching
                ``PyJWKClient`` against the issuer's JWKS endpoint.
        """
        self._issuer = issuer
        self._allowed_client_ids = frozenset(allowed_client_ids)
        if key_resolver is None:
            jwks_client = pyjwt.PyJWKClient(
                f"{issuer}/.well-known/jwks.json",
                cache_keys=True,
                lifespan=_JWKS_CACHE_SECONDS,
            )
            key_resolver = jwks_client.get_signing_key_from_jwt
        self._resolve_key = key_resolver

    def verify(self, token: str) -> Principal:
        """Verify ``token`` and return the authenticated principal.

        Args:
            token: The raw bearer token.

        Returns:
            The verified caller.

        Raises:
            AuthError: On any verification failure, with a structured reason.
        """
        try:
            key = self._resolve_key(token)
        except Exception as exc:
            raise AuthError("unresolvable_signing_key") from exc
        try:
            claims = pyjwt.decode(
                token,
                key=key,
                algorithms=["RS256"],
                issuer=self._issuer,
                options={"require": ["exp", "iss", "sub"]},
            )
        except pyjwt.ExpiredSignatureError as exc:
            raise AuthError("expired") from exc
        except pyjwt.InvalidIssuerError as exc:
            raise AuthError("wrong_issuer") from exc
        except pyjwt.MissingRequiredClaimError as exc:
            raise AuthError("missing_claim") from exc
        except pyjwt.PyJWTError as exc:
            raise AuthError("invalid_token") from exc

        if claims.get("token_use") != "access":
            raise AuthError("wrong_token_use")
        client_id = claims.get("client_id")
        if not isinstance(client_id, str) or client_id not in self._allowed_client_ids:
            raise AuthError("unknown_client")

        scopes = frozenset(str(claims.get("scope", "")).split())
        return Principal(
            subject=str(claims["sub"]),
            client_id=client_id,
            scopes=scopes,
            method="oauth",
        )
