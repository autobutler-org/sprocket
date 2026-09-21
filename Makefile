SHELL := /usr/bin/env
.SHELLFLAGS = bash -e -o pipefail -c
.DEFAULT_GOAL := help
.NOTPARALLEL:
.SILENT: # use set -v to print commands executed
.ONESHELL:

# .ONESHELL needs GNU Make 3.82+. macOS ships 3.81, where it is silently ignored and
# every recipe line runs in its own shell -- multi-line `if` blocks then die with
# "syntax error: unexpected end of file", which points nowhere near the real problem.
MIN_MAKE := 3.82
ifneq ($(firstword $(sort $(MAKE_VERSION) $(MIN_MAKE))),$(MIN_MAKE))
$(error GNU Make $(MAKE_VERSION) is too old; this Makefile needs $(MIN_MAKE)+. \
On macOS run `brew install make` and use `gmake`, or put \
"$$(brew --prefix)/opt/make/libexec/gnubin" first on PATH.)
endif

ifndef VERBOSE
MAKEFLAGS += --no-print-directory
endif

ifneq (,$(wildcard ./.env))
    include .env
    export
endif

export GOPROXY ?= https://proxy.golang.org,direct
GO := $(shell which go)
AIR := $(shell which air)

# Read the toolchain from go.mod rather than pinning it here, so the two cannot drift.
GO_MOD_VERSION := $(shell awk '/^go /{print $$2; exit}' go.mod)
export GOTOOLCHAIN=go$(GO_MOD_VERSION)

MAIN ?= ./cmd/example/main.go
EXE ?= ./build/example

##@ Development Environment

.PHONY: setup
setup: setup/gotools setup/air

.PHONY: setup/gotools
setup/gotools: ## Install go tools
	$(GO) install golang.org/x/tools/gopls@latest
	$(GO) install github.com/cweill/gotests/gotests@v1.6.0
	$(GO) install github.com/josharian/impl@v1.4.0
	$(GO) install github.com/haya14busa/goplay/cmd/goplay@v1.0.0
	$(GO) install github.com/go-delve/delve/cmd/dlv@latest
	$(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	# staticcheck is not installed on its own -- golangci-lint runs it as one of its
	# linters, and two copies at different versions disagree about what is a warning.

.PHONY: setup/air
setup/air: ## Install air tool
	$(GO) install github.com/air-verse/air@latest

##@ Build

.PHONY: build
build: build/go ## Build codebase

.PHONY: build/go
build/go: ## Build Go codebase
	mkdir -p ./build
	$(GO) build -o $(EXE) $(MAIN)

.PHONY: clean
clean: ## Clean build space
	rm -rf \
		./build \
		./coverage.out \
		./tmp

.PHONY: run
run: build ## Build and run the application
	$(EXE)

PRINT_COVERAGE ?= 0

.PHONY: test
test: ## Run tests
	$(GO) test ./... \
		-coverprofile=coverage.out \
		-covermode=atomic
	if [[ "$(PRINT_COVERAGE)" = "1" || "$(PRINT_COVERAGE)" = "true" ]] ; then
		$(GO) tool cover -func=coverage.out
	fi

.PHONY: watch
watch: ## Watch for changes and rebuild
	$(AIR) \
		--build.cmd "$(MAKE) build" \
		--build.entrypoint "$(EXE)" \
		--build.exclude_dir ".github,build,docs"

##@ Dependencies

.PHONY: deps
deps: ## Install dependencies for Go
	$(GO) mod download

.PHONY: tidy
tidy: ## Go deps (go mod tidy)
	$(GO) mod tidy

.PHONY: upgrade
upgrade: upgrade/go ## Upgrade dependencies

.PHONY: upgrade/go
upgrade/go: ## Upgrade Go dependencies
	$(GO) get -u ./...
	$(MAKE) tidy

##@ Code Quality

GOLINT_ARGS ?= --verbose --config .golangci.yml

.PHONY: check
check: check/go ## Check code quality

.PHONY: check/go
check/go: ## Check Go code quality
	$(GO) tool golangci-lint run --fix $(GOLINT_ARGS) ./...

.PHONY: check/vuln
check/vuln: ## Check Go module for known CVEs (govulncheck)
	if ! command -v govulncheck >/dev/null 2>&1; then
		echo "govulncheck is not installed. Run 'make setup/gotools' first."
		exit 1
	fi
	govulncheck ./...

.PHONY: format
format: format/go ## Format code

.PHONY: format/go
format/go: ## Format Go code
	$(GO) tool golangci-lint fmt $(GOLINT_ARGS)

##@ Helpers

.PHONY: help
help: ## Display this help
	awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_\/-]+:.*?##/ { printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

env-%: ## Check if env var is defined
	if [ -z "$($*)" ]; then \
		echo "Error: Environment variable '$*' is not set."; \
		exit 1; \
	fi
