# agentd

**A durable, sandboxed, policy-governed runtime for AI agents.**

> Status: pre-alpha (M0: foundation). Nothing below the roadmap line works yet.

Agent frameworks make it easy to write an agent. They do not make it safe to *run* one in
production. Real deployments keep re-inventing the same infrastructure:

| Problem | What agentd gives you |
|---|---|
| A run crashes on step 37 of 40 and starts over, re-paying for every LLM call | **Durable execution**: every step is journaled; a restarted run replays completed steps and resumes |
| A tool call runs `rm -rf` or curls an internal endpoint | **Sandboxed tools**: tool calls run in locked-down containers (no network by default, CPU/mem/time caps) |
| A loop burns $400 overnight | **Budgets**: per-run token, cost and step limits enforced before the LLM call, not after |
| "Should the agent be allowed to do that?" | **Policy + human approval**: declarative rules allow, deny, or pause a step until a human approves |
| "What did the agent actually do?" | **Tracing**: OpenTelemetry spans per run/step using the GenAI semantic conventions |

## How it works

Your agent stays plain Python. Every side effect (LLM call or tool call) goes through agentd as a
**step**:

```
Python agent (SDK)                    agentd (Go)
──────────────────                    ───────────────────────────────────────────
run = client.start("triage", input) → create run, attach budget + policy
run.llm(messages)                   → journal hit? return recorded result (replay)
                                      else: budget check → policy → provider call
                                            → record → return
run.tool("shell", cmd)              → journal hit? replay
                                      else: policy (may pause for approval)
                                            → Docker sandbox → record → return
```

Because results are keyed by step and stored before they're returned, a crashed worker can simply
run the agent function again: completed steps return instantly from the journal and execution
continues from the first unfinished step. See [ADR-0001](docs/adr/0001-architecture.md).

## Quick start (M0)

```bash
just run                       # agentd on :7700
curl localhost:7700/healthz    # {"status":"ok"}
just sdk-setup && just check   # Go + Python lint/tests
```

## Roadmap

| # | Milestone | Status |
|---|---|---|
| M0 | Foundation: Go server, Python SDK skeleton, CI, Dockerfile | ✅ |
| M1 | Runs + step journal (SQLite), deterministic replay, SDK `run`/`step` API | ⏳ |
| M2 | LLM gateway (Anthropic, OpenAI-compatible) + token/cost/step budgets | |
| M3 | Sandboxed tool execution (Docker: no network, caps, timeouts) | |
| M4 | Policy engine + human approvals (API, CLI, Slack) | |
| M5 | OpenTelemetry tracing + Grafana demo | |
| M6 | Polish: Helm chart, multi-arch images, PyPI, v0.1.0-alpha, launch post | |

## License

Apache 2.0
