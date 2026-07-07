# quickjs-worker-go — durable CodeAct AI agent
#
# Go code is built with the `go` tool (build/test/run below). The ONE artifact
# that `go` cannot build is the QuickJS guest wasm (agent/quickjs_guest.wasm). It
# is COMMITTED so a plain `go build` needs only the Go toolchain; rebuild it
# deliberately after editing the guest. Two guests exist:
#   - guest-rs/ (Rust / rquickjs)  → `make guest-rs`  — the ACTIVE guest (reuse-ready,
#     Vec pending, smaller binary). Needs the Rust toolchain + wasm32-wasip1 target.
#   - guest/    (C, via Docker)    → `make guest`     — the original, now superseded;
#     kept for reference. Both produce a byte-identical ABI.
# Rebuilding is intentionally NOT a dependency of `build` (git doesn't preserve mtimes).

GUEST_WASM := agent/quickjs_guest.wasm

.PHONY: help build test vet fmt tidy run guest guest-rs clean

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

guest-rs: ## Rebuild $(GUEST_WASM) from guest-rs/ (Rust/rquickjs) — the ACTIVE guest
	cd guest-rs && cargo build --release --target wasm32-wasip1
	cp guest-rs/target/wasm32-wasip1/release/quickjs_guest_rs.wasm $(GUEST_WASM)
	@echo "rebuilt $(GUEST_WASM) ($$(wc -c < $(GUEST_WASM)) bytes)"

guest: ## Rebuild $(GUEST_WASM) from guest/guest.c via Docker (SUPERSEDED by guest-rs)
	docker build -f guest/Dockerfile.build -t qjs-guest-go guest/
	docker run --rm qjs-guest-go cat /quickjs_guest.wasm > $(GUEST_WASM)
	@echo "rebuilt $(GUEST_WASM) ($$(wc -c < $(GUEST_WASM)) bytes)"

clean: ## Remove build artifacts
	rm -rf bin
	go clean
