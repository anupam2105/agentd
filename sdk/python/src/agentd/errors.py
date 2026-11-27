"""Exceptions raised by the agentd SDK."""

from __future__ import annotations

from typing import Any


class AgentdError(Exception):
    """Base error. ``status`` is the HTTP status (None for transport errors)."""

    def __init__(
        self,
        message: str,
        status: int | None = None,
        code: str | None = None,
        retryable: bool = False,
    ) -> None:
        super().__init__(message)
        self.status = status
        self.code = code
        self.retryable = retryable


class TransportError(AgentdError):
    """agentd could not be reached. The run is left resumable."""


class NonDeterminismError(AgentdError):
    """A step key was replayed with a different request.

    Agent code must issue the same steps in the same order when a run resumes.
    Wrap non-deterministic values (time, randomness, external reads) in
    ``run.side_effect``.
    """


class StepError(AgentdError):
    """A step did not produce a result."""

    def __init__(self, message: str, step: dict[str, Any], code: str | None = None) -> None:
        super().__init__(message, code=code)
        self.step = step
        self.key: str = step.get("key", "")


class StepDenied(StepError):
    """Policy, a human, or run cancellation refused the step."""


class BudgetExceeded(StepDenied):
    """The step would exceed the run's step, token, or cost budget."""


class StepFailed(StepError):
    """The step failed permanently (e.g. invalid tool input, provider rejected the request)."""


class ApprovalTimeout(StepError):
    """No human decision arrived in time. The run stays resumable."""


class RunFailed(AgentdError):
    """The run already finished as failed or cancelled."""
