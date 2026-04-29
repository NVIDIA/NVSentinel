# NVSentinel Local Demo: Custom Remediation Actions

**Demonstration of NVSentinel's custom remediation action extensibility with a real memory pressure health monitor.**

This demo showcases the end-to-end custom remediation workflow where a custom health monitor detects memory pressure on a node, and a third-party remediation controller automatically reclaims memory by killing offending pods.

> **No GPU Required!** This demo runs on **any laptop** — it demonstrates how to extend NVSentinel beyond GPU fault management to handle any hardware or system fault.

## What You'll Learn

1. **Custom Health Monitors** — How to build a health monitor that sends custom remediation actions via NVSentinel's gRPC interface
2. **Custom Remediation Actions** — How the `CUSTOM` enum + `customRecommendedAction` field routes events to arbitrary CRD templates
3. **Third-Party Controllers** — How to build a remediation controller that implements NVSentinel's status contract
4. **Full Pipeline** — Health event → quarantine → drain → custom CR → remediation → recovery

## Architecture

```text
┌─────────────────────────────────────────────────────────────────┐
│                         Demo Cluster                            │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  Memory Pressure Monitor (DaemonSet)                            │
│  - Reads /proc/meminfo from host                                │
│  - Detects when MemAvailable < threshold                        │
│         │                                                       │
│         │ gRPC (CUSTOM / "RECLAIM_MEMORY")                      │
│         v                                                       │
│  Platform Connectors ──► MongoDB ◄── Fault Quarantine           │
│                                        │ (cordon node)          │
│                                        v                        │
│                                  Node Drainer                   │
│                                        │ (drain pods)           │
│                                        v                        │
│                                Fault Remediation                │
│                                        │ (create CR)            │
│                                        v                        │
│                              MemoryReclaim CR                   │
│                              (custom CRD)                       │
│                                        │                        │
│                              Memory Reclaim Controller           │
│                              (third-party controller)           │
│                                        │                        │
│                              Deletes memory-hog pods            │
│                              Updates CR → NodeReady=True        │
│                                                                 │
│  Trigger: Deploy stress pod (memory-hog) ──► memory pressure    │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

## Prerequisites

- **Docker** — For running KIND ([install](https://docs.docker.com/get-docker/))
- **kubectl** — Kubernetes CLI ([install](https://kubernetes.io/docs/tasks/tools/))
- **kind** — Kubernetes in Docker ([install](https://kind.sigs.k8s.io/docs/user/quick-start/#installation))
- **helm** — Kubernetes package manager ([install](https://helm.sh/docs/intro/install/))
- **ko** — Go container image builder ([install](https://ko.build/install/))
- **go** — Go 1.25+ ([install](https://go.dev/dl/))
- **jq** — JSON processor ([install](https://jqlang.github.io/jq/download/))

**System Requirements:**
- Disk Space: ~10GB free
- Memory: 4GB RAM minimum, 8GB recommended
- CPU: 2 cores minimum

## Quick Start

### Option 1: Automated Demo

```bash
make demo
make cleanup
```

### Option 2: Step-by-Step (Recommended)

```bash
# Step 0: Create cluster, install NVSentinel, build and deploy custom components
make setup

# Step 1: View the healthy cluster
make show-cluster

# Step 2: Deploy a memory-hungry pod to trigger pressure
make trigger

# Step 3: Verify the full remediation pipeline
make verify

