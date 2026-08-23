"""FastAPI application: auth middleware, OAuth metadata routes, FastMCP mount.

Request path (PRD 4.1): origin-secret check first, then bearer verification
(break-glass hash, else Cognito JWT), then FastMCP dispatch where each tool
enforces its scope. Unauthenticated requests get ``401`` with the RFC 9728
``WWW-Authenticate`` pointer (PRD A4).
"""

from __future__ import annotations

import contextlib
from collections.abc import AsyncIterator, Awaitable, Callable
from contextvars import ContextVar

from fastapi import FastAPI, Request, Response
from fastapi.responses import JSONResponse
from mcp.server.mcpserver import MCPServer
from mcp.server.transport_security import TransportSecuritySettings

from aws_messaging_mcp import __version__
from aws_messaging_mcp.auth.breakglass import verify_break_glass
from aws_messaging_mcp.auth.jwt import AuthError, KeyResolver, TokenVerifier
from aws_messaging_mcp.auth.origin import origin_secret_ok
from aws_messaging_mcp.auth.principal import Principal
from aws_messaging_mcp.auth.scopes import require_scope
from aws_messaging_mcp.settings import SCOPES_SUPPORTED, Settings, resolve_origin_secret

_current_principal: ContextVar[Principal | None] = ContextVar("principal", default=None)

#: Paths served without bearer auth (still behind the origin-secret check).
_AUTH_EXEMPT_PREFIXES = ("/.well-known/",)
_AUTH_EXEMPT_PATHS = ("/healthz",)


def current_principal() -> Principal:
    """Return the authenticated caller for the request being served.

    Returns:
        The principal the auth middleware attached.

    Raises:
        RuntimeError: If called outside an authenticated request context.
    """
    principal = _current_principal.get()
    if principal is None:
        raise RuntimeError("no authenticated principal in context")
    return principal


def create_app(settings: Settings, key_resolver: KeyResolver | None = None) -> FastAPI:
    """Build the FastAPI application.

    Args:
        settings: Server configuration.
        key_resolver: JWT signing-key resolver override; tests inject a local
            RSA key here instead of fetching the Cognito JWKS.

    Returns:
        The configured application with FastMCP mounted at ``/mcp``.
    """
    verifier = TokenVerifier(
        issuer=settings.cognito_issuer,
        allowed_client_ids=settings.allowed_client_ids,
        key_resolver=key_resolver,
    )

    mcp = MCPServer("aws-messaging-mcp")

    @mcp.tool()
    def hello(name: str = "world") -> dict[str, str]:
        """Verify the full auth chain end to end.

        Args:
            name: Whom to greet.

        Returns:
            A greeting plus the stage and authenticated caller identity.
        """
        principal = current_principal()
        require_scope(principal, "msg/read")
        return {
            "message": f"Hello, {name}!",
            "stage": settings.stage,
            "caller": principal.subject,
            "auth_method": principal.method,
        }

    # Stateless JSON responses only (PRD R3); host-header (DNS-rebinding)
    # protection is disabled because CloudFront terminates the public
    # hostname - the origin secret and bearer auth are the real gate.
    mcp_app = mcp.streamable_http_app(
        streamable_http_path="/",
        json_response=True,
        stateless_http=True,
        transport_security=TransportSecuritySettings(enable_dns_rebinding_protection=False),
    )

    @contextlib.asynccontextmanager
    async def lifespan(_: FastAPI) -> AsyncIterator[None]:
        """Run the MCP session manager for the app's lifetime."""
        async with mcp.session_manager.run():
            yield

    app = FastAPI(
        title="aws-messaging-mcp",
        version=__version__,
        lifespan=lifespan,
        docs_url=None,
        redoc_url=None,
        openapi_url=None,
    )

    @app.middleware("http")
    async def auth_middleware(
        request: Request,
        call_next: Callable[[Request], Awaitable[Response]],
    ) -> Response:
        """Enforce origin secret and bearer auth before any handler runs."""
        provided = request.headers.get("x-origin-secret")
        if not origin_secret_ok(provided, settings.origin_secret, settings.require_origin_secret):
            return JSONResponse({"error": "forbidden"}, status_code=403)

        path = request.url.path
        if path in _AUTH_EXEMPT_PATHS or path.startswith(_AUTH_EXEMPT_PREFIXES):
            return await call_next(request)

        principal = _authenticate(request.headers.get("authorization"))
        if principal is None:
            return _unauthorized(settings)
        token = _current_principal.set(principal)
        try:
            return await call_next(request)
        finally:
            _current_principal.reset(token)

    def _authenticate(authorization: str | None) -> Principal | None:
        """Resolve the bearer token to a principal, or ``None``."""
        if authorization is None or not authorization.lower().startswith("bearer "):
            return None
        bearer = authorization[len("bearer ") :].strip()
        if not bearer:
            return None
        if settings.break_glass_enabled and settings.break_glass_sha256 is not None:
            principal = verify_break_glass(
                bearer, settings.break_glass_sha256, settings.break_glass_scopes
            )
            if principal is not None:
                return principal
        try:
            return verifier.verify(bearer)
        except AuthError:
            return None

    @app.get("/healthz")
    def healthz() -> dict[str, str]:
        """Liveness probe."""
        return {"status": "ok", "stage": settings.stage}

    @app.get("/.well-known/oauth-protected-resource")
    @app.get("/.well-known/oauth-protected-resource/mcp")
    def protected_resource() -> dict[str, object]:
        """RFC 9728 protected-resource metadata (PRD A5)."""
        return _protected_resource_doc(settings)

    @app.get("/.well-known/oauth-authorization-server")
    @app.get("/.well-known/oauth-authorization-server/oauth")
    def authorization_server() -> dict[str, object]:
        """RFC 8414 metadata mirroring Cognito, plus the PKCE field.

        Cognito omits ``code_challenge_methods_supported`` from its own
        discovery document and some clients then fail PKCE silently (PRD R2);
        in ``fronted`` mode the protected-resource metadata points here.
        """
        return _authorization_server_doc(settings)

    app.mount("/mcp", mcp_app)
    return app


