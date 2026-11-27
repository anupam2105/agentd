# agentd

**A durable, sandboxed, policy-governed runtime for AI agents.**

[![CI](https://github.com/anupam2105/agentd/actions/workflows/ci.yml/badge.svg)](https://github.com/anupam2105/agentd/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)

> Status: **v0.1.0-alpha**. The core works end to end and is tested against real Docker and
> Kubernetes. APIs may still change before v1.

Agent frameworks make it easy to *write* an agent. They don't make it safe to *run* one in
production, so every team ends up rebuilding the same infrastructure:

| Problem | What agentd gives you |
|---|---|
| A run crashes on step 37 of 40 and starts over, paying again for every LLM call | **Durable execution**: every step is journaled. Re-running replays completed steps from the journal and resumes at the first unfinished one |
| A tool call runs `rm -rf` or curls an internal endpoint | **Sandboxed tools**: each call runs in a throwaway container with no network, a read-only rootfs, no capabilities, a non-root user, and CPU/memory/PID/time caps |
| A loop burns $400 overnight | **Budgets**: per-run step, token and USD limits, checked *before* the provider call, including in-flight reservations |
| "Should the agent be allowed to do that?" | **Policy + human approval**: first-match rules allow, deny, or pause a step until a human approves it (CLI, API, Slack) |
| "What did the agent actually do?" | **Observability**: one OpenTelemetry trace per run (GenAI semantic conventions) plus Prometheus metrics |

Agents stay plain Python. agentd is one Go binary with a SQLite journal.

```
 your agent (Python SDK)                      agentd (Go)
 ───────────────────────                      ──────────────────────────────────────────────
 run.llm("claude-…", messages, tools)  ──►  journal hit? ─ yes ─► replay recorded result
                                              │ no
                                              ▼
                                           policy ─► budget (reserve) ─► Anthropic / OpenAI
 run.tool("shell", {"command": …})     ──►   ─► record result ─► return
                                           policy may pause: awaiting_approval ─► human ─►
                                           Docker sandbox ─► record ─► return
                                           every step ─► OTel span · Prometheus metrics
```

## Quick start (60 seconds, no API key needed)

```bash
git clone https://github.com/anupam2105/agentd && cd agentd
just demo-up        # agentd + Jaeger + Prometheus + Grafana (docker compose)
just sdk-setup      # Python venv for the SDK and the example agent

# 1. Start an agent and "crash" it after two steps
just demo-agent --run-id triage-1 --crash-after 2

# 2. Run it again: steps 1-2 replay from the journal, then it continues
just demo-agent --run-id triage-1
#    ...and pauses: its remediation command matches the `destructive-shell` policy rule

# 3. In another terminal, approve it as a human
go build -o bin/agentd ./cmd/agentd
export AGENTD_URL=http://localhost:7700 AGENTD_API_TOKEN=demo-token
bin/agentd approvals
bin/agentd approve triage-1 4:tool:shell --reason "safe: only a cache dir"
```

Then open the trace in **Jaeger** (http://localhost:26686, service `agentd`). The run before the
crash and after the resume shows up as *one* trace. The **Grafana** dashboard is at
http://localhost:23000 and **Prometheus** at http://localhost:29090.

The demo uses the free, deterministic `mock` model. To use a real model, export
`ANTHROPIC_API_KEY` or `OPENAI_API_KEY` before `just demo-up`, then pass
`--model claude-sonnet-4-5` (or another model). Check `pricing` in
[`agentd.example.yaml`](agentd.example.yaml) first.

## Writing an agent

```bash
pip install agentd-client          # or: pip install -e sdk/python
```

```python
from agentd import Budget, Client, Run, StepDenied, tool_result_block

def agent(run: Run, task: str) -> str:
    messages = [{"role": "user", "content": task}]
    tools = run.client.tools()                    # tools configured on the server
    while True:
        resp = run.llm("claude-sonnet-4-5", messages, tools=tools, max_tokens=1024)
        messages.append(resp.message())
        if not resp.tool_calls:
            return resp.text
        results = []
        for call in resp.tool_calls:
            try:
                results.append(tool_result_block(call, run.tool(call.name, call.input)))
            except StepDenied as e:               # policy, a human, or a budget said no
                results.append(tool_result_block(call, str(e), is_error=True))
        messages.append({"role": "user", "content": results})

client = Client()   # AGENTD_URL, AGENTD_API_TOKEN
print(client.execute(agent, agent="triage", input="Why is disk usage high?",
                     run_id="incident-4711",
                     budget=Budget(max_steps=30, max_cost_usd=2.00)))
```

`client.execute` is idempotent per `run_id`. Call it again after a crash, deploy, or OOM and
completed steps replay instantly; if the run already finished, its stored output is returned.

The rules (the same contract as Temporal workflows):

- Do side effects through the run: `run.llm`, `run.tool`, and `run.side_effect(fn)` for anything
  else non-deterministic (random IDs, timestamps, calls to your own APIs).
- Issue steps in a deterministic order. Steps are keyed by position (`1:llm`, `2:tool:shell`, …)
  and agentd stores a hash of each request; if a replayed step's request differs, you get a
  `NonDeterminismError` instead of a silently stale result.

Every provider is exposed in **Anthropic's message format** (content blocks, `tool_use`,
`tool_result`); agentd translates to and from OpenAI-compatible APIs. Errors are typed:
`BudgetExceeded`, `StepDenied`, `StepFailed`, `ApprovalTimeout`, `NonDeterminismError`,
`RunFailed`, `TransportError`. See [`sdk/python`](sdk/python) and
[`examples/sre_agent.py`](examples/sre_agent.py).

## Configuration

Server settings come from environment variables; tools, pricing and policy come from a YAML file
(`AGENTD_CONFIG`; see the annotated [`agentd.example.yaml`](agentd.example.yaml)).

| Variable | Default | |
|---|---|---|
| `AGENTD_HTTP_ADDR` | `:7700` | listen address |
| `AGENTD_API_TOKEN` | — | bearer token required on `/v1/*` (**set it** anywhere shared) |
| `AGENTD_DB_PATH` | `agentd.db` | SQLite journal |
| `AGENTD_CONFIG` | — | tools / pricing / policy YAML |
| `AGENTD_ANTHROPIC_API_KEY` (or `ANTHROPIC_API_KEY`) | — | enables Anthropic (models named `claude*`) |
| `AGENTD_OPENAI_API_KEY` (or `OPENAI_API_KEY`) | — | enables OpenAI (any other model name; or pass `provider=`) |
| `AGENTD_OPENAI_BASE_URL` | `https://api.openai.com/v1` | any OpenAI-compatible API (vLLM, Ollama, …) |
| `AGENTD_DOCKER_HOST` (or `DOCKER_HOST`) | `unix:///var/run/docker.sock` | where tool containers run |
| `AGENTD_STEP_LEASE` | `10m` | a `running` step older than this may be re-executed |
| `AGENTD_LLM_TIMEOUT` | `2m` | per provider call |
| `AGENTD_SLACK_WEBHOOK_URL`, `AGENTD_PUBLIC_URL` | — | approval notifications |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | — | enables tracing (standard OTel env vars apply) |
| `AGENTD_LOG_LEVEL`, `AGENTD_LOG_FORMAT` | `info`, `json` | |

The `mock` model is always available and free, which is useful for tests and CI.

### Tools

```yaml
tools:
  shell:
    description: Run a POSIX shell command in an isolated Alpine container (no network).
    image: alpine:3.22
    command: ["sh", "-c", "{{.command}}"]   # Go templates over the tool input
    timeout: 30s
    memory: 128m
    cpus: 0.5
    # network: true                          # off by default
    input_schema:                             # JSON Schema, validated before anything runs
      type: object
      required: [command]
      properties: { command: { type: string } }
```

A non-zero exit code or a timeout is a *result* the model sees, not an error.

### Policy

```yaml
policy:
  defaults: { llm: allow, tool: deny, record: allow }   # unknown tools are denied
  rules:                                                # first match wins
    - name: destructive-shell
      kind: tool
      tool: shell
      input: { command: '\b(rm|dd|mkfs|kill|chmod)\b' } # regex per input field
      action: require_approval
    - { name: shell, kind: tool, tool: shell, action: allow }
    - { name: expensive-llm-call, kind: llm, min_cost_usd: 0.50, action: require_approval }
```

Rules match on `kind`, `tool` / `model` / `agent` globs, per-field input regexes and the estimated
cost. Steps that need approval wait in `awaiting_approval`; decide with `agentd approve|deny`,
`POST /v1/runs/{id}/steps/{key}/decision`, or `client.decide(...)`.

### Budgets

`max_steps`, `max_tokens`, `max_cost_usd` per run. Before each LLM call agentd reserves the
worst case (input estimate + `max_tokens`) and refuses the step if used + reserved + estimate
would exceed the limit, so concurrent steps can't overshoot together. A run with a cost budget
refuses models that have no `pricing` entry (fail closed).

## Operator CLI

The same binary is also the CLI (`AGENTD_URL`, `AGENTD_API_TOKEN`; add `--json` to any command):

```
agentd runs [--status running] [--agent triage]   agentd approvals
agentd run <id>                                   agentd approve <run> <step> [--reason ...]
agentd steps <id>                                 agentd deny <run> <step> [--reason ...]
agentd tools                                      agentd cancel <run>
```

## HTTP API

All `/v1` routes except `/v1/version` require `Authorization: Bearer $AGENTD_API_TOKEN`. Errors
look like `{"error":{"code","message","retryable"}}`.

| Route | |
|---|---|
| `POST /v1/runs` | create or attach (`{"id"?, "agent", "input"?, "budget"?}`); idempotent per id |
| `GET /v1/runs`, `GET /v1/runs/{id}` | list (`?status=&agent=&limit=`) / get |
| `POST /v1/runs/{id}/finish` · `/cancel` | `{"status":"completed","output":…}` / `{"reason"}` |
| `POST /v1/runs/{id}/steps` | execute or replay `{"key","kind":"llm|tool|record","request"}` |
| `GET /v1/runs/{id}/steps[/{key}]` | journal |
| `POST /v1/runs/{id}/steps/{key}/decision` | `{"approve":true,"by","reason"}` |
| `GET /v1/approvals`, `GET /v1/tools` | pending approvals / tool definitions |
| `GET /healthz`, `/readyz`, `/metrics`, `/v1/version` | |

## Deploying

- **Container:** `ghcr.io/anupam2105/agentd` (linux/amd64, linux/arm64, distroless, non-root).
  agentd needs a Docker Engine API for tools: mount a socket, or point `AGENTD_DOCKER_HOST` at a
  dedicated sandbox host.
- **Kubernetes:** [`deploy/helm/agentd`](deploy/helm/agentd). By default tools run in a
  Docker-in-Docker sidecar, so the node's runtime doesn't matter.
- **Binaries:** on the [releases page](https://github.com/anupam2105/agentd/releases).

Read [SECURITY.md](SECURITY.md) before exposing agentd beyond your laptop.

## Observability

- **Traces:** each run is one trace (`invoke_agent {agent}`) with a deterministic trace ID derived
  from the run ID, so steps executed by different processes before and after a crash land in the
  same trace. Steps are `chat {model}` / `execute_tool {tool}` spans with `gen_ai.*` attributes
  (tokens, model, provider) plus `agentd.*` attributes (policy rule, cost, replayed).
- **Metrics:** `agentd_steps_total{kind,status}`, `agentd_step_replays_total`,
  `agentd_llm_tokens_total`, `agentd_llm_cost_usd_total`, `agentd_tool_executions_total`,
  `agentd_policy_decisions_total`, `agentd_budget_denials_total`, `agentd_approvals_pending`,
  `agentd_approval_wait_seconds`, and more. The Grafana dashboard is in
  [`deploy/grafana`](deploy/grafana).

## Design

- [ADR-0001: execution gateway with a step journal](docs/adr/0001-architecture.md)
- [Launch post: why agents need a runtime](docs/blog/launch.md)

Limitations in v0.1: single replica (SQLite), Python SDK only, no streaming responses, and
agentd can only govern side effects that go through it.

## Development

```bash
just check        # golangci-lint, go test -race, SDK ruff/mypy/pytest (against a real binary)
just docker-test  # sandbox + SDK tests against real containers
```

## License

Apache 2.0
