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
  (`llm#3`, `tool:shell#7`) unless the caller passes an explicit key. Agent code must therefore be
  deterministic *between* steps, the same contract as Temporal workflows. The journal stores a
  hash of each step's request; a replayed step with a different request fails loudly instead of
  silently returning a stale result.
- **Step lifecycle:** `pending → (awaiting_approval) → running → completed | failed`. The result is
  committed before it is returned, so a client that crashes after the commit gets the recorded
  result on retry.
- **Approvals** are steps in `awaiting_approval`; the SDK long-polls until a human decides.
- **Budgets** are checked before a provider call using a max-tokens estimate, then reconciled with
  reported usage.

## Consequences

- Crash recovery is "run the function again"; no worker coordination is needed in v0.1.
- Non-deterministic agent code (e.g. branching on `random()` outside a step) breaks replay; the
  SDK will offer `run.side_effect()` to journal such values.
- Tools must be invoked through agentd to be sandboxed; agentd cannot police code that bypasses it.
