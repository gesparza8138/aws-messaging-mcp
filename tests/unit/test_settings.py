"""Unit tests for aws_messaging_mcp.settings."""

import pytest

from aws_messaging_mcp.settings import Settings


def test_defaults() -> None:
    settings = Settings()
    assert settings.stage == "dev"
    assert settings.aws_region == "us-west-2"


def test_from_env_reads_mapping() -> None:
    settings = Settings.from_env({"STAGE": "prod", "AWS_REGION": "us-east-1"})
    assert settings.stage == "prod"
    assert settings.aws_region == "us-east-1"


def test_from_env_falls_back_to_defaults() -> None:
    settings = Settings.from_env({})
    assert settings == Settings()


def test_from_env_defaults_to_os_environ(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("STAGE", "prod")
    monkeypatch.setenv("AWS_REGION", "eu-west-1")
    settings = Settings.from_env()
    assert settings.stage == "prod"
    assert settings.aws_region == "eu-west-1"
