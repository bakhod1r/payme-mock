.PHONY: help test test-cover test-e2e check-measured cover-profile cover-html cover-gaps lint fmt up down logs playground

# Coverage gate. The plan commits to 100% across internal/; CI fails below it.
COVER_MIN  := 100.0
COVER_OUT  := cover.out

# Everything under internal/ is measured except two packages:
#
#   postgres/testdb  exists only to start a container for other tests; its
#                    failure paths are t.Fatalf calls no passing test can
#                    reach, so counting them would only invite a fake test.
#   traffic/domain   declares types and constants and nothing else. It has no
#                    statements to cover, so it never appears in a profile and
#                    the missing-data check would fail on an empty package.
COVER_PKGS  := ./internal/...
MEASURED    := $(shell go list ./internal/... | \
                 grep -v -e '/postgres/testdb$$' -e '/traffic/domain$$' | paste -sd, -)

help: ## Show available targets
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-16s\033[0m %s\n", $$1, $$2}'

fmt: ## Format all Go sources
	gofmt -w .

test: ## Run unit tests (no containers)
	go test $(COVER_PKGS) -race -short

test-e2e: ## Run tests that need real Postgres/Redis via testcontainers
	go test $(COVER_PKGS) -race -run 'E2E'

# Every test binary instruments every package under -coverpkg, so a single
# -coverprofile ends up holding one copy of each block per binary and
# `go tool cover -func` reports them separately instead of summing them.
# Collecting into a coverage directory and merging with `covdata` gives the
# real per-statement total.
COVER_DIR := .coverdata

cover-profile: ## Collect and merge coverage from every test binary
	@mkdir -p $(COVER_DIR)
	@find $(COVER_DIR) -type f -name 'cov*' -delete
	go test $(COVER_PKGS) -race -cover -coverpkg=$(MEASURED) -args -test.gocoverdir=$(PWD)/$(COVER_DIR)
	@go tool covdata textfmt -i=$(COVER_DIR) -o=$(COVER_OUT)

# A package no test binary imports is never instrumented, so it is absent from
# the profile and the percentage silently ignores it. Catching that is the
# difference between a real gate and one that reports 100% of what it happened
# to measure.
check-measured: cover-profile ## Fail if a measured package produced no coverage data
	@missing=""; \
	for pkg in $$(echo $(MEASURED) | tr ',' ' '); do \
		grep -q "^$$pkg/" $(COVER_OUT) || missing="$$missing $$pkg"; \
	done; \
	if [ -n "$$missing" ]; then \
		printf "\033[31mFAIL\033[0m no coverage data for:%s\n" "$$missing"; \
		echo "      add a test, or drop the package from MEASURED with a reason"; \
		exit 1; \
	fi

test-cover: cover-profile check-measured ## Run every test and enforce the coverage gate
	@go tool cover -func=$(COVER_OUT) | awk -v min=$(COVER_MIN) '\
		/^total:/ { \
			pct = $$3; sub(/%/, "", pct); \
			if (pct + 0 < min + 0) { \
				printf "\033[31mFAIL\033[0m coverage %s%% is below the %s%% gate\n", pct, min; \
				exit 1; \
			} \
			printf "\033[32mOK\033[0m coverage %s%%\n", pct; \
		}'

cover-html: cover-profile ## Open the coverage report in a browser
	go tool cover -html=$(COVER_OUT)

cover-gaps: cover-profile ## List every statement not covered by tests
	@go tool cover -func=$(COVER_OUT) | grep -v '100.0%' || echo "no gaps"

lint: ## Run golangci-lint, including the layer boundary rules
	golangci-lint run ./...

# The loader is Go's own and has to match the toolchain that built the binary,
# so it is copied from GOROOT on every build rather than committed once and
# forgotten.
playground: ## Build the WebAssembly playground into docs/playground
	GOOS=js GOARCH=wasm go build -o docs/playground/payme-mock.wasm ./cmd/playground
	cp "$$(go env GOROOT)/lib/wasm/wasm_exec.js" docs/playground/wasm_exec.js
	@ls -lh docs/playground/payme-mock.wasm

up: ## Start the whole stand
	docker compose up -d --build

down: ## Stop the stand and drop its volumes
	docker compose down -v

logs: ## Follow all service logs
	docker compose logs -f
