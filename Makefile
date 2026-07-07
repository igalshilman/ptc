# quickjs-worker-go — durable CodeAct AI agent
#
# Go code is built with the `go` tool (build/test/run below). The ONE artifact
# that `go` cannot build is the QuickJS guest wasm (agent/quickjs_guest.wasm),
# compiled from the Rust guest in guest-rs/ (rquickjs → wasm32-wasip1). It is
# COMMITTED so a plain `go build` needs only the Go toolchain; rebuild it
# deliberately with `make guest-rs` after editing guest-rs/ (needs the Rust
# toolchain + the wasm32-wasip1 target). Rebuilding is intentionally NOT a
# dependency of `build` (git doesn't preserve mtimes).

GUEST_WASM := agent/quickjs_guest.wasm

.PHONY: help build test vet fmt tidy run guest-rs clean

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
	gofmt -w agent examples

tidy: ## Tidy go.mod/go.sum
	go mod tidy

run: ## Run the research example (needs OPENAI_API_KEY; serves :9080)
	go run ./examples/research

guest-rs: ## Rebuild $(GUEST_WASM) from guest-rs/ (Rust/rquickjs → wasm32-wasip1)
	cd guest-rs && cargo build --release --target wasm32-wasip1
	cp guest-rs/target/wasm32-wasip1/release/quickjs_guest_rs.wasm $(GUEST_WASM)
	@echo "rebuilt $(GUEST_WASM) ($$(wc -c < $(GUEST_WASM)) bytes)"

clean: ## Remove build artifacts
	rm -rf bin
	go clean
