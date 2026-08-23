.PHONY: dev build test lint vet iac-lint e2e deploy-dev

dev:
	STAGE=local go run ./cmd/server --listen :8000

build:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o dist/bootstrap ./cmd/server

test:
	go test -race -coverpkg=./internal/... -coverprofile=coverage.out -covermode=atomic ./...
	./scripts/check_coverage.sh coverage.out internal/auth/ internal/guardrails/

lint:
	test -z "$$(gofmt -l .)" || { gofmt -l .; echo "gofmt: files need formatting"; exit 1; }
	golangci-lint run ./...

vet:
	go vet ./...

iac-lint:
	@if ! ls infra/*.yaml >/dev/null 2>&1; then echo "no CloudFormation templates in infra/ yet"; exit 0; fi
	uvx cfn-lint infra/*.yaml
	uvx checkov --directory infra --framework cloudformation --compact
	@if command -v cfn_nag_scan >/dev/null 2>&1; then \
		cfn_nag_scan --input-path infra --template-pattern '\.yaml$$'; \
	else \
		echo "cfn_nag_scan not installed (gem install cfn-nag); skipped locally, enforced in CI"; \
	fi

e2e:
	@test -n "$$E2E_BASE_URL" || { echo "set the E2E_* environment first (docs/testing.md)"; exit 1; }
	go test -tags e2e -count=1 -v ./e2e/

deploy-dev:
	@echo "Deploy dev with: gh workflow run deploy-dev.yml (preview) then -f execute=true"
	@exit 1
