# agentd-client

Python SDK for [agentd](https://github.com/anupam2105/agentd), a durable, sandboxed,
policy-governed runtime for AI agents. It has no runtime dependencies and supports Python 3.11+.

```bash
pip install agentd-client
```

```python
from agentd import Budget, Client, Run


def agent(run: Run, inp: dict) -> str:
    answer = run.llm("claude-sonnet-4-5", [{"role": "user", "content": inp["q"]}])
    listing = run.tool("shell", {"command": "ls -la /"})  # sandboxed, policy-checked
    token = run.side_effect(lambda: make_ticket(answer.text))  # journaled once
    return f"{answer.text}\n{listing.stdout}\n{token}"


client = Client()  # AGENTD_URL (default http://localhost:7700), AGENTD_API_TOKEN
out = client.execute(
    agent,
    agent="demo",
    input={"q": "hi"},
    run_id="job-42",
    budget=Budget(max_steps=10, max_cost_usd=0.50),
)
```

- **Durable:** if the process dies, call `execute` again with the same `run_id`. Completed steps
  replay from agentd's journal (`resp.replayed is True`) and execution continues.
- **Deterministic order:** steps are keyed `1:llm`, `2:tool:shell`, … by position. Put other
  non-determinism (random values, time, your own API calls) in `run.side_effect`.
- **Approvals:** if policy requires it, `run.tool(...)` blocks until a human runs
  `agentd approve <run> <step>` (default timeout 1 h, then `ApprovalTimeout`; the run stays
  resumable).
- **Errors:** `BudgetExceeded` ⊂ `StepDenied` ⊂ `StepError`, plus `StepFailed`,
  `ApprovalTimeout`, `NonDeterminismError`, `RunFailed`, `TransportError`, all subclasses of
  `AgentdError`.
- Messages use Anthropic's content-block format for every provider. `LLMResponse.message()`,
  `.tool_calls` and `tool_result_block()` help you build the tool loop.

Lower-level API: `client.run(...)` returns a `Run` (also usable as a context manager);
`client.get_run`, `steps`, `approvals`, `decide`, `cancel` and `tools` are also available.

## Development

The tests start a real `agentd` binary (built with `go`, or set `AGENTD_BIN`):

```bash
python3 -m venv .venv && .venv/bin/pip install -e '.[dev]'
.venv/bin/pytest && .venv/bin/ruff check . && .venv/bin/mypy
AGENTD_DOCKER_TESTS=1 .venv/bin/pytest   # include real sandbox tests
```
