default:
    @just --list

test:
    go test -race -count=1 ./...

lint:
    golangci-lint run

check: lint test sdk-check

run:
    go run ./cmd/agentd

build:
    CGO_ENABLED=0 go build -o bin/agentd ./cmd/agentd

fmt:
    gofmt -s -w .
    goimports -w .

tidy:
    go mod tidy

clean:
    rm -rf bin/

# Python SDK: creates sdk/python/.venv on first use
sdk-setup:
    cd sdk/python && python3 -m venv .venv && .venv/bin/pip install -q -e '.[dev]'

sdk-check:
    cd sdk/python && .venv/bin/ruff check . && .venv/bin/ruff format --check . && .venv/bin/mypy && .venv/bin/pytest -q
