# Security

## Reporting a vulnerability

Please **do not open a public GitHub issue**. Open a private security advisory via the repository's
Security tab. Expected acknowledgement: within 3 business days.

## Threat model

agentd executes actions chosen by an LLM. Treat model output, tool input and tool output as
untrusted (prompt injection can make a model request anything).

### What agentd enforces

- **Tools are deny-by-default.** A tool must be defined in the config *and* allowed by policy.
  Tool input is validated against the tool's JSON Schema before policy runs.
- **Sandbox per call.** Each tool call runs in a new container that is removed afterwards:
  no network unless the tool sets `network: true`, read-only root filesystem (writable `/tmp`
  tmpfs only), all capabilities dropped, `no-new-privileges`, uid 65534, memory / CPU / PID limits,
  a wall-clock timeout and capped output. Inputs are passed as argv (Go templates) and as the
  `AGENTD_TOOL_INPUT` env var, never through a host shell.
- **Policy before execution**, including human approval for risky actions. Decisions are
  recorded on the step (rule, decided by, reason).
- **Budgets fail closed.** Worst-case usage is reserved before an LLM call; models without
  pricing are refused when a cost budget is set.
- **API auth:** a bearer token compared in constant time; 4 MiB request bodies; unknown JSON
  fields rejected.

### What you must do

- **Set `AGENTD_API_TOKEN`** and terminate TLS in front of agentd. Anyone holding the token can
  start runs, read the journal (prompts, tool output) and approve steps. v0.1 has a single token,
  with no per-user roles.
- **Protect the Docker endpoint.** agentd needs a Docker Engine API to start tool containers.
  Whatever can reach that API is root-equivalent on the machine running the daemon:
  - `docker-compose` demo / `sandbox.mode=hostSocket`: agentd runs as root with the host socket.
    Fine on a laptop; avoid on shared hosts.
  - Helm default `sandbox.mode=dind`: a **privileged** Docker-in-Docker sidecar. Tool containers
    are isolated from the node's runtime, but the sidecar itself is privileged. Run it on a
    dedicated node pool if your cluster policy allows it at all.
  - `sandbox.mode=external`: point `AGENTD_DOCKER_HOST` at a dedicated, disposable sandbox VM.
    This is the strongest option.
  - Containers share the daemon's kernel. For hostile workloads, configure the daemon with a
    stronger runtime (gVisor `runsc`, Kata).
- **Treat `network: true` tools as egress.** Gate them with `require_approval` (as the example
  config does) or an egress proxy.
- **The journal holds sensitive data** (prompts, model output, tool output). Protect the SQLite
  file / PVC accordingly.
- agentd only governs side effects that go **through** it. Agent code running on your machine
  can still do anything that machine can do.

## Supply chain

- Container image: `gcr.io/distroless/static-debian12:nonroot`, static binary, multi-arch.
- Python SDK has zero runtime dependencies.
- CI runs `golangci-lint`, `go test -race`, `govulncheck` and `gitleaks` on every push.
