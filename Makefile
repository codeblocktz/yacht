.DEFAULT_GOAL := help
GO ?= go
BIN := bin/yacht

# The Tailwind standalone CLI is a single binary with no Node dependency.
# `make tailwind` fetches it into ./bin so a contributor needs nothing
# installed beyond Go.
TAILWIND_VERSION ?= v4.3.3
TAILWIND ?= bin/tailwindcss
UNAME_S := $(shell uname -s | tr '[:upper:]' '[:lower:]')
UNAME_M := $(shell uname -m)
ifeq ($(UNAME_S),darwin)
	TW_OS := macos
else
	TW_OS := $(UNAME_S)
endif
ifeq ($(UNAME_M),x86_64)
	TW_ARCH := x64
else
	TW_ARCH := arm64
endif
TAILWIND_URL := https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$(TW_OS)-$(TW_ARCH)

CSS_IN  := assets/css/input.css
CSS_OUT := internal/web/assets/css/app.css

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

$(TAILWIND):
	@mkdir -p "$(dir $(TAILWIND))"
	@set -eu; \
		tmp=$$(mktemp "$(TAILWIND).tmp.XXXXXX"); \
		trap 'rm -f "$$tmp"' EXIT HUP INT TERM; \
		curl --fail --silent --show-error --location -o "$$tmp" "$(TAILWIND_URL)"; \
		chmod +x "$$tmp"; \
		mv "$$tmp" "$(TAILWIND)"

.PHONY: tailwind
tailwind: $(TAILWIND) ## Download the Tailwind standalone CLI

.PHONY: css
css: $(TAILWIND) ## Compile the stylesheet
	$(TAILWIND) -i $(CSS_IN) -o $(CSS_OUT) --minify

.PHONY: css-watch
css-watch: $(TAILWIND) ## Recompile the stylesheet on change
	$(TAILWIND) -i $(CSS_IN) -o $(CSS_OUT) --watch

.PHONY: generate
generate: ## Run templ codegen
	$(GO) tool templ generate

.PHONY: assets
assets: generate css ## Regenerate templates and stylesheet

.PHONY: generated-check
generated-check: assets ## Reject stale generated templates or styles
	@git diff --exit-code -- '*_templ.go' $(CSS_OUT) \
		|| { echo "::error::generated output is stale — run 'make assets' and commit"; exit 1; }

.PHONY: gallery
gallery: assets ## Render every visual state to HTML
	YACHT_GALLERY_OUT=$(or $(OUT),/tmp/yacht-gallery) $(GO) test ./internal/web -run Gallery -count=1
	@count=$$(find "$(or $(OUT),/tmp/yacht-gallery)" -maxdepth 1 -type f -name '*.html' -print 2>/dev/null \
		| wc -l | tr -d '[:space:]'); \
	if [ "$$count" -eq 0 ]; then \
		echo "::error::gallery rendered no HTML files"; \
		exit 1; \
	fi
	@echo
	@echo "  Serve it — the pages ask for /assets, so file:// renders unstyled:"
	@echo "    cd $(or $(OUT),/tmp/yacht-gallery) && python3 -m http.server 8123"

.PHONY: build
build: assets ## Build the yacht binary
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/yacht

.PHONY: test
test: ## Run tests
	$(GO) test ./... -race -count=1

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: require-test-database
require-test-database:
	@if [ -z "$$YACHT_TEST_DATABASE_URL" ] && [ "$$YACHT_ALLOW_DATABASE_TEST_SKIPS" != "1" ]; then \
		echo "::error::YACHT_TEST_DATABASE_URL is required because database security tests otherwise skip."; \
		echo "Set YACHT_ALLOW_DATABASE_TEST_SKIPS=1 only when an intentionally partial run is acceptable."; \
		exit 1; \
	fi

.PHONY: check
check: require-test-database vet test ## Vet + database-backed race tests

.PHONY: shell-check
shell-check: ## Lint the root-executed installer scripts
	shellcheck -s sh install.sh upgrade.sh
	sh -n install.sh
	sh -n upgrade.sh

.PHONY: boundary-check
boundary-check: ## Reject commercial-layer declarations in the engine
	@# internal/web/ui is vendored templUI; its Lucide Wallet icon is not billing.
	@if grep -rnE '^[[:space:]]*(type|func|var|const)[[:space:]]+[A-Za-z_]*(Tenant|tenant|Wallet|wallet|Billing|billing|Invoice|invoice|Subscription)' \
		--include='*.go' --exclude-dir=ui ./cmd ./internal 2>/dev/null; then \
		echo "::error::engine declares a commercial-layer concept — it belongs in the wrapping layer"; \
		exit 1; \
	fi
	@echo "boundary clean: no commercial declarations in engine code"

.PHONY: build-check
build-check: ## Compile every Go package without CGO
	CGO_ENABLED=0 $(GO) build ./...

# Keep these dependency graphs serial even when a caller has MAKEFLAGS=-j.
# `verify` then executes each expensive gate once, while `assets` remains shared
# by generated-check and gallery within the same Make invocation.
.NOTPARALLEL: verify check assets

.PHONY: verify
verify: require-test-database generated-check shell-check check boundary-check gallery build-check ## Run every CI/release gate

.PHONY: run
run: build ## Run locally
	$(BIN)

.PHONY: dev
dev: assets ## Rebuild and run
	$(GO) run ./cmd/yacht

.PHONY: tidy
tidy: ## Tidy modules
	$(GO) mod tidy

.PHONY: sqlc
sqlc: ## Regenerate database code from SQL
	$(GO) tool sqlc generate

.PHONY: clean
clean: ## Remove build artifacts
	rm -rf bin dist
