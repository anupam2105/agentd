"""HTTP client for the agentd API. Standard library only, so the SDK has no runtime deps."""

from __future__ import annotations

import json
import urllib.error
import urllib.request
from typing import Any


class AgentdError(Exception):
    """Raised when agentd is unreachable or answers with a non-2xx status."""

    def __init__(self, message: str, status: int | None = None) -> None:
        super().__init__(message)
        self.status = status


class Client:
    def __init__(self, base_url: str, token: str | None = None, timeout: float = 30.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.timeout = timeout

    def health(self) -> bool:
        return self._request("GET", "/healthz").get("status") == "ok"

    def version(self) -> str:
        return str(self._request("GET", "/v1/version")["version"])

    def _request(self, method: str, path: str, body: Any = None) -> dict[str, Any]:
        data = None if body is None else json.dumps(body).encode()
        req = urllib.request.Request(self.base_url + path, data=data, method=method)
        req.add_header("Accept", "application/json")
        if data is not None:
            req.add_header("Content-Type", "application/json")
        if self.token:
            req.add_header("Authorization", f"Bearer {self.token}")
        try:
            with urllib.request.urlopen(req, timeout=self.timeout) as resp:
                payload = resp.read()
        except urllib.error.HTTPError as e:
            raise AgentdError(f"{method} {path}: HTTP {e.code}", status=e.code) from e
        except urllib.error.URLError as e:
            raise AgentdError(f"{method} {path}: {e.reason}") from e
        result: dict[str, Any] = json.loads(payload) if payload else {}
        return result
