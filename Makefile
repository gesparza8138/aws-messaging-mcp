.PHONY: dev test lint typecheck iac-lint e2e deploy-dev

dev:
	uv run uvicorn aws_messaging_mcp.main:app_from_env --factory --port 8000 --reload

test:
	uv run pytest tests/unit tests/integration --cov --cov-report=term-missing --cov-report=json
	uv run python scripts/check_coverage.py coverage.json --require-100 src/aws_messaging_mcp/auth

lint:
	uv run ruff check .
	uv run ruff format --check .

typecheck:
	uv run mypy

iac-lint:
	@if ! ls infra/*.yaml >/dev/null 2>&1; then echo "no CloudFormation templates in infra/ yet"; exit 0; fi
	uv run cfn-lint infra/*.yaml
	uvx checkov --directory infra --framework cloudformation --compact
	@if command -v cfn_nag_scan >/dev/null 2>&1; then \
		cfn_nag_scan --input-path infra --template-pattern '\.yaml$$'; \
	else \
		echo "cfn_nag_scan not installed (gem install cfn-nag); skipped locally, enforced in CI"; \
	fi

e2e:
	@echo "E2E tests arrive with M2 and need a deployed dev stack."
	@exit 1

deploy-dev:
	@echo "deploy-dev arrives with M1 (PR-4) and always asks for confirmation first."
	@exit 1
