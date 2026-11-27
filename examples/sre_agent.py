#!/usr/bin/env python3
"""A small SRE triage agent running on agentd.

The agent investigates a host with sandboxed tools, writes a summary, then asks
to run a remediation command. The remediation matches the `destructive-shell`
policy rule, so the run pauses until a human approves (or denies) it:

    agentd approvals
    agentd approve <run-id> <step-key>

Durability demo: kill the agent mid-run, then re-run it with the same --run-id.
Completed steps (LLM calls, tool executions) replay from the journal instead of
running again:

    python examples/sre_agent.py --run-id triage-1 --crash-after 2
    python examples/sre_agent.py --run-id triage-1

Environment: AGENTD_URL, AGENTD_API_TOKEN, AGENTD_MODEL (default "mock"; use a
real model such as "claude-sonnet-4-5" when the server has a provider key).
"""

from __future__ import annotations

import argparse
import os
import sys
import uuid
from collections.abc import Callable
from typing import Any

from agentd import Budget, Client, Run, RunFailed, StepDenied, tool_result_block

SYSTEM = (
    "You are an SRE triage agent. Use the tools to inspect the host, then reply "
    "with a short summary of what you found. Do not run destructive commands."
)
REMEDIATION = "rm -rf /tmp/agentd-demo-cache && echo cache cleared"
MAX_TURNS = 6


def make_agent(model: str, crash_after: int | None) -> Callable[[Run, Any], dict[str, Any]]:
    steps = 0

    def checkpoint(run: Run, what: str, replayed: bool) -> None:
        nonlocal steps
        steps += 1
        print(f"  [{steps}] {what} ({'replayed' if replayed else 'executed'})", flush=True)
        if crash_after is not None and steps >= crash_after:
            print(f"\n*** simulated crash after {steps} steps; re-run with --run-id {run.id}")
            sys.stdout.flush()
            os._exit(1)  # a hard crash: no cleanup, the run stays "running"

    def agent(run: Run, inp: dict[str, Any]) -> dict[str, Any]:
        # Shell first so the deterministic mock model reaches for it.
        tools = sorted(run.client.tools(), key=lambda t: t["name"] != "shell")
        messages: list[dict[str, Any]] = [{"role": "user", "content": inp["task"]}]

        summary = ""
        for _ in range(MAX_TURNS):
            resp = run.llm(model, messages, system=SYSTEM, tools=tools, max_tokens=1024)
            checkpoint(run, f"llm {resp.model} -> {resp.stop_reason}", resp.replayed)
            messages.append(resp.message())
            if not resp.tool_calls:
                summary = resp.text
                break
            results = []
            for call in resp.tool_calls:
                try:
                    out = run.tool(call.name, call.input)
                    checkpoint(run, f"tool {call.name} exit={out.exit_code}", out.replayed)
                    results.append(tool_result_block(call, out))
                except StepDenied as e:
                    checkpoint(run, f"tool {call.name} denied ({e.code})", False)
                    results.append(tool_result_block(call, f"denied: {e}", is_error=True))
            messages.append({"role": "user", "content": results})

        print(f"\nsummary:\n{summary}\n", flush=True)
        print(f"remediation {REMEDIATION!r} needs human approval:")
        print(f"  agentd approvals  &&  agentd approve {run.id} <step-key>", flush=True)
        try:
            fix = run.tool("shell", {"command": REMEDIATION})
            checkpoint(run, f"remediation exit={fix.exit_code}", fix.replayed)
            remediation = fix.stdout.strip() or fix.output_text
        except StepDenied as e:
            print(f"  remediation not applied: {e}")
            remediation = f"not applied ({e.code})"
        return {"summary": summary, "remediation": remediation}

    return agent


def main() -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawTextHelpFormatter)
    p.add_argument("--run-id", help="name of the run; re-use it to resume (default: random)")
    p.add_argument("--task", default="Check the host's OS, kernel and disk usage.")
    p.add_argument("--model", default=os.environ.get("AGENTD_MODEL", "mock"))
    p.add_argument("--crash-after", type=int, help="simulate a crash after N steps")
    p.add_argument("--max-cost", type=float, default=0.50, help="USD budget for real models")
    args = p.parse_args()

    run_id = args.run_id or f"triage-{uuid.uuid4().hex[:8]}"
    client = Client()
    print(f"run {run_id} on {client.base_url} (model={args.model})", flush=True)
    budget = Budget(
        max_steps=20,
        max_tokens=200_000,
        max_cost_usd=None if args.model == "mock" else args.max_cost,
    )
    try:
        out = client.execute(
            make_agent(args.model, args.crash_after),
            agent="sre-triage",
            input={"task": args.task},
            run_id=run_id,
            budget=budget,
        )
    except RunFailed as e:
        print(f"run failed: {e}", file=sys.stderr)
        return 2
    info = client.get_run(run_id)
    print(f"\nrun {info.status}  trace_id={info.trace_id}  usage={info.usage}")
    print(f"output: {out}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
