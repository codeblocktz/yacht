.DEFAULT_GOAL := help
GO ?= go

# The engine builds alone. A go.work in a parent directory — the one that
# lets an application wrapping the engine build against this checkout — must
# not leak into the engine's own gates, or a change here could pass only
# because of something beside it.
export GOWORK := off
TESTFLAGS ?=
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

# What each platform's TAILWIND_VERSION binary hashes to, from the release's
# sha256sums.txt. The download is an executable `make verify` runs, so it is
# checked before it lands in ./bin, and a mismatch fails rather than runs.
# Bumping TAILWIND_VERSION means replacing all four; overriding it alone
# fails the check until TAILWIND_SHA256 is given too.
TAILWIND_SHA256_linux-x64   := dc61b3ac6b8c9ca874c0cc4c57b2409791a64c5540404ca5f5367360babc313a
TAILWIND_SHA256_linux-arm64 := 55fd0b241214eff3de1e8ee4f22796662f2d2e7a49bcfca7477cfd0bac398195
TAILWIND_SHA256_macos-x64   := 7922e0953f2110c05976e3bf58f14e643d90427575e766b7d433f5f80cbee7e1
TAILWIND_SHA256_macos-arm64 := cdf646702987a743464dff4d9c60fd4480d1c1e73dd819a9a67f1078815dce9d
TAILWIND_SHA256 ?= $(TAILWIND_SHA256_$(TW_OS)-$(TW_ARCH))

CSS_IN  := assets/css/input.css
CSS_OUT := internal/web/assets/css/app.css

# Disposable Postgres for the tests that need one. Real Postgres binaries,
# downloaded once per version and cached globally, run as an ordinary local
# process on a free port — no Docker and nothing installed system-wide.
#
# Reached through npx because that needs no install step, but it is only a
# launcher: point POPGRES at a native binary and nothing here changes.
POPGRES ?= npx --yes @popgres/cli

.PHONY: help
help: ## Show this help
	@grep -E '^[a-zA-Z_-]+:.*?## ' $(MAKEFILE_LIST) | awk 'BEGIN{FS=":.*?## "}{printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

$(TAILWIND):
	@mkdir -p "$(dir $(TAILWIND))"
	@set -eu; \
		tmp=$$(mktemp "$(TAILWIND).tmp.XXXXXX"); \
		trap 'rm -f "$$tmp"' EXIT HUP INT TERM; \
		want="$(TAILWIND_SHA256)"; \
		if [ -z "$$want" ]; then \
			echo "no checksum for tailwindcss-$(TW_OS)-$(TW_ARCH): set TAILWIND_SHA256" >&2; exit 1; \
		fi; \
		curl --fail --silent --show-error --location -o "$$tmp" "$(TAILWIND_URL)"; \
		if command -v sha256sum >/dev/null 2>&1; then \
			got=$$(sha256sum "$$tmp" | cut -d' ' -f1); \
		else \
			got=$$(shasum -a 256 "$$tmp" | cut -d' ' -f1); \
		fi; \
		if [ "$$got" != "$$want" ]; then \
			echo "tailwindcss-$(TW_OS)-$(TW_ARCH) $(TAILWIND_VERSION): sha256 $$got, want $$want" >&2; exit 1; \
		fi; \
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
# Every package shares one test database, and operation admission/reclaim queues
# are install-global. Package test binaries must not mutate those queues at the
# same time; -p 1 comes after TESTFLAGS so callers cannot override this guard.
test: ## Run tests
	$(GO) test $(TESTFLAGS) -p 1 ./... -race -count=1

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

# popgres exports DATABASE_URL; Yacht reads YACHT_TEST_DATABASE_URL, and the
# two are kept separate deliberately — the test DSN names a database being
# written to and dropped, which is not a name to hand to anything that might be
# pointed at a real install.
#
# The instance is torn down when make exits. An instance already running for
# this directory is reused and left alone, so a `popgres up` you started by
# hand survives.
#
# What popgres runs is make again, in plain words, not sh -c and a quoted
# script. Not every npx is npm's own: npq's npq-hero, which is often aliased
# to npx, joins its arguments into one shell command line, which splits the
# script at its spaces. sh -c then ran only the assignment at its start and
# exited 0, a pass with nothing tested.
.PHONY: check-db
check-db: ## make check against a throwaway Postgres
	$(POPGRES) run -- $(MAKE) popgres-gate GATE=check

.PHONY: verify-db
verify-db: ## make verify against a throwaway Postgres
	$(POPGRES) run -- $(MAKE) popgres-gate GATE=verify

# Inside popgres run: its database is the tests'. require-test-database already
# refuses an empty one; this says where it should have come from.
.PHONY: popgres-gate
popgres-gate:
	@if [ -z "$$DATABASE_URL" ] || [ -z "$(GATE)" ]; then \
		echo "::error::popgres-gate runs a GATE inside popgres run: use make check-db or make verify-db"; \
		exit 1; \
	fi
	YACHT_TEST_DATABASE_URL="$$DATABASE_URL" $(MAKE) $(GATE)

.PHONY: shell-check
shell-check: ## Lint the root-executed installer scripts
	shellcheck -s sh install.sh upgrade.sh
	sh -n install.sh
	sh -n upgrade.sh

.PHONY: boundary-check
boundary-check: ## Reject commercial-layer declarations in the engine
	@# internal/web/ui is vendored templUI; its Lucide Wallet icon is not billing.
	@if grep -rnE '^[[:space:]]*(type|func|var|const)[[:space:]]+[A-Za-z_]*(Tenant|tenant|Wallet|wallet|Billing|billing|Invoice|invoice|Subscription)' \
		--include='*.go' --exclude-dir=ui ./cmd ./internal ./engine 2>/dev/null; then \
		echo "::error::engine declares a commercial-layer concept — it belongs in the wrapping layer"; \
		exit 1; \
	fi
	@echo "boundary clean: no commercial declarations in engine code"

.PHONY: build-check
build-check: ## Compile every Go package without CGO
	CGO_ENABLED=0 $(GO) build ./...

# Only reports what the code actually reaches, so a finding here is a fix to
# make, not noise to triage. The go directive in go.mod is the toolchain CI and
# releases build with: a standard-library finding means raising it.
.PHONY: vulncheck
vulncheck: ## Fail on known vulnerabilities reachable from the code
	$(GO) tool govulncheck ./...

# Keep these dependency graphs serial even when a caller has MAKEFLAGS=-j.
# `verify` then executes each expensive gate once, while `assets` remains shared
# by generated-check and gallery within the same Make invocation.
.NOTPARALLEL: verify check assets

.PHONY: verify
verify: require-test-database generated-check shell-check check boundary-check gallery build-check vulncheck ## Run every CI/release gate

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
