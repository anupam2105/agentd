"""agentd client and durable run API. Standard library only."""

from __future__ import annotations

import json
import logging
import os
import random
import time
import urllib.error
import urllib.parse
import urllib.request
from collections.abc import Callable
from types import TracebackType
from typing import Any, TypeVar

from agentd.errors import (
    AgentdError,
    ApprovalTimeout,
    BudgetExceeded,
    NonDeterminismError,
    RunFailed,
    StepDenied,
    StepFailed,
    TransportError,
)
from agentd.types import Budget, LLMResponse, RunInfo, ToolResult

log = logging.getLogger("agentd")

T = TypeVar("T")

_RETRYABLE_STATUS = {429, 502, 503, 504}


class Client:
    """Connection to an agentd server.

    ``base_url`` and ``token`` default to ``AGENTD_URL`` and ``AGENTD_API_TOKEN``.
    """

    def __init__(
        self,
        base_url: str | None = None,
        token: str | None = None,
        timeout: float = 900.0,
        max_retries: int = 8,
    ) -> None:
        self.base_url = (
            base_url or os.environ.get("AGENTD_URL") or "http://localhost:7700"
        ).rstrip("/")
        self.token = token if token is not None else os.environ.get("AGENTD_API_TOKEN")
        self.timeout = timeout
        self.max_retries = max_retries

    # ------------------------------------------------------------------ API

    def health(self) -> bool:
        return self.request("GET", "/healthz").get("status") == "ok"

    def version(self) -> str:
        return str(self.request("GET", "/v1/version")["version"])

    def tools(self) -> list[dict[str, Any]]:
        """Tool definitions in the shape ``Run.llm(tools=...)`` expects."""
        return [
            {
                "name": t["name"],
                "description": t.get("description", ""),
                "input_schema": t["input_schema"],
            }
            for t in self.request("GET", "/v1/tools")["tools"]
        ]

    def get_run(self, run_id: str) -> RunInfo:
        return RunInfo.from_json(self.request("GET", f"/v1/runs/{_q(run_id)}"))

    def steps(self, run_id: str) -> list[dict[str, Any]]:
        steps: list[dict[str, Any]] = self.request("GET", f"/v1/runs/{_q(run_id)}/steps")["steps"]
        return steps

    def approvals(self) -> list[dict[str, Any]]:
        out: list[dict[str, Any]] = self.request("GET", "/v1/approvals")["approvals"]
        return out

    def decide(
        self, run_id: str, key: str, approve: bool, by: str = "sdk", reason: str = ""
    ) -> None:
        self.request(
            "POST",
            f"/v1/runs/{_q(run_id)}/steps/{_q(key)}/decision",
            {"approve": approve, "by": by, "reason": reason},
        )

    def cancel(self, run_id: str, reason: str = "") -> RunInfo:
        return RunInfo.from_json(
            self.request("POST", f"/v1/runs/{_q(run_id)}/cancel", {"reason": reason})
        )

    def run(
        self,
        agent: str,
        *,
        run_id: str | None = None,
        input: Any = None,
        budget: Budget | None = None,
        approval_timeout: float = 3600.0,
    ) -> Run:
        """Start a run, or attach to an existing one when ``run_id`` is given."""
        body: dict[str, Any] = {"agent": agent, "budget": (budget or Budget()).to_json()}
        if run_id:
            body["id"] = run_id
        if input is not None:
            body["input"] = input
        info = RunInfo.from_json(self._with_retries(lambda: self.request("POST", "/v1/runs", body)))
        return Run(self, info, approval_timeout=approval_timeout)

    def execute(
        self,
        fn: Callable[[Run, Any], T],
        *,
        agent: str,
        input: Any = None,
        run_id: str | None = None,
        budget: Budget | None = None,
        approval_timeout: float = 3600.0,
    ) -> T:
        """Run ``fn(run, input)`` durably.

        Re-executing with the same ``run_id`` after a crash replays completed
        steps from the journal and continues where the run left off. If the run
        already completed, its stored output is returned without calling ``fn``.
        """
        run = self.run(
            agent, run_id=run_id, input=input, budget=budget, approval_timeout=approval_timeout
        )
        if run.info.status == "completed":
            out: T = run.info.output
            return out
        if run.info.status in ("failed", "cancelled"):
            raise RunFailed(f"run {run.id} is {run.info.status}: {run.info.error}")
        stored_input = run.info.raw.get("input", input)
        try:
            result = fn(run, stored_input)
        except (TransportError, ApprovalTimeout):
            raise  # leave the run resumable
        except Exception as e:
            try:
                run.fail(f"{type(e).__name__}: {e}")
            except AgentdError:
                log.warning("could not mark run %s failed", run.id, exc_info=True)
            raise
        run.complete(result)
        return result

    # ------------------------------------------------------------------ HTTP

    def request(self, method: str, path: str, body: Any = None) -> dict[str, Any]:
        status, payload = self._raw(method, path, body)
        if status >= 400:
            raise _error(method, path, status, payload)
        return payload

    def _raw(self, method: str, path: str, body: Any = None) -> tuple[int, dict[str, Any]]:
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base_url + path, data=data, method=method)
        req.add_header("Accept", "application/json")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if self.token:
            req.add_header("Authorization", f"Bearer {self.token}")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                return resp.status, _decode(resp.read())
        except urllib.error.HTTPError as e:
            return e.code, _decode(e.read())
        except (urllib.error.URLError, TimeoutError, ConnectionError) as e:
            reason = getattr(e, "reason", e)
            raise TransportError(f"{method} {path}: {reason}", retryable=True) from e

    def _with_retries(self, fn: Callable[[], T]) -> T:
        attempt = 0
        while True:
            try:
                return fn()
            except AgentdError as e:
                if not e.retryable or attempt >= self.max_retries:
                    raise
                delay = _backoff(attempt)
                log.info("agentd: %s; retrying in %.1fs", e, delay)
                time.sleep(delay)
                attempt += 1


