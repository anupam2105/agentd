# Context for AI pair-programming

Read this before making changes.

## Author and portfolio

Solo side-project developer (`anupam2105`), Go/SRE background. Earlier projects (independent of
this one, but share conventions): `shipmetrics` (DORA/SLO platform) and `pageguard` (AI on-call
co-pilot), both under `~/personal/`.

## agentd

- Repo `github.com/anupam2105/agentd`, local `~/personal/agentd`, Apache 2.0.
- Pitch: a durable, sandboxed, policy-governed runtime for AI agents ("Kubernetes for agents").
- Server in Go 1.26; agent SDK in **Python only** (`sdk/python`, stdlib-only runtime deps).
- Architecture: execution gateway + step journal, see `docs/adr/0001-architecture.md`.

## Roadmap

| # | Milestone | Status |
|---|---|---|
| M0 | Foundation: server (health/ready/metrics/version), config, logging, Python SDK skeleton, CI (Go lint/test/build/vuln/secrets + Python ruff/mypy/pytest), Dockerfile, Justfile | ✅ Done (2026-11-26) |
| M1 | Runs + step journal in SQLite; `POST /v1/runs`, `POST /v1/runs/{id}/steps` with replay + request-hash check; bearer auth; SDK `client.start()`, `run.step()` | ⏳ Next |
| M2 | LLM gateway: provider interface (Anthropic, OpenAI-compatible); per-run token/cost/step budgets; SDK `run.llm()` | |
| M3 | Docker sandbox tools: no network by default, read-only rootfs, CPU/mem/pids/time caps, output caps; SDK `run.tool()` | |
| M4 | Policy (YAML: allow/deny/require_approval by tool, args, cost); approvals API + `agentd approve` CLI + Slack | |
| M5 | OpenTelemetry traces (GenAI semconv), Prometheus metrics, docker-compose + Grafana demo | |
| M6 | Helm chart, multi-arch ghcr.io images, PyPI publish, v0.1.0-alpha, launch post | |

## Conventions

- Mirror shipmetrics/pageguard: Justfile targets, CI gate structure, distroless Dockerfile,
  `/healthz` `/readyz` `/metrics`, env config with `AGENTD_` prefix, private Prometheus registry,
  bounded route labels, ADRs in `docs/adr`.
- Default port 7700 (8080/3000/9090 are used by the other projects' demo stacks).
- Safety defaults are non-negotiable: sandbox has no network unless policy grants it; unknown
  tools are denied; budgets fail closed.
