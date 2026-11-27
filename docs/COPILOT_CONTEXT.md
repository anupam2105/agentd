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
| M0 | Foundation: server, config, logging, SDK skeleton, CI, Dockerfile, Justfile | ✅ 2026-11-26 |
| M1 | Runs + step journal (SQLite), replay, request-hash nondeterminism check, leases, bearer auth | ✅ 2026-11-27 |
| M2 | LLM gateway (Anthropic, OpenAI-compatible, mock) + step/token/cost budgets with reservations | ✅ 2026-11-27 |
| M3 | Docker sandbox tools (no network, ro rootfs, caps dropped, uid 65534, limits) | ✅ 2026-11-27 |
| M4 | Policy (allow/deny/require_approval) + approvals API/CLI/Slack | ✅ 2026-11-27 |
| M5 | OTel traces (GenAI semconv, deterministic trace per run), metrics, compose + Grafana demo | ✅ 2026-11-27 |
| M6 | Helm chart (dind/hostSocket/external), release workflow (GHCR, binaries, PyPI), docs, v0.1.0-alpha | ✅ 2026-11-27 |

## Layout

- `cmd/agentd`: serve + CLI dispatch. `internal/engine`: ExecuteStep/Decide/budgets/leases (core).
- `internal/{store,llm,sandbox,policy,agentcfg,tracing,notify,observability,httpserver,cli,config}`.
- `sdk/python` (dist name `agentd-client`, import `agentd`), `examples/sre_agent.py`,
  `deploy/{docker-compose.yml,helm/agentd,grafana,prometheus}`.

## Gotchas

- SQLite has MaxOpenConns(1): never call Store reader methods inside an `InTx` closure (deadlock).
- The mock LLM calls the first tool with `input_schema.examples[0]`, so give demo tools examples.
- Jaeger 2.23 removed the v1 query API that Grafana 11's Jaeger datasource uses; the demo links
  to the Jaeger UI instead.
- OrbStack: `DOCKER_HOST=$(docker context inspect -f '{{.Endpoints.docker.Host}}')`.

## Conventions

- Mirror shipmetrics/pageguard: Justfile targets, CI gate structure, distroless Dockerfile,
  `/healthz` `/readyz` `/metrics`, env config with `AGENTD_` prefix, private Prometheus registry,
  bounded route labels, ADRs in `docs/adr`.
- Default port 7700. Demo ports: Jaeger 26686, Prometheus 29090, Grafana 23000 (avoid 3000/8080/9090/13000).
- Safety defaults are non-negotiable: sandbox has no network unless policy grants it; unknown
  tools are denied; budgets fail closed.
