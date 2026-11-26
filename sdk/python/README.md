# agentd Python SDK

Python client for [agentd](https://github.com/anupam2105/agentd). Pre-alpha: M0 ships the client
skeleton only; durable runs, LLM steps and sandboxed tools arrive in later milestones.

```python
from agentd import Client

client = Client("http://localhost:7700", token="...")
print(client.health(), client.version())
```

Development:

```bash
python3 -m venv .venv && . .venv/bin/activate
pip install -e '.[dev]'
pytest && ruff check . && mypy
```
