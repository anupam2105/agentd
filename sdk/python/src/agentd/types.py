"""Typed views over step results."""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from typing import Any


@dataclass(frozen=True)
class Budget:
    """Limits for a run. ``None`` (or 0) means unlimited."""

    max_steps: int | None = None
    max_tokens: int | None = None
    max_cost_usd: float | None = None

    def to_json(self) -> dict[str, Any]:
        out: dict[str, Any] = {}
        if self.max_steps:
            out["max_steps"] = self.max_steps
        if self.max_tokens:
            out["max_tokens"] = self.max_tokens
        if self.max_cost_usd:
            out["max_cost_usd"] = self.max_cost_usd
        return out


@dataclass(frozen=True)
class Usage:
    input_tokens: int = 0
    output_tokens: int = 0
    cost_usd: float = 0.0
    steps: int = 0

    @classmethod
    def from_json(cls, d: dict[str, Any] | None) -> Usage:
        d = d or {}
        return cls(
            input_tokens=int(d.get("input_tokens", 0)),
            output_tokens=int(d.get("output_tokens", 0)),
            cost_usd=float(d.get("cost_usd", 0.0)),
            steps=int(d.get("steps", 0)),
        )


@dataclass(frozen=True)
class ToolCall:
    """A tool invocation requested by the model."""

    id: str
    name: str
    input: dict[str, Any]


@dataclass(frozen=True)
class LLMResponse:
    """A model response in Anthropic content-block format (for every provider)."""

    content: list[dict[str, Any]]
    stop_reason: str
    model: str
    provider: str
    usage: Usage
    replayed: bool = False

    @classmethod
    def from_result(cls, result: dict[str, Any], replayed: bool) -> LLMResponse:
        return cls(
            content=list(result.get("content") or []),
            stop_reason=str(result.get("stop_reason", "")),
            model=str(result.get("model", "")),
            provider=str(result.get("provider", "")),
            usage=Usage.from_json(result.get("usage")),
            replayed=replayed,
        )

    @property
    def text(self) -> str:
        return "".join(b.get("text", "") for b in self.content if b.get("type") == "text")

    @property
    def tool_calls(self) -> list[ToolCall]:
        return [
            ToolCall(id=b["id"], name=b["name"], input=dict(b.get("input") or {}))
            for b in self.content
            if b.get("type") == "tool_use"
        ]

    def message(self) -> dict[str, Any]:
        """The assistant message to append to the conversation."""
        return {"role": "assistant", "content": self.content}


@dataclass(frozen=True)
class ToolResult:
    """Outcome of a sandboxed tool execution."""

    exit_code: int
    stdout: str
    stderr: str
    timed_out: bool = False
    truncated: bool = False
    duration_ms: int = 0
    replayed: bool = False

    @classmethod
    def from_result(cls, result: dict[str, Any], replayed: bool) -> ToolResult:
        return cls(
            exit_code=int(result.get("exit_code", 0)),
            stdout=str(result.get("stdout", "")),
            stderr=str(result.get("stderr", "")),
            timed_out=bool(result.get("timed_out", False)),
            truncated=bool(result.get("truncated", False)),
            duration_ms=int(result.get("duration_ms", 0)),
            replayed=replayed,
        )

    @property
    def ok(self) -> bool:
        return self.exit_code == 0 and not self.timed_out

    @property
    def output_text(self) -> str:
        """A compact rendering suitable for feeding back to a model."""
        parts = [f"exit_code={self.exit_code}"]
        if self.timed_out:
            parts.append("timed_out=true")
        if self.truncated:
            parts.append("output_truncated=true")
        text = " ".join(parts)
        if self.stdout:
            text += f"\nstdout:\n{self.stdout}"
        if self.stderr:
            text += f"\nstderr:\n{self.stderr}"
        return text


def tool_result_block(
    call: ToolCall, result: ToolResult | str, is_error: bool = False
) -> dict[str, Any]:
    """Build a ``tool_result`` content block answering ``call``."""
    if isinstance(result, ToolResult):
        content = result.output_text
        is_error = is_error or not result.ok
    else:
        content = result
    block: dict[str, Any] = {"type": "tool_result", "tool_use_id": call.id, "content": content}
    if is_error:
        block["is_error"] = True
    return block


@dataclass
class RunInfo:
    """Server-side view of a run."""

    id: str
    agent: str
    status: str
    usage: Usage
    trace_id: str = ""
    output: Any = None
    error: str = ""
    raw: dict[str, Any] = field(default_factory=dict, repr=False)

    @classmethod
    def from_json(cls, d: dict[str, Any]) -> RunInfo:
        return cls(
            id=d["id"],
            agent=d["agent"],
            status=d["status"],
            usage=Usage.from_json(d.get("usage")),
            trace_id=d.get("trace_id", ""),
            output=d.get("output"),
            error=d.get("error", ""),
            raw=d,
        )


def dumps(v: Any) -> str:
    return json.dumps(v, separators=(",", ":"), sort_keys=True)
