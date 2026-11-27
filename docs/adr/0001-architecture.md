# ADR-0001: Architecture: execution gateway with a step journal

- Status: accepted
- Date: 2026-11-26

## Context

agentd must give agents durable execution, sandboxed tools, budgets, policy/approval and tracing,
while letting people write agents in ordinary Python.

Options considered:

1. **Library-only (Python):** simplest, but every guarantee lives in the same process as the agent
   and dies with it; sandboxing and approvals need a separate service anyway.
2. **Run agent code inside agentd:** strongest control, but forces a packaging/deploy model and a
   Python runtime inside the server.
3. **Execution gateway:** the agent runs anywhere; every *side effect* is a step sent to agentd,
   which journals it and enforces budget, policy and sandboxing.

## Decision

Option 3.

- **Server:** Go 1.26 (single static binary, distroless image), same stack and layout conventions
  as shipmetrics/pageguard. SQLite (`modernc.org/sqlite`, no cgo) for the journal; a Postgres
  backend may follow behind the same interface.
- **SDK:** Python only, standard-library HTTP so it has zero runtime dependencies.
- **Step identity:** `(run_id, step_key)`. The SDK derives `step_key` from call order
  (`1:llm`, `2:tool:shell`, `3:record`). Agent code must therefore be
  deterministic *between* steps, the same contract as Temporal workflows. The journal stores a
  hash of each step's request; a replayed step with a different request fails loudly instead of
  silently returning a stale result.
- **Step lifecycle:** `(awaiting_approval → approved) → running → completed | failed | denied`.
  The result is committed before it is returned, so a client that crashes after the commit gets
  the recorded result on retry. Retryable failures (provider 429/5xx, Docker unreachable) are
  not terminal: the same step key executes again on the next call.
- **Leases:** a step stuck in `running` (agentd crashed mid-call) can be re-executed after
  `AGENTD_STEP_LEASE`; a late result from the old attempt is discarded.
- **Approvals** are steps in `awaiting_approval`; the SDK polls until a human decides.
- **Budgets** reserve the worst case (input estimate + `max_tokens`) before a provider call,
  inside the same transaction that admits the step, then reconcile with reported usage.
  Concurrent steps therefore cannot overshoot together.
- **Policy** runs before admission. Approvals and denials are recorded on the step, so replays
  never re-ask a human.
- **Tracing:** the trace ID is derived from the run ID, so every process that touches a run emits
  spans into the same trace without propagating context through the SDK.

## Consequences

- Crash recovery is "run the function again"; no worker coordination is needed in v0.1.
- Non-deterministic agent code (e.g. branching on `random()` outside a step) breaks replay;
  `run.side_effect()` journals such values.
- SQLite means one replica. Throughput is bounded by a single writer, which is fine for agent
  workloads dominated by LLM latency. A Postgres store would lift this.
- Tools must be invoked through agentd to be sandboxed; agentd cannot police code that bypasses it.