class Run:
    """A durable agent run. Every side effect goes through a journaled step.

    Steps are keyed by their position in the run (``1:llm``, ``2:tool:shell``,
    ...), so agent code must issue them in a deterministic order.
    """

    def __init__(self, client: Client, info: RunInfo, approval_timeout: float = 3600.0) -> None:
        self.client = client
        self.info = info
        self.approval_timeout = approval_timeout
        self._seq = 0

    @property
    def id(self) -> str:
        return self.info.id

    @property
    def trace_id(self) -> str:
        return self.info.trace_id

    def __enter__(self) -> Run:
        return self

    def __exit__(
        self,
        exc_type: type[BaseException] | None,
        exc: BaseException | None,
        tb: TracebackType | None,
    ) -> None:
        if self.info.status != "running":
            return
        if exc is None:
            self.complete(None)
        elif isinstance(exc, Exception) and not isinstance(exc, (TransportError, ApprovalTimeout)):
            self.fail(f"{type(exc).__name__}: {exc}")

    # ------------------------------------------------------------------ steps

    def llm(
        self,
        model: str,
        messages: list[dict[str, Any]],
        *,
        system: str | None = None,
        tools: list[dict[str, Any]] | None = None,
        max_tokens: int = 1024,
        temperature: float | None = None,
        provider: str | None = None,
    ) -> LLMResponse:
        """Call a model through agentd's gateway (budgeted, policy-checked, journaled)."""
        req: dict[str, Any] = {"model": model, "messages": messages, "max_tokens": max_tokens}
        if system:
            req["system"] = system
        if tools:
            req["tools"] = tools
        if temperature is not None:
            req["temperature"] = temperature
        if provider:
            req["provider"] = provider
        step = self._step("llm", f"{self._next()}:llm", req)
        return LLMResponse.from_result(step["result"], bool(step.get("replayed")))

    def tool(self, name: str, input: dict[str, Any] | None = None) -> ToolResult:
        """Execute a registered tool in agentd's sandbox."""
        step = self._step(
            "tool", f"{self._next()}:tool:{name}", {"tool": name, "input": input or {}}
        )
        return ToolResult.from_result(step["result"], bool(step.get("replayed")))

    def side_effect(self, fn: Callable[[], T], name: str = "record") -> T:
        """Run ``fn`` once and journal its (JSON-serialisable) result.

        On replay the journaled value is returned and ``fn`` is not called. Use
        it for anything non-deterministic: clocks, random IDs, external reads.
        """
        key = f"{self._next()}:{name}"
        existing = self._get_step(key)
        if existing is not None and existing.get("status") == "completed":
            value: T = existing["result"]
            return value
        value = fn()
        step = self._step("record", key, {"value": value})
        stored: T = step["result"]
        return stored

    def complete(self, output: Any = None) -> RunInfo:
        self.info = RunInfo.from_json(
            self.client._with_retries(
                lambda: self.client.request(
                    "POST",
                    f"/v1/runs/{_q(self.id)}/finish",
                    {"status": "completed", "output": output},
                )
            )
        )
        return self.info

    def fail(self, error: str) -> RunInfo:
        self.info = RunInfo.from_json(
            self.client._with_retries(
                lambda: self.client.request(
                    "POST", f"/v1/runs/{_q(self.id)}/finish", {"status": "failed", "error": error}
                )
            )
        )
        return self.info

    def refresh(self) -> RunInfo:
        self.info = self.client.get_run(self.id)
        return self.info

    # ------------------------------------------------------------------ internals

    def _next(self) -> int:
        self._seq += 1
        return self._seq

    def _get_step(self, key: str) -> dict[str, Any] | None:
        try:
            return self.client._with_retries(
                lambda: self.client.request("GET", f"/v1/runs/{_q(self.id)}/steps/{_q(key)}")
            )
        except AgentdError as e:
            if e.status == 404:
                return None
            raise

    def _await_decision(self, key: str, step: dict[str, Any]) -> None:
        log.warning(
            "agentd: step %s of run %s needs approval (policy rule %s). "
            "Approve with: agentd approve %s %s",
            key,
            self.id,
            step.get("policy_rule"),
            self.id,
            key,
        )
        deadline = time.monotonic() + self.approval_timeout
        poll = 0.5
        while True:
            if time.monotonic() >= deadline:
                raise ApprovalTimeout(f"step {key} was not approved in time", step)
            time.sleep(poll)
            poll = min(poll * 1.5, 5.0)
            cur = self._get_step(key)
            if cur is None or cur.get("status") != "awaiting_approval":
                return

    def _step(self, kind: str, key: str, request: dict[str, Any]) -> dict[str, Any]:
        body = {"key": key, "kind": kind, "request": request}
        path = f"/v1/runs/{_q(self.id)}/steps"
        attempt = 0
        while True:
            try:
                status, step = self.client._raw("POST", path, body)
            except TransportError:
                if attempt >= self.client.max_retries:
                    raise
                time.sleep(_backoff(attempt))
                attempt += 1
                continue
            if status >= 400:
                err = _error("POST", path, status, step)
                if err.retryable and attempt < self.client.max_retries:
                    time.sleep(_backoff(attempt))
                    attempt += 1
                    continue
                raise err

            st = step.get("status")
            if st == "completed":
                return step
            if st in ("approved", "running"):
                time.sleep(0.2)  # decided or executing elsewhere; re-post
                continue
            if st == "awaiting_approval":
                self._await_decision(key, step)
                continue  # re-post: executes if approved, returns the denial otherwise
            error = step.get("error") or {}
            code = str(error.get("code", ""))
            msg = f"step {key}: {code}: {error.get('message', '')}"
            if st == "failed" and error.get("retryable") and attempt < self.client.max_retries:
                time.sleep(_backoff(attempt))
                attempt += 1
                continue
            if st == "denied":
                cls = BudgetExceeded if code.startswith("budget_exceeded") else StepDenied
                raise cls(msg, step, code=code)
            raise StepFailed(msg, step, code=code)


def _q(s: str) -> str:
    return urllib.parse.quote(s, safe="")


def _decode(payload: bytes) -> dict[str, Any]:
    if not payload:
        return {}
    try:
        v = json.loads(payload)
    except ValueError:
        return {"error": {"message": payload[:300].decode(errors="replace")}}
    return v if isinstance(v, dict) else {"value": v}


def _error(method: str, path: str, status: int, payload: dict[str, Any]) -> AgentdError:
    e = payload.get("error") or {}
    code = e.get("code")
    msg = f"{method} {path}: HTTP {status}: {e.get('message', '')}".rstrip(": ")
    retryable = bool(e.get("retryable")) or status in _RETRYABLE_STATUS
    if code == "nondeterminism":
        return NonDeterminismError(msg, status=status, code=code)
    return AgentdError(msg, status=status, code=code, retryable=retryable)


def _backoff(attempt: int) -> float:
    return float(min(30.0, 0.5 * 2**attempt) * (0.5 + random.random() / 2))
