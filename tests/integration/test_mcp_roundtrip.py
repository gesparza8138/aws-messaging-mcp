"""MCP protocol round-trips through the full auth middleware."""

from typing import Any

import pytest
from mcp import Client
from mcp.client.streamable_http import streamable_http_client
from mcp.shared._httpx_utils import create_mcp_http_client

from aws_messaging_mcp.settings import Settings
from tests.integration.conftest import BREAK_GLASS_TOKEN, ORIGIN_SECRET

pytestmark = pytest.mark.anyio

Server = tuple[str, Settings]


def _headers(token: str) -> dict[str, str]:
    return {"Authorization": f"Bearer {token}", "X-Origin-Secret": ORIGIN_SECRET}


async def _call_hello(base_url: str, token: str, name: str = "Gabe") -> Any:
    http_client = create_mcp_http_client(headers=_headers(token))
    transport = streamable_http_client(f"{base_url}/mcp/", http_client=http_client)
    async with Client(transport) as client:
        tools = await client.list_tools()
        assert [tool.name for tool in tools.tools] == ["hello"]
        return await client.call_tool("hello", {"name": name})


async def test_oauth_token_roundtrip(server: Server, mint_token: Any) -> None:
    base_url, _ = server
    result = await _call_hello(base_url, mint_token())
    assert result.is_error is False
    assert result.structured_content == {
        "message": "Hello, Gabe!",
        "stage": "test",
        "caller": "integration-user",
        "auth_method": "oauth",
    }


async def test_break_glass_token_roundtrip(server: Server) -> None:
    base_url, _ = server
    result = await _call_hello(base_url, BREAK_GLASS_TOKEN)
    assert result.is_error is False
    assert result.structured_content is not None
    assert result.structured_content["auth_method"] == "break_glass"
    assert result.structured_content["caller"] == "break-glass"


async def test_missing_scope_is_tool_error_not_401(server: Server, mint_token: Any) -> None:
    base_url, _ = server
    result = await _call_hello(base_url, mint_token(scope="msg/email:send"))
    assert result.is_error is True
    text = "".join(block.text for block in result.content if hasattr(block, "text"))
    assert "msg/read" in text
