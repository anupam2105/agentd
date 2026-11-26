# Security

## Reporting a vulnerability

Please **do not open a public GitHub issue**. Open a private security advisory via the repository's
Security tab. Expected acknowledgement: within 3 business days.

## Threat model (summary)

agentd executes actions chosen by an LLM, so its inputs must be treated as untrusted:

- Tool calls run in containers with no network, a read-only root filesystem and resource caps
  unless policy explicitly grants more (M3).
- Unknown tools are denied; policy is evaluated before every step (M4).
- Budgets fail closed: if a limit cannot be checked, the step is refused (M2).
- The `/v1` API requires `AGENTD_API_TOKEN` in any shared environment. Terminate TLS in front of
  agentd.

## What's already in place

- Container image: `gcr.io/distroless/static-debian12:nonroot`.
- Metric route labels come from registered routes only (no cardinality attacks).
- CI runs `govulncheck` and `gitleaks` on every push.
