# Familiar — convenience targets (pure Go; no Node/npm). See DEPLOYMENT.md.
# The README screenshot is regenerated in CI (.github/workflows/screenshot.yml),
# not here, so local builds never need Node.
.PHONY: build build-gateway build-workspace test help

help: ## Show this help
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: build-gateway build-workspace ## Build both binaries

build-gateway: ## Build the gateway binary
	cd familiar-gateway && go build -o familiar-gateway ./cmd/gateway/

build-workspace: ## Build the workspace binary
	cd familiar-workspace && go build -o familiar-workspace ./cmd/workspace/

test: ## Run the Go unit tests (gateway) — needs FAMILIAR_TEST_DSN
	cd familiar-gateway && go test ./...
