"""Integration fixtures: a real uvicorn server with an injected test JWKS.

The app runs on a loopback port with a locally generated RSA key standing in
for the Cognito JWKS, so the full HTTP + auth path is exercised offline.
"""

import hashlib
import socket
import threading
import time
from collections.abc import Iterator
from typing import Any

import jwt as pyjwt
import pytest
import uvicorn
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import rsa

from aws_messaging_mcp.main import create_app
from aws_messaging_mcp.settings import Settings

ISSUER = "https://cognito-idp.us-west-2.amazonaws.com/us-west-2_ITPOOL"
CLIENT_ID = "integration-client"
ORIGIN_SECRET = "integration-origin-secret"
BREAK_GLASS_TOKEN = "integration-break-glass-token"


@pytest.fixture(scope="session")
def anyio_backend() -> str:
    """Run async tests on asyncio."""
    return "asyncio"


@pytest.fixture(scope="session")
def keypair() -> tuple[bytes, bytes]:
    """Session RSA keypair standing in for the Cognito signing key."""
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


def _free_port() -> int:
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        port = sock.getsockname()[1]
        assert isinstance(port, int)
        return port


@pytest.fixture(scope="session")
def server(keypair: tuple[bytes, bytes]) -> Iterator[tuple[str, Settings]]:
    """Serve the app on a loopback port for the whole session."""
    port = _free_port()
    base_url = f"http://127.0.0.1:{port}"
    settings = Settings(
        stage="test",
        mcp_resource_url=f"{base_url}/mcp/",
        public_base_url=base_url,
        cognito_issuer=ISSUER,
        cognito_domain="https://auth.test.example.com",
        allowed_client_ids=(CLIENT_ID,),
        auth_metadata_mode="direct",
        require_origin_secret=True,
        origin_secret=ORIGIN_SECRET,
        break_glass_enabled=True,
        break_glass_sha256=hashlib.sha256(BREAK_GLASS_TOKEN.encode()).hexdigest(),
        break_glass_scopes=("msg/read",),
    )
    _, public_pem = keypair
    app = create_app(settings, key_resolver=lambda _token: public_pem)
    config = uvicorn.Config(app, host="127.0.0.1", port=port, log_level="warning")
    uv_server = uvicorn.Server(config)
    thread = threading.Thread(target=uv_server.run, daemon=True)
    thread.start()
    deadline = time.time() + 10
    while not uv_server.started:
        if time.time() > deadline:  # pragma: no cover - startup failure path
            raise RuntimeError("uvicorn did not start in time")
        time.sleep(0.05)
    yield base_url, settings
    uv_server.should_exit = True
    thread.join(timeout=5)


@pytest.fixture(scope="session")
def mint_token(keypair: tuple[bytes, bytes]) -> Any:
    """Factory producing signed access tokens with overridable claims."""
    private_pem, _ = keypair

    def mint(**overrides: Any) -> str:
        claims: dict[str, Any] = {
            "sub": "integration-user",
            "iss": ISSUER,
            "exp": int(time.time()) + 300,
            "token_use": "access",
            "client_id": CLIENT_ID,
            "scope": "msg/read",
        }
        claims.update(overrides)
        return pyjwt.encode(claims, private_pem, algorithm="RS256")

    return mint