# Clean up
make cleanup
```

## What Happens During the Demo

### Phase 0: Setup (00-setup.sh)

1. Creates a KIND cluster with 1 worker node
2. Installs cert-manager and NVSentinel (with custom remediation action configured)
3. Builds the **memory pressure health monitor** and loads it into KIND
4. Builds the **memory reclaim controller** and loads it into KIND
5. Deploys both as Kubernetes workloads

### Phase 1: Show Cluster (01-show-cluster.sh)

Shows the healthy state:
- All nodes Ready and schedulable
- Memory pressure monitor running on the worker node
- Memory reclaim controller watching for maintenance CRs
- Available memory on the worker node

### Phase 2: Trigger Memory Pressure (02-trigger-memory-pressure.sh)

Deploys a `stress` pod that consumes ~300MB of memory on the worker node. The memory pressure health monitor detects `MemAvailable` dropping below the threshold and sends a health event:

```json
{
  "agent": "memory-pressure-monitor",
  "componentClass": "Memory",
  "checkName": "MemoryAvailableCheck",
  "isFatal": true,
  "recommendedAction": 27,
  "customRecommendedAction": "RECLAIM_MEMORY"
}
```

### Phase 3: Verify Remediation (03-verify-remediation.sh)

Verifies the complete pipeline:

1. **Node cordoned** — Fault quarantine detected the fatal event and cordoned the worker
2. **Maintenance CR created** — Fault remediation created a `MemoryReclaim` CR (a custom CRD defined by the demo)
3. **Memory hog deleted** — The memory reclaim controller watched for `MemoryReclaim` CRs and killed the stress pod
4. **CR completed** — Controller updated the CR status with `MemoryReclaimed=True`

## Components

### Memory Pressure Health Monitor

A Go DaemonSet (`memory-pressure-monitor/`) that:
- Reads `/proc/meminfo` from the host (mounted read-only)
- Polls every 10 seconds for `MemAvailable`
- Sends `CUSTOM` / `"RECLAIM_MEMORY"` health events via gRPC when below threshold
- Sends healthy events when memory recovers
- Only sends events on state transitions (avoids duplicates)

### Memory Reclaim Controller

A Go Deployment (`memory-reclaim-controller/`) that:
- Watches `MemoryReclaim` CRs (`demo.nvsentinel.nvidia.com/v1alpha1`) — a custom CRD defined by the demo
- Finds and deletes pods labeled `nvsentinel.nvidia.com/memory-hog: "true"` on the affected node
- Updates CR status condition to `MemoryReclaimed=True`

This is a **reference implementation** of a third-party remediation controller for NVSentinel.

### NVSentinel Configuration

Fault remediation is configured with a custom action:

```yaml
fault-remediation:
  maintenance:
    actions:
      "RECLAIM_MEMORY":
        apiGroup: "demo.nvsentinel.nvidia.com"
        version: "v1alpha1"
        kind: "MemoryReclaim"
        scope: "Cluster"
        completeConditionType: "MemoryReclaimed"
        templateFileName: "reclaim-memory-template.yaml"
        equivalenceGroup: "memory-reclaim"
```

## Extending This Demo

This demo is designed as a starting point. To adapt it for your own use case:

1. **Custom Health Monitor** — Replace the memory check in `memory-pressure-monitor/main.go` with your own detection logic (disk, network, temperature, etc.)
2. **Custom Action Name** — Change `"RECLAIM_MEMORY"` to your action name in the monitor, Helm values, and template
3. **Custom Remediation** — Replace the pod deletion in `memory-reclaim-controller/main.go` with your remediation logic
4. **Custom CRD** — The demo already uses a custom `MemoryReclaim` CRD. Adapt `config/memoryreclaim-crd.yaml` for your use case

## Troubleshooting

### Memory hog not triggering pressure
```bash
# Check available memory on the worker
kubectl top node
# Check monitor logs
kubectl logs -l app=memory-pressure-monitor -n nvsentinel
```

### Node not being cordoned
```bash
# Check fault-quarantine logs
kubectl logs deployment/fault-quarantine -n nvsentinel --tail=20
# Verify event was stored
kubectl exec -n nvsentinel deployment/simple-health-client -- curl -s localhost:8080/health
```

### CR not being created
```bash
# Check fault-remediation logs
kubectl logs deployment/fault-remediation -n nvsentinel --tail=20
# Check for RECLAIM_MEMORY in config
kubectl get configmap fault-remediation -n nvsentinel -o jsonpath='{.data.config\.toml}' | grep RECLAIM
# Check if MemoryReclaim CRD is installed
kubectl get crd memoryreclaims.demo.nvsentinel.nvidia.com
```

### Memory hog not being deleted
```bash
# Check controller logs
kubectl logs deployment/memory-reclaim-controller -n nvsentinel --tail=20
# Check MemoryReclaim CRs
kubectl get memoryreclaims -A
```

## Next Steps

- **[Fault Injection Demo](../local-fault-injection-demo/)** — See GPU fault detection and quarantine
- **[Slinky Drain Demo](../local-slinky-drain-demo/)** — See custom drain extensibility
- **[Configuration Guide](../../docs/configuration/fault-remediation.md)** — Full custom action configuration reference
- **[ADR-036](../../docs/designs/036-custom-remediation-actions.md)** — Design document for custom remediation actions

## License

Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.
Licensed under the Apache License, Version 2.0.
