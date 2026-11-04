# CLAUDE.md

This repository is a limit order book matching engine written in Go. Code clarity matters as much as verifiable performance data.

## Read before starting

1. `docs/SPEC.md`: requirements and architecture; it takes precedence in any conflict
2. `docs/TASKS.md`: start from the first unchecked milestone
3. `docs/DECISIONS.md`: decisions already made; do not overturn them on your own. To change one, explain why and get approval first

## Working practices

- Write a plan before starting each milestone
- Commit after each completed task, with commit messages in English following Conventional Commits
- When a task is done, check it off in TASKS.md; append new technical decisions to DECISIONS.md
- Everything committed to GitHub (code, comments, docs, commit messages) is in English and concise: say what changed and why, not the step-by-step process. Commit messages use Conventional Commits with a short subject and at most a brief body.

## Common commands

```bash
make help                           # list the Makefile shortcuts
go test -race ./...                 # all tests
go test -short ./...                # quick run; property tests use only 10,000 commands
go test -bench=. -benchmem ./internal/book
go test -fuzz=FuzzBook -fuzztime=30s ./internal/book
golangci-lint run ./...             # static analysis, including govet and staticcheck
go run ./cmd/engine                 # start the service
go run ./cmd/loadgen -rate 50000    # load test
```

## Hard rules

- Prices and quantities are always `int64` integers (prices in ticks, the minimum price increment); floating point is forbidden
- The matching core `internal/book` does not depend on networking, time, or randomness; timestamps are passed in by the caller so that everything is replayable and testable
- The matching core uses only the standard library where possible; the order book's tree or skip list is implemented by hand
- Every exported function has tests; any change to matching logic must also run the fuzz tests and invariant tests
- Performance changes must include before/after benchmark comparisons, with results written to `bench-results/`
- Do not trade readability for performance unless benchmarks show a clear gain, and record that in DECISIONS.md
