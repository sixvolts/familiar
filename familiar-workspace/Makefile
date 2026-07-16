BINARY := familiar-workspace
MODULE := github.com/familiar/workspace

.PHONY: build run test tidy clean

# Default: build the workspace binary in repo root.
build:
	go build -o $(BINARY) ./cmd/workspace/

# Run from repo root using ./workspace.toml (or defaults if missing).
# For a quick smoke test:
#   make run
#   curl http://localhost:3000/console/api/auth/status
run: build
	./$(BINARY)

test:
	go test ./...

tidy:
	go mod tidy

clean:
	rm -f $(BINARY)
