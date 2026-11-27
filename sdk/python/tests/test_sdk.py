from __future__ import annotations

import os
import threading
import time
import uuid
from typing import Any

import pytest

from agentd import (
    AgentdError,
    Budget,
    BudgetExceeded,
    Client,
    NonDeterminismError,
    Run,
    StepDenied,
    TransportError,
    tool_result_block,
)

needs_docker = pytest.mark.skipif(
    os.environ.get("AGENTD_DOCKER_TESTS") != "1", reason="set AGENTD_DOCKER_TESTS=1"
)


def rid() -> str:
    return "t-" + uuid.uuid4().hex[:12]


def ask(run: Run, q: str) -> str:
    return run.llm("mock", [{"role": "user", "content": q}]).text


def test_health_and_auth(server: str, client: Client) -> None:
    assert client.health()
    assert client.version()
    with pytest.raises(AgentdError) as e:
        Client(server, token="wrong").tools()
    assert e.value.status == 401


def test_execute_replays_after_crash(client: Client) -> None:
    run_id = rid()
    calls: list[str] = []
    crashed: list[bool] = []

    def agent(run: Run, inp: dict[str, Any]) -> dict[str, Any]:
        a = ask(run, inp["q"])
        token = run.side_effect(lambda: (calls.append("side"), uuid.uuid4().hex)[1])
        if not crashed:
            crashed.append(True)
            raise TransportError("simulated crash", retryable=True)
        b = ask(run, "second")
        return {"a": a, "b": b, "token": token}

    with pytest.raises(TransportError):
        client.execute(agent, agent="demo", input={"q": "hello"}, run_id=run_id)
    assert client.get_run(run_id).status == "running"

    out = client.execute(agent, agent="demo", input={"q": "ignored on resume"}, run_id=run_id)
    assert out["a"] == "Mock response to: hello"
    assert calls == ["side"], "side effect must run once"
    steps = client.steps(run_id)
    assert [s["key"] for s in steps] == ["1:llm", "2:record", "3:llm"]

    again = client.execute(lambda r, i: pytest.fail("must not rerun"), agent="demo", run_id=run_id)
    assert again == out
    assert client.get_run(run_id).usage.steps == 2


def test_nondeterminism_detected(client: Client) -> None:
    run_id = rid()
    ask(client.run("demo", run_id=run_id), "one")
    with pytest.raises(NonDeterminismError):
        ask(client.run("demo", run_id=run_id), "two")


def test_budget_exceeded_fails_run(client: Client) -> None:
    run_id = rid()

    def agent(run: Run, _: Any) -> None:
        ask(run, "1")
        ask(run, "2")

    with pytest.raises(BudgetExceeded) as e:
        client.execute(agent, agent="demo", run_id=run_id, budget=Budget(max_steps=1))
    assert e.value.code == "budget_exceeded_steps"
    assert client.get_run(run_id).status == "failed"


def test_cost_budget(client: Client) -> None:
    with client.run("demo", budget=Budget(max_cost_usd=0.01)) as run, pytest.raises(BudgetExceeded):
        run.llm("mock-priced", [{"role": "user", "content": "x"}], provider="mock")


def test_tool_denied_by_default_policy(client: Client) -> None:
    run = client.run("demo")
    with pytest.raises(StepDenied) as e:
        run.tool("nope", {})
    assert e.value.code == "unknown_tool"


def test_approval_denied(client: Client) -> None:
    run = client.run("demo", approval_timeout=30)

    def deny() -> None:
        for _ in range(100):
            for a in client.approvals():
                if a["run_id"] == run.id:
                    client.decide(run.id, a["key"], approve=False, by="pytest", reason="nope")
                    return
            time.sleep(0.1)

    t = threading.Thread(target=deny)
    t.start()
    with pytest.raises(StepDenied) as e:
        run.tool("echo", {"msg": "danger zone"})
    t.join()
    assert e.value.code == "approval_denied"


def test_transport_error() -> None:
    with pytest.raises(TransportError):
        Client("http://127.0.0.1:1", max_retries=0).run("demo")


def test_mock_agent_loop_builds_tool_messages(client: Client) -> None:
    tools = client.tools()
    assert tools[0]["name"] == "echo"
    run = client.run("demo")
    resp = run.llm("mock", [{"role": "user", "content": "go"}], tools=tools)
    assert resp.stop_reason == "tool_use"
    call = resp.tool_calls[0]
    block = tool_result_block(call, "hi")
    assert block == {"type": "tool_result", "tool_use_id": call.id, "content": "hi"}


@needs_docker
def test_tool_execution_and_approval(client: Client) -> None:
    run = client.run("demo", approval_timeout=60)
    res = run.tool("echo", {"msg": "hello"})
    assert res.ok and res.stdout == "hello\n" and not res.replayed

    def approve() -> None:
        for _ in range(100):
            for a in client.approvals():
                if a["run_id"] == run.id:
                    client.decide(run.id, a["key"], approve=True, by="pytest")
                    return
            time.sleep(0.1)

    t = threading.Thread(target=approve)
    t.start()
    res = run.tool("echo", {"msg": "danger ok"})
    t.join()
    assert res.stdout == "danger ok\n"
