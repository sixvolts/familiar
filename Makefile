# Familiar — convenience targets. See DEPLOYMENT.md, and
# tests/e2e/MAKE_TESTS.md for the E2E harness.
#
# Test layering:
#   make test              Go tests, no services needed        (~2s)
#   make test-integration  + the DB-backed ones                (needs a DSN)
#   make test-e2e          Playwright against a real gateway   (needs a DSN)
#   make test-all          all three
.PHONY: build build-gateway build-workspace \
	test test-integration test-e2e test-all e2e-setup help

help: ## Show this help
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

build: build-gateway build-workspace ## Build both binaries

build-gateway: ## Build the gateway binary
	cd familiar-gateway && go build -o familiar-gateway ./cmd/gateway/

build-workspace: ## Build the workspace binary
	cd familiar-workspace && go build -o familiar-workspace ./cmd/workspace/

# -count=1 on every target, deliberately. Some tests read files OUTSIDE
# their Go module — the prompt/tool-name guard walks ../../prompts — and
# Go's test cache does not key on those, so a prompt-only edit can replay
# a stale PASS. The whole Go suite is ~2s cold, so there is nothing to
# save by caching it.
GOTEST := go test -count=1 ./...

test: ## Go tests for both modules (no DB, no services)
	cd familiar-gateway && $(GOTEST)
	cd familiar-workspace && $(GOTEST)

# The DB-gated tests skip themselves when FAMILIAR_TEST_DSN is unset,
# which means `make test` silently covers less than it looks like it
# does. This target refuses to run rather than skip quietly.
test-integration: guard-dsn ## Go tests incl. DB-backed (needs FAMILIAR_TEST_DSN)
	cd familiar-gateway && $(GOTEST)
	cd familiar-workspace && $(GOTEST)

e2e-setup: ## Install E2E node deps (and browsers where available)
	cd tests/e2e && npm install
	@# Playwright ships no browser build for some hosts (Ubuntu 26.04 among
	@# them). Try, but do not fail the target — test-e2e falls back to a
	@# system Chromium, which is the supported path there.
	-cd tests/e2e && npx playwright install --with-deps chromium

# CHROMIUM is only consulted when Playwright has no browser of its own.
# Override to pin a specific binary.
CHROMIUM ?= $(shell command -v chromium-browser 2>/dev/null || command -v chromium 2>/dev/null)

test-e2e: guard-dsn ## Playwright E2E (needs FAMILIAR_TEST_DSN)
	@if [ ! -d tests/e2e/node_modules ]; then \
		echo "e2e deps missing — run: make e2e-setup"; exit 2; \
	fi
	@if [ -z "$$PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH" ] && [ ! -d "$$HOME/.cache/ms-playwright" ]; then \
		if [ -z "$(CHROMIUM)" ]; then \
			echo "No Playwright browser and no system Chromium found."; \
			echo "Install one (apt install chromium-browser) or set"; \
			echo "PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH to a Chromium binary."; \
			exit 2; \
		fi; \
		echo "Using system Chromium: $(CHROMIUM)"; \
	fi
	@cd tests/e2e && \
		PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH="$${PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH:-$$([ -d "$$HOME/.cache/ms-playwright" ] || echo '$(CHROMIUM)')}" \
		npx playwright test $(E2E_ARGS)

test-all: test-integration test-e2e ## Everything: Go + DB-backed + E2E

# guard-dsn fails loudly instead of letting the DB-backed tests skip
# themselves into a green run that proved nothing.
#
# IMPORTANT: point this at a database you are willing to have mutated.
# The E2E suite TRUNCATEs the auth tables (users CASCADE) between specs.
# The cheapest way to stay isolated on a shared Postgres is a dedicated
# schema rather than a dedicated database:
#
#   psql -c 'CREATE SCHEMA IF NOT EXISTS e2e_test'
#   export FAMILIAR_TEST_DSN='postgresql://user:pw@localhost:5432/db?sslmode=disable&options=-csearch_path%3De2e_test%2Cpublic'
#
# public stays on the search_path so the pgvector `vector` type resolves.
guard-dsn:
	@if [ -z "$$FAMILIAR_TEST_DSN" ]; then \
		echo "FAMILIAR_TEST_DSN is required for this target."; \
		echo "Point it at a throwaway database or schema — these tests mutate it."; \
		echo "See the guard-dsn comment in the Makefile for the schema-isolation recipe."; \
		exit 2; \
	fi
