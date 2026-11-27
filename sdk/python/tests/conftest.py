"""Starts a real agentd binary for the SDK tests.

Set AGENTD_BIN to a prebuilt binary; otherwise the tests build one with `go`.
"""

from __future__ import annotations

import os
import shutil
import socket
import subprocess
import time
import urllib.request
from collections.abc import Iterator
from pathlib import Path

import pytest

from agentd import Client

REPO = Path(__file__).resolve().parents[3]

CONFIG = """
pricing:
  "mock-priced*": { input: 1000, output: 1000 }
tools:
  echo:
    description: echo a message
    image: alpine:3.22
    command: ["echo", "{{.msg}}"]
    input_schema:
      type: object
      required: [msg]
      properties: { msg: { type: string } }
policy:
  rules:
    - { name: gate, kind: tool, tool: echo, input: { msg: "^danger" }, action: require_approval }
    - { name: echo, kind: tool, tool: echo, action: allow }
"""


def _binary(tmp: Path) -> str:
    if os.environ.get("AGENTD_BIN"):
        return os.environ["AGENTD_BIN"]
    if shutil.which("go") is None:
        pytest.skip("set AGENTD_BIN or install Go to run the SDK tests")
    out = tmp / "agentd"
    subprocess.run(["go", "build", "-o", str(out), "./cmd/agentd"], cwd=REPO, check=True)
    return str(out)


def _free_port() -> int:
    with socket.socket() as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


@pytest.fixture(scope="session")
def server(tmp_path_factory: pytest.TempPathFactory) -> Iterator[str]:
    tmp = tmp_path_factory.mktemp("agentd")
    (tmp / "agentd.yaml").write_text(CONFIG)
    port = _free_port()
    env = {
        **os.environ,
        "AGENTD_HTTP_ADDR": f"127.0.0.1:{port}",
        "AGENTD_DB_PATH": str(tmp / "agentd.db"),
        "AGENTD_CONFIG": str(tmp / "agentd.yaml"),
        "AGENTD_API_TOKEN": "test-token",
        "AGENTD_LOG_LEVEL": "warn",
    }
    proc = subprocess.Popen([_binary(tmp)], env=env)
    url = f"http://127.0.0.1:{port}"
    for _ in range(100):
        try:
            urllib.request.urlopen(url + "/healthz", timeout=1).close()
            break
        except OSError:
            time.sleep(0.1)
    else:
        proc.kill()
        raise RuntimeError("agentd did not start")
    yield url
    proc.terminate()
    proc.wait(timeout=10)


@pytest.fixture
def client(server: str) -> Client:
    return Client(server, token="test-token", max_retries=2)
