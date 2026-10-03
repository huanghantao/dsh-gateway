# dsh-gateway — Makefile
#
# The frontend is built first because the Go binary embeds its output: `go build`
# fails outright if web/dist is missing, which is deliberate (a binary that serves
# a blank page is harder to diagnose than a build that stops).

SHELL := /bin/bash
.DEFAULT_GOAL := help

BINARY      := dsh-gateway
CMD         := ./cmd/dsh-gateway
# The agent host is a second binary, not a mode of the first: it is a peer
# process with its own launchd job, because a host spawned by the gateway would
# be killed by the gateway's own redeploy.
HOST_BINARY := dsh-agent-host
HOST_CMD    := ./cmd/dsh-agent-host
BIN_DIR     := bin
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
GOFLAGS     := -trimpath

WEB_DIR     := web
WEB_STAMP   := $(WEB_DIR)/dist/.build-stamp

.PHONY: help
help: ## Show this help.
	@grep -hE '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) \
		| awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

# --- build ------------------------------------------------------------------

$(WEB_STAMP): $(shell find $(WEB_DIR)/src $(WEB_DIR)/index.html -type f 2>/dev/null)
	@echo "==> building the web app"
	@# `npm ci`, not `npm install`: the lockfile is committed and the build should
	@# use exactly what it pins. `npm install` would silently update the lock when
	@# package.json and it disagree, which turns a dependency bump into a diff
	@# nobody reviewed.
	@cd $(WEB_DIR) && { [ -d node_modules ] || npm ci --no-audit --no-fund --silent; } && npm run build --silent
	@touch $@

.PHONY: web
web: $(WEB_STAMP) ## Build the embedded web app.

.PHONY: build
build: web ## Build both binaries into bin/.
	@echo "==> building $(BINARY) $(VERSION)"
	@mkdir -p $(BIN_DIR)
	@go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(BINARY) $(CMD)
	@go build $(GOFLAGS) -ldflags '$(LDFLAGS)' -o $(BIN_DIR)/$(HOST_BINARY) $(HOST_CMD)
	@echo "    $(BIN_DIR)/$(BINARY)  $$(du -h $(BIN_DIR)/$(BINARY) | cut -f1)"
	@echo "    $(BIN_DIR)/$(HOST_BINARY)  $$(du -h $(BIN_DIR)/$(HOST_BINARY) | cut -f1)"

.PHONY: install
install: build ## Install the binary into GOBIN (or GOPATH/bin).
	@go install $(GOFLAGS) -ldflags '$(LDFLAGS)' $(CMD)
	@echo "    installed $$(go env GOPATH)/bin/$(BINARY)"

# --- quality ----------------------------------------------------------------

.PHONY: test
test: web ## Run the test suite.
	@go test ./... -timeout 300s

.PHONY: test-race
test-race: web ## Run the test suite under the race detector.
	@go test -race ./... -timeout 600s

.PHONY: test-llm
test-llm: web ## Run the opt-in tests that make a real model call (costs money).
	@DSH_GATEWAY_TEST_LLM=1 go test ./internal/harness/acp/ -run TestPromptEndToEnd -v -timeout 600s

.PHONY: e2e
e2e: ## Drive the real PWA in real Chrome against a running gateway (needs one running).
	@command -v node >/dev/null || { echo "node is required"; exit 1; }
	@node scripts/e2e-browser.mjs

.PHONY: e2e-host
e2e-host: ## Build both binaries and exercise the two-tier deployment surface.
	@bash scripts/e2e-agent-host.sh

.PHONY: cover
cover: web ## Write a coverage profile and print the total.
	@go test ./... -coverprofile=coverage.out -covermode=atomic -timeout 300s >/dev/null
	@go tool cover -func=coverage.out | tail -1

.PHONY: vet
vet: web ## Run go vet.
	@go vet ./...

.PHONY: fmt
fmt: web ## Format Go sources and type-check the frontend.
	@gofmt -w cmd internal web
	@cd $(WEB_DIR) && npx tsc --noEmit

.PHONY: lint
lint: web ## Run golangci-lint if it is installed.
	@command -v golangci-lint >/dev/null || { \
		echo "golangci-lint is not installed; see https://golangci-lint.run/usage/install/"; exit 1; }
	@golangci-lint run ./...

.PHONY: check
check: fmt vet test ## Format, vet, and test. The fast local loop.
	@echo "note: CI also runs golangci-lint, shellcheck, and a build; run \`make lint build\` too"

.PHONY: check-ci
check-ci: check lint build ## Everything CI runs, when the same tools are installed.

# --- operations -------------------------------------------------------------

.PHONY: run
run: build ## Build and run against a local state directory.
	@# -workspace is not optional: the gateway refuses to start without at least
	@# one allowlisted root, and defaulting it would decide what this dev instance
	@# may reach. For a dev run the repository itself is the honest answer.
	@./$(BIN_DIR)/$(BINARY) run -state-dir ./.dev-state -log-level debug \
		-workspace "$(CURDIR)"

.PHONY: pair
pair: build ## Print a pairing link and QR code for the local dev instance.
	@./$(BIN_DIR)/$(BINARY) pair -state-dir ./.dev-state \
		-workspace "$(CURDIR)"

.PHONY: clean
clean: ## Remove build output.
	@rm -rf $(BIN_DIR) coverage.out $(WEB_DIR)/dist .dev-state
	@echo "cleaned"
