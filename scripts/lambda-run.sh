#!/bin/bash
# Entrypoint executed by the Lambda Web Adapter (AWS_LAMBDA_EXEC_WRAPPER):
# starts the ASGI app; settings come from environment variables and SSM.
exec python -m uvicorn aws_messaging_mcp.main:app_from_env --factory \
  --host 0.0.0.0 --port "${PORT:-8000}"
