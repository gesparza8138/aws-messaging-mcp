"""Unit tests for aws_messaging_mcp.settings."""

import pytest

from aws_messaging_mcp.settings import Settings, resolve_origin_secret


def test_defaults() -> None:
    settings = Settings()
    assert settings.stage == "dev"
    assert settings.aws_region == "us-west-2"
    assert settings.auth_metadata_mode == "direct"
    assert settings.require_origin_secret is False
    assert settings.break_glass_enabled is False


def test_from_env_reads_mapping() -> None:
    settings = Settings.from_env(
        {
            "STAGE": "prod",
            "AWS_REGION": "us-east-1",
            "MCP_RESOURCE_URL": "https://mcp.example.com/mcp/",
            "COGNITO_ISSUER": "https://cognito-idp.us-west-2.amazonaws.com/pool",
            "COGNITO_DOMAIN": "https://auth.example.com",
            "ALLOWED_CLIENT_IDS": "client-a, client-b",
            "AUTH_METADATA_MODE": "fronted",
            "REQUIRE_ORIGIN_SECRET": "true",
            "ORIGIN_SECRET": "shh",
            "BREAK_GLASS_ENABLED": "true",
            "BREAK_GLASS_SHA256": "abc123",
            "BREAK_GLASS_SCOPES": "msg/read,msg/email:send",
        }
    )
    assert settings.stage == "prod"
    assert settings.mcp_resource_url == "https://mcp.example.com/mcp/"
    assert settings.public_base_url == "https://mcp.example.com"
    assert settings.allowed_client_ids == ("client-a", "client-b")
    assert settings.auth_metadata_mode == "fronted"
    assert settings.require_origin_secret is True
    assert settings.origin_secret == "shh"
    assert settings.break_glass_enabled is True
    assert settings.break_glass_sha256 == "abc123"
    assert settings.break_glass_scopes == ("msg/read", "msg/email:send")
    assert settings.jwks_url.endswith("/pool/.well-known/jwks.json")


def test_from_env_falls_back_to_defaults() -> None:
    settings = Settings.from_env({})
    assert settings == Settings()


def test_public_base_url_override() -> None:
    settings = Settings.from_env(
        {
            "MCP_RESOURCE_URL": "https://x.example.com/mcp/",
            "PUBLIC_BASE_URL": "https://y.example.com",
        }
    )
    assert settings.public_base_url == "https://y.example.com"


def test_from_env_defaults_to_os_environ(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("STAGE", "prod")
    settings = Settings.from_env()
    assert settings.stage == "prod"


def test_resolve_origin_secret_no_param_is_noop() -> None:
    settings = Settings()
    assert resolve_origin_secret(settings, env={}) is settings


def test_resolve_origin_secret_literal_wins() -> None:
    settings = Settings(origin_secret="literal")
    resolved = resolve_origin_secret(settings, env={"ORIGIN_SECRET_PARAM": "/x"})
    assert resolved.origin_secret == "literal"


def test_resolve_origin_secret_fetches_from_ssm() -> None:
    fetched: list[str] = []

    def fake_fetch(name: str) -> str:
        fetched.append(name)
        return "from-ssm"

    settings = Settings()
    resolved = resolve_origin_secret(
        settings, env={"ORIGIN_SECRET_PARAM": "/stack/origin-secret"}, fetch=fake_fetch
    )
    assert resolved.origin_secret == "from-ssm"
    assert fetched == ["/stack/origin-secret"]
