"""Runtime settings for the aws-messaging-mcp server.

Settings are resolved from environment variables so the same code runs
unchanged on Lambda (variables set by CloudFormation) and locally (variables
set by the shell or ``make dev``).
"""

from __future__ import annotations

import os
from collections.abc import Mapping
from dataclasses import dataclass

_DEFAULT_STAGE = "dev"
_DEFAULT_REGION = "us-west-2"


@dataclass(frozen=True, slots=True)
class Settings:
    """Immutable server configuration.

    Attributes:
        stage: Deployment stage, ``dev`` or ``prod``.
        aws_region: AWS region the server operates in.
    """

    stage: str = _DEFAULT_STAGE
    aws_region: str = _DEFAULT_REGION

    @classmethod
    def from_env(cls, env: Mapping[str, str] | None = None) -> Settings:
        """Build a ``Settings`` instance from environment variables.

        Args:
            env: Variable mapping to read from; defaults to ``os.environ``.

        Returns:
            A populated ``Settings`` instance.
        """
        source: Mapping[str, str] = os.environ if env is None else env
        return cls(
            stage=source.get("STAGE", _DEFAULT_STAGE),
            aws_region=source.get("AWS_REGION", _DEFAULT_REGION),
        )
