# Agents don't need another framework. They need a runtime.

*Launching agentd v0.1.0-alpha: durable, sandboxed, policy-governed execution for AI agents.*

Writing an agent takes an afternoon: a loop, an LLM call, a few tools. Running that agent for
real, overnight and unattended with access to things that matter, is where the problems start:

- **It crashes on step 37 of 40.** The process gets OOM-killed, the pod is rescheduled, or the
  provider returns a 529. You start over, pay for 37 LLM calls again, and re-run tools that
  already changed the world.
- **It does something it shouldn't.** A prompt-injected web page convinces the model to `curl`
  an internal endpoint, or to `rm -rf` the wrong directory.
- **It loops.** In the morning you find a $400 bill and a 3,000-message conversation.
- **Nobody can say what it did.** Logs are scattered across print statements.

These aren't framework problems. They're the same problems that job schedulers, workflow engines
and container platforms solved for ordinary software, and agents need the same runtime
guarantees. That runtime is what agentd provides.

## The idea: every side effect is a step

agentd is an **execution gateway**. Your agent stays ordinary Python and runs wherever you like.
Every side effect it performs, whether an LLM call, a tool call, or anything else
non-deterministic, goes through agentd as a step:

```python
def agent(run, task):
    messages = [{"role": "user", "content": task}]
    while True:
        resp = run.llm("claude-sonnet-4-5", messages, tools=run.client.tools())
        ...
        result = run.tool(call.name, call.input)
```

For each step, agentd:

1. **Checks the journal.** If this `(run, step)` already completed, it returns the recorded
   result. It also hashes the request: if your code now asks for something different at the same
   position, you get a `NonDeterminismError`, not a silently stale answer.
2. **Evaluates policy.** First-match rules can allow, deny, or `require_approval`, matching on
   tool, model, agent, regexes over tool inputs, or estimated cost.
3. **Enforces the budget.** It reserves the worst case *before* calling the provider, so a run
   (or ten concurrent steps of one run) can't overshoot `max_cost_usd`.
4. **Executes.** LLM calls go to Anthropic or any OpenAI-compatible API through one message
   format. Tools run in a fresh container: no network, read-only root, no capabilities,
   non-root, with memory/CPU/PID/time limits.
5. **Records the result**, then returns it, and emits an OpenTelemetry span and metrics.

Crash recovery becomes "run the function again":

```
$ python sre_agent.py --run-id triage-1 --crash-after 2
  [1] llm mock -> tool_use (executed)
  [2] tool shell exit=0 (executed)
*** simulated crash after 2 steps

$ python sre_agent.py --run-id triage-1
  [1] llm mock -> tool_use (replayed)
  [2] tool shell exit=0 (replayed)
  [3] llm mock -> end_turn (executed)
agentd: step 4:tool:shell needs approval (policy rule destructive-shell).
```

This is the Temporal model (deterministic code between journaled activities), shrunk to fit
agents and packaged as one Go binary with SQLite.

## Humans in the loop, durably

`require_approval` doesn't block a thread somewhere. The step is stored as
`awaiting_approval`, a Slack message goes out, and anyone with the token can decide:

```
$ agentd approvals
RUN       STEP          KIND  NAME   RULE               WAITING
triage-1  4:tool:shell  tool  shell  destructive-shell  12s
$ agentd approve triage-1 4:tool:shell --reason "only a cache dir"
```

The decision is journaled with the step. If the agent process dies while waiting, the approval
is still there when it comes back, and a replay never asks the human twice.

## One trace per run, even across crashes

agentd derives each run's trace ID from its run ID. Steps executed before a crash and after the
resume, possibly by different processes, land in the **same** trace. Spans follow the
OpenTelemetry GenAI semantic conventions (`chat {model}`, `execute_tool {tool}`,
`gen_ai.usage.*`), so your existing tracing backend understands them.

## What's in v0.1.0-alpha

- Durable journal with replay, non-determinism detection, leases and retryable failures
- Anthropic, OpenAI-compatible and mock providers; step, token and USD budgets
- Hardened Docker sandbox; deny-by-default tools validated by JSON Schema
- Policy rules plus approvals via API, CLI and Slack
- OpenTelemetry traces, Prometheus metrics, Grafana dashboard
- Python SDK (`pip install agentd-client`, zero dependencies), Helm chart, multi-arch images

Try it in about a minute with no API key (the `mock` model is deterministic and free):

```bash
git clone https://github.com/anupam2105/agentd && cd agentd
just demo-up && just sdk-setup && just demo-agent --crash-after 2
```

## What it isn't (yet)

- **A framework.** Bring your own loop, or wrap the one you have; agentd only sees side effects.
- **A security boundary for code that bypasses it.** If your agent shells out directly, agentd
  can't help. And Docker containers share a kernel: for hostile code, run the sandbox daemon on a
  dedicated VM with gVisor or Kata.
- **Horizontally scalable.** v0.1 is a single replica on SQLite. Agent workloads are dominated
  by LLM latency, so that goes surprisingly far, and a Postgres store is the obvious next step.

Next on the list: streaming responses, per-user tokens and roles, a TypeScript SDK, and
Postgres.

Feedback and issues are welcome: https://github.com/anupam2105/agentd
