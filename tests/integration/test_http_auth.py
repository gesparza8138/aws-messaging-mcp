"""HTTP-level auth behaviour: origin secret, 401 shape, well-known documents."""

from typing import Any

import httpx

from aws_messaging_mcp.settings import SCOPES_SUPPORTED, Settings
from tests.integration.conftest import ORIGIN_SECRET

Server = tuple[str, Settings]


def _get(base_url: str, path: str, **kwargs: Any) -> httpx.Response:
    headers = kwargs.pop("headers", {})
    headers.setdefault("X-Origin-Secret", ORIGIN_SECRET)
    return httpx.get(f"{base_url}{path}", headers=headers, **kwargs)


def test_missing_origin_secret_is_403(server: Server) -> None:
    base_url, _ = server
    response = httpx.get(f"{base_url}/healthz")
    assert response.status_code == 403


def test_wrong_origin_secret_is_403(server: Server) -> None:
    base_url, _ = server
    response = httpx.get(f"{base_url}/healthz", headers={"X-Origin-Secret": "wrong"})
    assert response.status_code == 403


def test_healthz_passes_with_origin_secret(server: Server) -> None:
    base_url, _ = server
    response = _get(base_url, "/healthz")
    assert response.status_code == 200
    assert response.json() == {"status": "ok", "stage": "test"}


def test_mcp_without_token_is_401_with_resource_metadata(server: Server) -> None:
    base_url, _ = server
    response = httpx.post(
        f"{base_url}/mcp/",
        headers={"X-Origin-Secret": ORIGIN_SECRET},
        json={},
    )
    assert response.status_code == 401
    expected = f'Bearer resource_metadata="{base_url}/.well-known/oauth-protected-resource"'
    assert response.headers["WWW-Authenticate"] == expected


def test_protected_resource_metadata(server: Server) -> None:
    base_url, settings = server
    for path in (
        "/.well-known/oauth-protected-resource",
        "/.well-known/oauth-protected-resource/mcp",
    ):
        document = _get(base_url, path).json()
        assert document == {
            "resource": settings.mcp_resource_url,
            "authorization_servers": [settings.cognito_issuer],
            "scopes_supported": list(SCOPES_SUPPORTED),
            "bearer_methods_supported": ["header"],
        }


def test_authorization_server_metadata_adds_pkce_field(server: Server) -> None:
    base_url, settings = server
    document = _get(base_url, "/.well-known/oauth-authorization-server").json()
    assert document["issuer"] == settings.cognito_issuer
    assert document["authorization_endpoint"] == f"{settings.cognito_domain}/oauth2/authorize"
    assert document["token_endpoint"] == f"{settings.cognito_domain}/oauth2/token"
    assert document["jwks_uri"] == settings.jwks_url
    assert document["code_challenge_methods_supported"] == ["S256"]


def test_mcp_without_trailing_slash_is_not_redirected(server: Server) -> None:
    """Anthropic's hosted bridge posts to /mcp and never follows redirects."""
    base_url, _ = server
    response = httpx.post(
        f"{base_url}/mcp",
        headers={"X-Origin-Secret": ORIGIN_SECRET},
        json={},
        follow_redirects=False,
    )
    assert response.status_code == 401
    assert "WWW-Authenticate" in response.headers
