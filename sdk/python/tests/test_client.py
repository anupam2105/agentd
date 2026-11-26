import json
import threading
from collections.abc import Iterator
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from agentd import AgentdError, Client

seen_auth: list[str | None] = []


class _Handler(BaseHTTPRequestHandler):
    def do_GET(self) -> None:
        seen_auth.append(self.headers.get("Authorization"))
        routes = {"/healthz": {"status": "ok"}, "/v1/version": {"version": "dev", "api": "v1"}}
        if self.path not in routes:
            self.send_response(404)
            self.end_headers()
            return
        body = json.dumps(routes[self.path]).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args: object) -> None:
        pass


@pytest.fixture
def base_url() -> Iterator[str]:
    srv = HTTPServer(("127.0.0.1", 0), _Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{srv.server_port}"
    srv.shutdown()


def test_health_version_and_auth_header(base_url: str) -> None:
    seen_auth.clear()
    c = Client(base_url, token="s3cret")
    assert c.health() is True
    assert c.version() == "dev"
    assert seen_auth == ["Bearer s3cret", "Bearer s3cret"]


def test_http_error_carries_status(base_url: str) -> None:
    with pytest.raises(AgentdError) as exc:
        Client(base_url)._request("GET", "/missing")
    assert exc.value.status == 404


def test_unreachable_server() -> None:
    with pytest.raises(AgentdError) as exc:
        Client("http://127.0.0.1:1", timeout=1).health()
    assert exc.value.status is None