def _unauthorized(settings: Settings) -> JSONResponse:
    """Build the 401 response with the RFC 9728 pointer (PRD A4)."""
    resource_metadata = f"{settings.public_base_url}/.well-known/oauth-protected-resource"
    return JSONResponse(
        {"error": "unauthorized"},
        status_code=401,
        headers={"WWW-Authenticate": f'Bearer resource_metadata="{resource_metadata}"'},
    )


def _protected_resource_doc(settings: Settings) -> dict[str, object]:
    """Protected-resource metadata, honoring ``auth_metadata_mode``."""
    if settings.auth_metadata_mode == "fronted":
        authorization_server = f"{settings.public_base_url}/oauth"
    else:
        authorization_server = settings.cognito_issuer
    return {
        "resource": settings.mcp_resource_url,
        "authorization_servers": [authorization_server],
        "scopes_supported": list(SCOPES_SUPPORTED),
        "bearer_methods_supported": ["header"],
    }


def _authorization_server_doc(settings: Settings) -> dict[str, object]:
    """Authorization-server metadata built from the Cognito endpoints."""
    return {
        "issuer": settings.cognito_issuer,
        "authorization_endpoint": f"{settings.cognito_domain}/oauth2/authorize",
        "token_endpoint": f"{settings.cognito_domain}/oauth2/token",
        "revocation_endpoint": f"{settings.cognito_domain}/oauth2/revoke",
        "jwks_uri": f"{settings.cognito_issuer}/.well-known/jwks.json",
        "response_types_supported": ["code"],
        "grant_types_supported": ["authorization_code", "refresh_token"],
        "token_endpoint_auth_methods_supported": ["none"],
        "scopes_supported": ["openid", *SCOPES_SUPPORTED],
        "code_challenge_methods_supported": ["S256"],
    }


def dev_app() -> FastAPI:
    """Application factory for ``make dev`` (uvicorn ``--factory``)."""
    return create_app(resolve_origin_secret(Settings.from_env()))
