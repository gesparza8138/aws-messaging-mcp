"""Unit tests for aws_messaging_mcp.auth.jwt - the PRD 11.1 validation matrix."""

import time
from typing import Any

import jwt as pyjwt
import pytest
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

from aws_messaging_mcp.auth.jwt import AuthError, TokenVerifier

ISSUER = "https://cognito-idp.us-west-2.amazonaws.com/us-west-2_TESTPOOL"
CLIENT_ID = "test-client-id"


def _keypair() -> tuple[bytes, bytes]:
    key = rsa.generate_private_key(public_exponent=65537, key_size=2048)
    private_pem = key.private_bytes(
        serialization.Encoding.PEM,
        serialization.PrivateFormat.PKCS8,
        serialization.NoEncryption(),
    )
    public_pem = key.public_key().public_bytes(
        serialization.Encoding.PEM,
        serialization.PublicFormat.SubjectPublicKeyInfo,
    )
    return private_pem, public_pem


PRIVATE_PEM, PUBLIC_PEM = _keypair()
OTHER_PRIVATE_PEM, _ = _keypair()


def _claims(**overrides: Any) -> dict[str, Any]:
    claims: dict[str, Any] = {
        "sub": "user-sub-123",
        "iss": ISSUER,
        "exp": int(time.time()) + 300,
        "token_use": "access",
        "client_id": CLIENT_ID,
        "scope": "msg/read msg/email:send",
    }
    claims.update(overrides)
    return {k: v for k, v in claims.items() if v is not None}


def _mint(signing_key: bytes = PRIVATE_PEM, **overrides: Any) -> str:
    return pyjwt.encode(_claims(**overrides), signing_key, algorithm="RS256")


def _verifier() -> TokenVerifier:
    return TokenVerifier(
        issuer=ISSUER,
        allowed_client_ids=(CLIENT_ID,),
        key_resolver=lambda _token: PUBLIC_PEM,
    )


def test_valid_token_yields_principal() -> None:
    principal = _verifier().verify(_mint())
    assert principal.subject == "user-sub-123"
    assert principal.client_id == CLIENT_ID
    assert principal.method == "oauth"
    assert principal.scopes == {"msg/read", "msg/email:send"}


def test_bad_signature_rejected() -> None:
    with pytest.raises(AuthError) as excinfo:
        _verifier().verify(_mint(signing_key=OTHER_PRIVATE_PEM))
    assert excinfo.value.reason == "invalid_token"


def test_expired_token_rejected() -> None:
    with pytest.raises(AuthError) as excinfo:
        _verifier().verify(_mint(exp=int(time.time()) - 60))
    assert excinfo.value.reason == "expired"


def test_wrong_issuer_rejected() -> None:
    with pytest.raises(AuthError) as excinfo:
        _verifier().verify(_mint(iss="https://evil.example.com"))
    assert excinfo.value.reason == "wrong_issuer"


def test_missing_sub_rejected() -> None:
    with pytest.raises(AuthError) as excinfo:
        _verifier().verify(_mint(sub=None))
    assert excinfo.value.reason == "missing_claim"


def test_id_token_rejected() -> None:
    with pytest.raises(AuthError) as excinfo:
        _verifier().verify(_mint(token_use="id"))
    assert excinfo.value.reason == "wrong_token_use"


def test_unknown_client_id_rejected() -> None:
    with pytest.raises(AuthError) as excinfo:
        _verifier().verify(_mint(client_id="someone-elses-client"))
    assert excinfo.value.reason == "unknown_client"


def test_missing_scope_claim_yields_empty_scopes() -> None:
    principal = _verifier().verify(_mint(scope=None))
    assert principal.scopes == frozenset()


def test_key_resolver_failure_rejected() -> None:
    def broken_resolver(_token: str) -> bytes:
        raise pyjwt.PyJWKClientError("kid not found")

    verifier = TokenVerifier(ISSUER, (CLIENT_ID,), key_resolver=broken_resolver)
    with pytest.raises(AuthError) as excinfo:
        verifier.verify(_mint())
    assert excinfo.value.reason == "unresolvable_signing_key"


def test_default_resolver_is_jwks_client() -> None:
    verifier = TokenVerifier(ISSUER, (CLIENT_ID,))
    resolver = verifier._resolve_key
    assert resolver.__self__.uri == f"{ISSUER}/.well-known/jwks.json"  # type: ignore[attr-defined]
