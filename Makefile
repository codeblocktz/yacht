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
ifeq ($(UNAME_M),x86_64)
	TW_ARCH := x64
else
	TW_ARCH := arm64
endif
TAILWIND_URL := https://github.com/tailwindlabs/tailwindcss/releases/download/$(TAILWIND_VERSION)/tailwindcss-$(UNAME_S)-$(TW_ARCH)

CSS_IN  := assets/css/input.css
CSS_OUT := internal/web/assets/css/app.css

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

$(TAILWIND):
	@mkdir -p bin
	curl -sSL -o $(TAILWIND) $(TAILWIND_URL)
	chmod +x $(TAILWIND)

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

.PHONY: gallery
gallery: assets ## Render every visual state to HTML
	YACHT_GALLERY_OUT=$(or $(OUT),/tmp/yacht-gallery) $(GO) test ./internal/web -run Gallery -count=1

.PHONY: build
build: assets ## Build the yacht binary
	CGO_ENABLED=0 $(GO) build -o $(BIN) ./cmd/yacht

.PHONY: test
test: ## Run tests
	$(GO) test ./... -race -count=1

.PHONY: vet
vet: ## Run go vet
	$(GO) vet ./...

.PHONY: check
check: vet test ## Vet + test

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
