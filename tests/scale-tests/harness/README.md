# NVSentinel Scale-Test Harness

KWOK-based, reproducible scale testing for NVSentinel. The operator CLI
(`harnessctl`) installs the scale stack and drives simulated node fleets:

| Capability | Command |
|------------|---------|
| Idempotent bring-up (kube-prometheus-stack, metrics-server, KWOK, cert-manager, NVSentinel) | `harnessctl stack bringup` |
| Scale up GPU-shaped KWOK nodes | `harnessctl nodes scale --count N` |


## Layout

```
harness/
├── values/                     # complete values for multi-node clusters
│   ├── values-nvsentinel.yaml
│   └── values-monitoring.yaml
├── kind/                       # local Kind cluster + complete Helm values
│   ├── kind-cluster.yaml
│   ├── values-nvsentinel.yaml
│   └── values-monitoring.yaml
└── harnessctl/                 # Go CLI
    ├── assets/                 # embedded KWOK stages (values are not embedded)
    ├── install_*.go
    └── installer               # multi-arch curl installer
```

## Install harnessctl

```bash
curl -fsSL https://raw.githubusercontent.com/nvidia/nvsentinel/<tag>/tests/scale-tests/harness/harnessctl/installer | bash
# pin a version:
HARNESSCTL_VERSION=vX.Y.Z curl -fsSL https://raw.githubusercontent.com/nvidia/nvsentinel/<tag>/tests/scale-tests/harness/harnessctl/installer | bash
```

The installer picks `linux|darwin|windows` × `amd64|arm64` and installs to
`/usr/local/bin` (override with `-d` / `INSTALL_DIR`).

Or build from source (from the NVSentinel repo root):

```bash
cd tests/scale-tests/harness/harnessctl && CGO_ENABLED=0 go build -o harnessctl .
```

## Quick start

Run from `tests/scale-tests/harness/` (paths below are relative to that directory).
Use `values/` for a multi-node cluster or `kind/` for a local Kind cluster.

```bash
cd tests/scale-tests/harness

harnessctl stack bringup \
  --nvs-chart-version v1.16.0 \
  --kwok-version v0.6.1 \
  --cert-manager-version v1.16.2 \
  --metrics-server-version v0.7.2 \
  --kps-chart-version 65.5.0 \
  --nvsentinel-values values/values-nvsentinel.yaml \
  --monitoring-values values/values-monitoring.yaml
harnessctl nodes scale --count 200
# artifacts default to $TMPDIR/nvsentinel-harness/results
```

On managed clusters that require a `spec.providerID`, pass `--provider-id-scheme kwok`.

## Kind smoke

Requires Docker and the `kind` CLI.

```bash
cd tests/scale-tests/harness

kind create cluster --config kind/kind-cluster.yaml
harnessctl stack bringup \
  --nvs-chart-version v1.16.0 \
  --kwok-version v0.6.1 \
  --cert-manager-version v1.16.2 \
  --metrics-server-version v0.7.2 \
  --kps-chart-version 65.5.0 \
  --nvsentinel-values kind/values-nvsentinel.yaml \
  --monitoring-values kind/values-monitoring.yaml
harnessctl nodes scale --count 50
```

## Prerequisites

- A Kubernetes cluster reachable via your kubeconfig
- Network access for Helm chart pulls during `stack bringup`
- `go` (only if building from source)
- Docker and `kind` (only for Kind smoke)
- If the cluster is Argo-managed, disable auto-sync before `stack bringup`

## Commands

Noun-verb: `harnessctl <group> <command> [--flags]`. Legacy aliases
`bringup` and `scale-nodes` still work. All inputs are flags (`-h` lists them);
there is no config/env file.

| Group | Flags | Used by |
|-------|-------|---------|
| namespaces | `--nvs-namespace`, `--monitoring-namespace`, `--kwok-namespace`, `--janitor-namespace` (detection only), `--cert-manager-namespace` | `stack bringup` |
| target (**required**) | `--count` | `nodes scale` |
| results | `--results-dir` (default `$TMPDIR/nvsentinel-harness/results`) | `nodes scale` |
| KWOK stages | `--job-complete-delay` | `stack bringup` |
| prometheus | `--monitoring-namespace`, `--prom-service`, `--prom-port` | `nodes scale` |
| readiness | `--node-ready-timeout` | `nodes scale` |
| node shape | `--node-prefix`, `--gpu-count`, `--node-cpu`, `--node-memory`, `--node-max-pods`, `--node-batch`, `--provider-id-scheme` | `nodes scale` |
| version targets (**required**) | `--nvs-chart-version`, `--kwok-version`, `--cert-manager-version`, `--metrics-server-version`, `--kps-chart-version` (+ optional `--nvs-chart`) | `stack bringup` |
| values files (**required**) | `--nvsentinel-values`, `--monitoring-values` | `stack bringup` |

Bringup installs/upgrades a component only when it is missing or its installed
version does not match the requested one. Prometheus is queried through the API
server service proxy (no port-forward).
