"""Runtime settings for the aws-messaging-mcp server.

Settings are resolved from environment variables so the same code runs
unchanged on Lambda (variables set by CloudFormation) and locally (variables
set by the shell or ``make dev``). Secrets referenced by SSM parameter name
are fetched once at cold start via ``resolve_origin_secret``.
"""

from __future__ import annotations

import os
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from urllib.parse import urlsplit

_DEFAULT_STAGE = "dev"
_DEFAULT_REGION = "us-west-2"

#: Scopes the resource server defines (PRD A6), in the order advertised.
SCOPES_SUPPORTED: tuple[str, ...] = (
    "msg/read",
    "msg/email:send",
    "msg/sms:send",
    "msg/rcs:send",
    "msg/files:write",
)


def _split_csv(value: str) -> tuple[str, ...]:
    """Split a comma-separated variable into a tuple, dropping blanks.

    Args:
        value: Raw variable value, e.g. ``"a,b, c"``.

    Returns:
        The non-empty, stripped items.
    """
    return tuple(item.strip() for item in value.split(",") if item.strip())


@dataclass(frozen=True, slots=True)
class Settings:
    """Immutable server configuration.

    Attributes:
        stage: Deployment stage, ``dev`` or ``prod``.
        aws_region: AWS region the server operates in.
        mcp_resource_url: Canonical resource identifier of this MCP server
            (RFC 9728 ``resource``), e.g. ``https://dev.mcp.example.com/mcp/``.
        public_base_url: Origin of this server as clients reach it, derived
            from ``mcp_resource_url`` unless set explicitly.
        cognito_issuer: Cognito user-pool issuer URL (the JWT ``iss``).
        cognito_domain: Cognito hosted-UI base URL (authorize/token endpoints).
        allowed_client_ids: App-client ids accepted in the ``client_id`` claim.
        auth_metadata_mode: ``direct`` advertises Cognito as the authorization
            server; ``fronted`` advertises this host, which serves patched
            RFC 8414 metadata adding ``code_challenge_methods_supported``
            (Cognito omits it and some clients then fail PKCE - PRD R2).
        require_origin_secret: Enforce the CloudFront origin-secret header.
        origin_secret: Expected ``X-Origin-Secret`` value, or ``None``.
        break_glass_enabled: Whether the static bearer fallback is active.
        break_glass_sha256: Hex SHA-256 of the break-glass token, or ``None``.
        break_glass_scopes: Scopes granted to a break-glass caller.
    """

    stage: str = _DEFAULT_STAGE
    aws_region: str = _DEFAULT_REGION
    mcp_resource_url: str = "http://127.0.0.1:8000/mcp/"
    public_base_url: str = "http://127.0.0.1:8000"
    cognito_issuer: str = ""
    cognito_domain: str = ""
    allowed_client_ids: tuple[str, ...] = ()
    auth_metadata_mode: str = "direct"
    require_origin_secret: bool = False
    origin_secret: str | None = None
    break_glass_enabled: bool = False
    break_glass_sha256: str | None = None
    break_glass_scopes: tuple[str, ...] = ("msg/read",)

    @property
    def jwks_url(self) -> str:
        """URL of the Cognito JWKS document for this issuer."""
        return f"{self.cognito_issuer}/.well-known/jwks.json"

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Settings:
        """Build a ``Settings`` instance from environment variables.

        Args:
            env: Variable mapping to read from; defaults to ``os.environ``.

        Returns:
            A populated ``Settings`` instance.
        """
        source: Mapping[str, str] = os.environ if env is None else env
        resource_url = source.get("MCP_RESOURCE_URL", "http://127.0.0.1:8000/mcp/")
        parsed = urlsplit(resource_url)
        default_base = f"{parsed.scheme}://{parsed.netloc}"
        return cls(
            stage=source.get("STAGE", _DEFAULT_STAGE),
            aws_region=source.get("AWS_REGION", _DEFAULT_REGION),
            mcp_resource_url=resource_url,
            public_base_url=source.get("PUBLIC_BASE_URL", default_base),
            cognito_issuer=source.get("COGNITO_ISSUER", ""),
            cognito_domain=source.get("COGNITO_DOMAIN", ""),
            allowed_client_ids=_split_csv(source.get("ALLOWED_CLIENT_IDS", "")),
            auth_metadata_mode=source.get("AUTH_METADATA_MODE", "direct"),
            require_origin_secret=source.get("REQUIRE_ORIGIN_SECRET", "false").lower() == "true",
            origin_secret=source.get("ORIGIN_SECRET") or None,
            break_glass_enabled=source.get("BREAK_GLASS_ENABLED", "false").lower() == "true",
            break_glass_sha256=source.get("BREAK_GLASS_SHA256") or None,
            break_glass_scopes=_split_csv(source.get("BREAK_GLASS_SCOPES", "msg/read")),
        )


def resolve_origin_secret(
    settings: Settings,
    env: Mapping[str, str] | None = None,
    fetch: Callable[[str], str] | None = None,
) -> Settings:
    """Return settings with ``origin_secret`` loaded from SSM if referenced.

    When ``ORIGIN_SECRET_PARAM`` names an SSM parameter and no literal secret
    is set, fetch it once (cold start) with decryption. The Lambda execution
    role only holds ``ssm:GetParameter`` on the stack's prefix (PRD S1).

    Args:
        settings: Base settings, typically ``Settings.from_env()``.
        env: Variable mapping to read from; defaults to ``os.environ``.
        fetch: Parameter fetcher taking the parameter name; defaults to an
            SSM ``get_parameter`` call. Injectable for tests.

    Returns:
        ``settings`` unchanged, or a copy with ``origin_secret`` populated.
    """
    source: Mapping[str, str] = os.environ if env is None else env
    param_name = source.get("ORIGIN_SECRET_PARAM", "")
    if settings.origin_secret is not None or not param_name:
        return settings
    if fetch is None:
        fetch = _fetch_ssm_parameter(settings.aws_region)
    from dataclasses import replace

    return replace(settings, origin_secret=fetch(param_name))


def _fetch_ssm_parameter(region: str) -> Callable[[str], str]:
    """Build the default SSM SecureString fetcher for ``region``."""

    def fetch(name: str) -> str:
        import boto3

        client = boto3.client("ssm", region_name=region)
        response = client.get_parameter(Name=name, WithDecryption=True)
        value: str = response["Parameter"]["Value"]
        return value

    return fetch
