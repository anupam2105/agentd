# Changelog

All notable changes to this project are documented here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.1.0-alpha] - 2026-11-27

First public release.

### Added
- **Durable execution:** runs and a `(run_id, step_key)` step journal in SQLite; replay of
  completed steps; request hashing with non-determinism detection; leases for crashed in-flight
  steps; retryable vs. terminal failures.
- **LLM gateway:** Anthropic and OpenAI-compatible providers behind one Anthropic-style message
  format (text, tool use, tool results); a deterministic `mock` provider.
- **Budgets:** per-run `max_steps`, `max_tokens`, `max_cost_usd`, with worst-case reservations
  and fail-closed pricing.
- **Sandboxed tools:** declarative tools run in hardened, throwaway Docker containers (no
  network by default, read-only rootfs, no capabilities, non-root, resource and time limits).
- **Policy and approvals:** first-match allow / deny / require_approval rules on kind, tool,
  model, agent, input regexes and cost; approvals over API, CLI and Slack notifications.
- **Observability:** one OpenTelemetry trace per run (GenAI semantic conventions) and Prometheus
  metrics; Grafana dashboard.
- **Operator CLI** in the same binary: `runs`, `run`, `steps`, `approvals`, `approve`, `deny`,
  `cancel`, `tools`.
- **Python SDK** (`agentd-client`): `Client.execute`, `Run.llm/tool/side_effect`, typed errors,
  retries.
- Example SRE agent, docker-compose demo (Jaeger, Prometheus, Grafana), Helm chart (dind,
  host-socket or external sandbox), multi-arch images, release binaries.
