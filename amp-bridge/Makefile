# amp-bridge quality gates. `make check` is what CI (and you, before handing the
# binary to a live session) should run.

BIN := amp-bridge
GO  ?= go

# Install location. ~/.local/bin is on PATH and, unlike $(go env GOBIN), is not
# pinned to a toolchain version — mise puts GOBIN inside installs/go/<version>/,
# so anything installed there disappears on the next Go upgrade.
PREFIX ?= $(HOME)/.local
BINDIR ?= $(PREFIX)/bin

.DEFAULT_GOAL := check

.PHONY: help
help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | \
		awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Rebuild the binary
	# rm first: overwriting a Mach-O in place invalidates its macOS signature,
	# and the kernel then SIGKILLs it (exit 137) on every later exec.
	rm -f $(BIN)
	$(GO) build -trimpath -o $(BIN) .
	# Smoke test. A signature-invalidated binary dies with exit 137 here rather
	# than silently failing later, where Claude reports only "server not found".
	@./$(BIN) --help >/dev/null || { echo "built binary will not execute"; exit 1; }
	@echo "built ./$(BIN)"

.PHONY: install
install: build ## Install to $(BINDIR) (override with PREFIX=/usr/local)
	mkdir -p $(BINDIR)
	# Same signature trap as above: install(1) may overwrite in place, so unlink
	# the old binary first and let the copy land on a fresh inode.
	rm -f $(BINDIR)/$(BIN)
	install -m 0755 $(BIN) $(BINDIR)/$(BIN)
	@$(BINDIR)/$(BIN) --help >/dev/null || { echo "installed binary will not execute"; exit 1; }
	@echo "installed $(BINDIR)/$(BIN)"
	@command -v $(BIN) >/dev/null 2>&1 || \
		echo "note: $(BINDIR) is not on your PATH — add it to use \`$(BIN)\` bare"

# The directory whose .mcp.json gets the bridge. Defaults to the repo root, one
# level up from this Makefile; point it at any project you want a bridge in:
#   make setup PROJECT=~/Projects/other-app
PROJECT ?= $(CURDIR)/..

.PHONY: setup
setup: install ## Build, install, and register the bridge in $(PROJECT)/.mcp.json
	@$(BINDIR)/$(BIN) init $(PROJECT)

.PHONY: doctor
doctor: ## Diagnose the installed bridge against $(PROJECT)
	@$(BINDIR)/$(BIN) doctor $(PROJECT)

.PHONY: uninstall
uninstall: ## Remove the installed binary
	rm -f $(BINDIR)/$(BIN)
	@echo "removed $(BINDIR)/$(BIN)"

.PHONY: fmt
fmt: ## Format (gci import grouping + gofumpt)
	golangci-lint fmt

.PHONY: fmt-check
fmt-check: ## Fail if anything is unformatted
	golangci-lint fmt --diff

.PHONY: vet
vet: ## go vet, including the integration tier
	$(GO) vet ./...
	$(GO) vet -tags=integration ./...

.PHONY: lint
lint: ## golangci-lint (29 linters, see .golangci.yml)
	golangci-lint run ./...

.PHONY: test
test: ## Unit tests, race detector on
	$(GO) test -race -count=1 ./...

.PHONY: test-integration
test-integration: ## Spawn a real bridge process and drive both ends
	$(GO) test -tags=integration -race -count=1 -timeout 300s ./...

.PHONY: cover
cover: ## Full-tier coverage report
	$(GO) test -tags=integration -covermode=atomic -coverprofile=coverage.out -count=1 ./...
	$(GO) tool cover -func=coverage.out | tail -1
	@echo "html: $(GO) tool cover -html=coverage.out"

.PHONY: tidy
tidy: ## Verify go.mod is tidy
	$(GO) mod tidy -diff

.PHONY: check
check: tidy fmt-check vet lint test test-integration ## Everything above
	@echo "all checks passed"

.PHONY: tools
tools: ## Report tool versions and flag drift from ../.tool-versions
	@command -v golangci-lint >/dev/null || \
		{ echo "golangci-lint missing: run 'mise install' from the repo root"; exit 1; }
	@$(GO) version
	@golangci-lint --version | head -1
	@command -v mise >/dev/null 2>&1 || exit 0; \
	want=$$(mise current go 2>/dev/null); \
	have=$$($(GO) env GOVERSION 2>/dev/null | sed 's/^go//'); \
	if [ -n "$$want" ] && [ "$$want" != "$$have" ]; then \
		echo "warning: go $$have in use, ../.tool-versions pins $$want (run 'mise install')"; \
	fi; \
	wantl=$$(mise current golangci-lint 2>/dev/null); \
	havel=$$(golangci-lint --version 2>/dev/null | sed -n 's/.*version \([0-9.]*\) .*/\1/p'); \
	if [ -n "$$wantl" ] && [ "$$wantl" != "$$havel" ]; then \
		echo "warning: golangci-lint $$havel in use, ../.tool-versions pins $$wantl"; \
	fi

.PHONY: outdated
outdated: ## List dependency updates (there are none by design — see go.mod)
	$(GO) list -u -m all

.PHONY: clean
clean: ## Remove build and coverage artefacts
	rm -f $(BIN) coverage.out
