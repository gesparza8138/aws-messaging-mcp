"""Authentication and authorization for the MCP server.

The middleware chain (PRD 4.1 step 5) runs in this order: origin-secret
check, then bearer verification (break-glass hash or Cognito JWT), then
per-tool scope enforcement.
"""

from aws_messaging_mcp.auth.principal import Principal

__all__ = ["Principal"]
