# agentd Helm chart

Deploys [agentd](https://github.com/anupam2105/agentd), a durable, sandboxed, policy-governed
runtime for AI agents.

## Prerequisites

- Kubernetes 1.26+, Helm 3.12+
- A default StorageClass (or `persistence.storageClass`) for the SQLite journal
- For the default `sandbox.mode=dind`: privileged pods must be allowed in the namespace

## Install

```bash
kubectl create namespace agents
kubectl -n agents create secret generic agentd \
  --from-literal=api-token="$(openssl rand -hex 24)" \
  --from-literal=anthropic-api-key=sk-ant-...        # optional; also openai-api-key, slack-webhook-url

helm install agentd ./deploy/helm/agentd -n agents --set existingSecret=agentd
helm test agentd -n agents
```

## Tools, pricing and policy

`config` is rendered verbatim into `agentd.yaml` (same schema as
[`agentd.example.yaml`](../../../agentd.example.yaml)). Override it in your values file, or set
`existingConfigMap` (key `agentd.yaml`).

## Sandbox modes

| `sandbox.mode` | Where tool containers run | Notes |
|---|---|---|
| `dind` (default) | Docker-in-Docker sidecar in the agentd pod | Works on any cluster; sidecar is **privileged** |
| `hostSocket` | Node's Docker daemon via `/var/run/docker.sock` | Nodes must run Docker; root-equivalent on the node |
| `external` | `sandbox.external.dockerHost`, e.g. a dedicated VM | Strongest isolation; you secure the endpoint |
| `none` | nowhere | Tool steps fail; LLM-only agents |

See [SECURITY.md](../../../SECURITY.md).

## Notable values

| Key | Default | |
|---|---|---|
| `existingSecret` | `""` | keys: `api-token`, `anthropic-api-key`, `openai-api-key`, `slack-webhook-url` |
| `config` | shell tool + policy | tools / pricing / policy |
| `sandbox.mode` | `dind` | see above |
| `tracing.otlpEndpoint` | `""` | e.g. `http://otel-collector.observability:4318` |
| `server.publicURL` | `""` | linked from approval notifications |
| `persistence.size` | `2Gi` | the PVC is kept on uninstall (`helm.sh/resource-policy: keep`) |
| `serviceMonitor.enabled` | `false` | Prometheus Operator |

agentd runs as a single replica with a `Recreate` strategy (SQLite is single-writer).
