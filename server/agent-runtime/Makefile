.PHONY: build test fmt vet lint clean test-postgres tidy-check

build:
	go build ./...

test:
	go test ./...

fmt:
	go fmt ./...

vet:
	go vet ./...

lint: fmt vet
	golangci-lint run ./...

e2e-llm:
	go test -tags=llm ./e2e/llm/...

# test-postgres runs the PostgreSQL-backed contract tests. It requires
# POSTGRES_TEST_DSN and fails (never silently skips) when the DSN is absent.
# Test packages must honor POSTGRES_TEST_FAIL_ON_SKIP=1: presence of that
# variable turns a would-be skip into a failure.
test-postgres:
	@test -n "$$POSTGRES_TEST_DSN" || { echo "POSTGRES_TEST_DSN is required, e.g. postgres://user:pass@localhost:5432/db?sslmode=disable"; exit 1; }
	POSTGRES_TEST_FAIL_ON_SKIP=1 go test -count=1 ./pkg/db/... ./pkg/session/... ./pkg/workflow/...

# tidy-check fails if go.mod/go.sum are not tidy.
tidy-check:
	@go mod tidy
	@git diff --exit-code go.mod go.sum || { echo "go.mod/go.sum are not tidy (run 'go mod tidy')"; exit 1; }

clean:
	go clean ./...
