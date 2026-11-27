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

# Real-container tests (sandbox + SDK). Uses the active docker context's socket.
docker-test:
    DOCKER_HOST=$(docker context inspect -f '{{{{.Endpoints.docker.Host}}') AGENTD_DOCKER_TESTS=1 go test -count=1 -run TestDockerIntegration -v ./internal/sandbox/
    cd sdk/python && DOCKER_HOST=$(docker context inspect -f '{{{{.Endpoints.docker.Host}}') AGENTD_DOCKER_TESTS=1 .venv/bin/pytest -q

# Full demo: agentd + Jaeger + Prometheus + Grafana
demo-up:
    docker compose -f deploy/docker-compose.yml up -d --build

demo-down:
    docker compose -f deploy/docker-compose.yml down -v

# Run the example SRE agent against the demo stack (mock LLM by default)
demo-agent *ARGS:
    AGENTD_URL=http://localhost:7700 AGENTD_API_TOKEN=demo-token sdk/python/.venv/bin/python examples/sre_agent.py {{ARGS}}
