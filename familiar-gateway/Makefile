BINARY := familiar-gateway

.PHONY: build run test test-integration test-notes-migration tidy

build:
	go build -o $(BINARY) ./cmd/gateway/

run:
	go run ./cmd/gateway/

test:
	go test ./...

# Integration tests opt in via FAMILIAR_TEST_DSN. The default `test`
# target stays hermetic (no DB required); this target wires the DSN
# through and runs every package — integration-tagged tests in
# internal/notesmigration, internal/shards, internal/memory, etc.
# pick it up via internal/testutil.PgTestPool. -count=1 disables the
# go test result cache so the SQL paths actually re-run.
#
# Usage (devops agent supplies the DSN):
#   FAMILIAR_TEST_DSN='postgres://user:pass@host:5432/db?sslmode=disable' \
#       make test-integration
test-integration:
	@if [ -z "$$FAMILIAR_TEST_DSN" ]; then \
		echo "FAMILIAR_TEST_DSN is required for test-integration"; \
		echo "Example: FAMILIAR_TEST_DSN='postgres://localhost/familiar_test?sslmode=disable' make test-integration"; \
		exit 2; \
	fi
	go test -count=1 ./...

# notes-migrate-only variant — same DSN gate, scoped to the
# migration package. Faster feedback while iterating on Phase 1
# step 5 work; doesn't drag the full integration suite.
test-notes-migration:
	@if [ -z "$$FAMILIAR_TEST_DSN" ]; then \
		echo "FAMILIAR_TEST_DSN is required"; exit 2; \
	fi
	go test -count=1 -v ./internal/notesmigration/

tidy:
	go mod tidy
