# quickjs-worker-go — durable CodeAct AI agent
#
# Go code is built with the `go` tool (build/test/run below). The ONE artifact
# that `go` cannot build is the QuickJS guest wasm (agent/quickjs_guest.wasm),
# compiled from C (guest/guest.c) via Docker. That wasm is COMMITTED to the repo
# so a plain `go build` works with only the Go toolchain; rebuild it deliberately
# with `make guest` after editing guest/guest.c. It is intentionally NOT a
# dependency of `build` (Docker-only + git doesn't preserve mtimes).

GUEST_WASM := agent/quickjs_guest.wasm

.PHONY: help build test vet fmt tidy run guest clean

help: ## Show available targets
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-7s\033[0m %s\n", $$1, $$2}'

build: ## Compile everything (uses the committed guest wasm)
	go build ./...

test: ## Run tests
	go test ./...

vet: ## Run go vet
	go vet ./...

fmt: ## Format sources
	gofmt -w agent cmd

tidy: ## Tidy go.mod/go.sum
	go mod tidy

run: ## Run the agent (needs OPENAI_API_KEY; serves :9080)
	go run ./cmd/agent

guest: ## Rebuild $(GUEST_WASM) from guest/guest.c via Docker (run after editing the C)
	docker build -f guest/Dockerfile.build -t qjs-guest-go guest/
	docker run --rm qjs-guest-go cat /quickjs_guest.wasm > $(GUEST_WASM)
	@echo "rebuilt $(GUEST_WASM) ($$(wc -c < $(GUEST_WASM)) bytes)"

clean: ## Remove build artifacts
	rm -rf bin
	go clean
