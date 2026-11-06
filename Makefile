GO       ?= go
BOOK     := ./internal/book

# Overridable knobs, e.g. `make bench BENCH=Match COUNT=10`.
BENCH    ?= .
COUNT    ?= 1
FUZZ     ?= FuzzBook
FUZZTIME ?= 30s
OUT      ?= bench-results/e2e
RATES    ?=

.DEFAULT_GOAL := help
.PHONY: help test cover bench fuzz e2e lint fmt run

help: ## Show available targets
	@awk -F':.*## ' '/^[a-z0-9]+:.*## / {printf "  %-8s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

test: ## Run all tests with the race detector
	$(GO) test -race ./...

cover: ## Run tests and print per-function coverage
	@mkdir -p coverage
	$(GO) test -race -coverprofile=coverage/cover.out ./...
	$(GO) tool cover -func=coverage/cover.out

bench: ## Run order book benchmarks (BENCH, COUNT)
	$(GO) test -run='^$$' -bench='$(BENCH)' -benchmem -count=$(COUNT) $(BOOK)

fuzz: ## Fuzz the order book (FUZZ, FUZZTIME)
	$(GO) test -run='^$$' -fuzz='^$(FUZZ)$$' -fuzztime=$(FUZZTIME) $(BOOK)

e2e: ## Measure end-to-end latency with loadgen into OUT (RATES)
	scripts/e2e.sh $(OUT) $(RATES)

lint: ## Run golangci-lint (includes govet and staticcheck)
	golangci-lint run ./...

fmt: ## Format code with gofmt and goimports
	golangci-lint fmt ./...

run: ## Start the engine service
	$(GO) run ./cmd/engine
