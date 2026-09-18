# NVSentinel Local Demo: Fault Injection and Recovery

**Detect, protect and remediate, end to end, on a KIND cluster with no GPU hardware.**

This demo breaks a GPU and then gets out of the way. NVSentinel detects the fault, protects the workload by cordoning and draining the node, remediates it with a reboot, and returns it to service once the fault clears — with nothing typed at any point in between.

> **No GPU required.** The GPU is simulated by a fake DCGM hostengine backed by NVML injection. Everything above it — the health monitor, the event pipeline, the quarantine rules, the drain, the repair request — is the code that runs in production clusters, unmodified.

## What You'll Learn

The same three stages the [project README](../../README.md) describes, watched one at a time:

1. **Detect** — gpu-health-monitor polls DCGM, and a fault becomes a health event and a node condition
2. **Protect** — fault-quarantine cordons the node so nothing new lands on it, and node-drainer evicts what was already there
3. **Remediate** — fault-remediation asks for the repair the fault calls for, janitor carries it out, and the node comes back on its own

## The Pipeline

```text
┌───────────────────────────────────────────────────────────────────────────┐
│  KIND cluster                                                             │
│                                                                           │
│  GPU node                                                                 │
│  ┌───────────────────────────────────────────────────────┐                │
│  │  fake DCGM hostengine  ◄── you inject XID 95 here     │                │
│  │          │  :5555                                     │                │
│  │          ▼                                            │                │
│  │  gpu-health-monitor                                   │                │
│  │          │  health event, over a Unix socket          │                │
│  │          ▼                                            │                │
│  │  platform-connectors ─────────────┐                   │                │
│  └───────────────────────────────────┼───────────────────┘                │
│                                      │ write                              │
│                                      ▼                                    │
│                              ┌───────────────┐                            │
│                              │   MongoDB     │                            │
│                              └───────┬───────┘                            │
│                                change stream                              │
│            ┌─────────────────────────┼─────────────────────────┐          │
│            ▼                         ▼                         ▼          │
│    fault-quarantine  ──────►   node-drainer  ──────►  fault-remediation   │
│      cordon the node          evict the pods          request a reboot    │
│                                                                │          │
│                                                                ▼          │
│                                                            janitor        │
│                                                       perform the reboot  │
└───────────────────────────────────────────────────────────────────────────┘
```

The core modules coordinate only through the datastore — none of them calls another — which is why each can be enabled, disabled or replaced on its own. The one direct hop is at the edge: a health monitor hands its events to the platform-connectors instance on its own node over a Unix socket, which is what puts them in the datastore in the first place.

## Prerequisites

Runs on Linux and on macOS, Intel or Apple Silicon.

- **Docker** — runs the KIND cluster ([install](https://docs.docker.com/get-docker/))
- **kind** — Kubernetes in Docker ([install](https://kind.sigs.k8s.io/docs/user/quick-start/#installation))
- **kubectl** — Kubernetes CLI ([install](https://kubernetes.io/docs/tasks/tools/))
- **helm** — Kubernetes package manager ([install](https://helm.sh/docs/intro/install/))
- **jq** — JSON processor ([install](https://jqlang.github.io/jq/download/))
- **curl** — used to look up the current NVSentinel release

Resources: roughly 4 CPU cores, 8 GB RAM and 20 GB of free disk.

## Quick Start

```bash
cd demos/local-fault-injection-demo

./demo.sh            # every step, start to finish
./demo.sh cleanup    # delete the cluster
```

To read what happens at each stage, run the steps yourself instead:

```bash
./scripts/00-setup.sh              # build the cluster, install NVSentinel and the workload
./scripts/01-show-cluster.sh       # look at the cluster before anything is broken
./scripts/02-inject-fault.sh       # inject XID 95 into the GPU
./scripts/03-watch-remediation.sh  # detect, protect, remediate
./scripts/04-recover.sh            # clear the fault, watch the node return to service
./scripts/99-cleanup.sh            # delete the cluster
```

These are ordered stages, not independent scripts: step 2 needs the cluster and DCGM pod that step 0 builds, and step 3 needs the fault step 2 injects. Run them in order. Any one of them can be re-run as long as the earlier stages still hold — re-running step 3 after it has already completed, for instance, just re-checks a node that is already drained.

## What Happens at Each Step

### Step 0: Setup (`00-setup.sh`)

Creates a two node KIND cluster, installs cert-manager, then installs NVSentinel from `oci://ghcr.io/nvidia/nvsentinel` with [config/nvsentinel-values.yaml](config/nvsentinel-values.yaml).

It then deploys the fake DCGM hostengine into the `gpu-operator` namespace, labels the worker as a GPU node, and starts a single-replica Deployment on it to stand in for a training job.

Before it finishes, setup waits for the `GpuDcgmConnectivityFailure` condition on the node to read `False`. A running gpu-health-monitor pod proves nothing about whether it can reach the hostengine; that condition is only written once a poll has actually succeeded. Without the gate, a fault injected in step 2 could be silently dropped.

### Step 1: Before the fault (`01-show-cluster.sh`)

The baseline: both nodes `Ready` and schedulable, and the workload running on the GPU node.

It also checks that no GPU health check is currently failing. NVSentinel keeps one node condition per DCGM check it watches, `False` while the check passes and `True` once it fails, so a healthy node carries the full list all `False`. To see them:

```bash
kubectl get node <node> -o json | jq '.status.conditions[] | select(.type | startswith("Gpu"))'
```

### Step 2: Break the GPU (`02-inject-fault.sh`)

```bash
dcgmi test --inject --gpuid 0 -f 230 -v 95
```

Field 230 is `DCGM_FI_DEV_XID_ERRORS`. XID 95 is an uncontained ECC error which classes as fatal with a recommended action of `RESTART_VM`.

### Step 3: Detect, protect, remediate (`03-watch-remediation.sh`)

Three stages, each waited on by the effect it produces rather than by the clock:

| Stage | Who acts | Waits for | Node state label |
|---|---|---|---|
| **Detect** | gpu-health-monitor, platform-connectors | a GPU condition flipping to `True` | — |
| **Protect** | fault-quarantine, node-drainer | the node cordoned, then the workload off it | `quarantined` → `draining` → `drain-succeeded` |
| **Remediate** | fault-remediation, janitor | a `RebootNode`, then its completion | `remediating` → `remediation-succeeded` |

**Detect.** gpu-health-monitor polls DCGM every 15 seconds. When it sees the XID it sends a health event to platform-connectors over the node's Unix socket; platform-connectors stores it and records it as a node condition. 

**Protect.** fault-quarantine watches the datastore, matches its default fatal GPU rule, and cordons the node so the scheduler stops placing work on hardware known to be broken. The cordon only stops *new* pods, so node-drainer then evicts the job already running there. That pod goes `Pending`, not away: the Deployment still wants its replica and the only GPU node is cordoned, so there is nowhere to put it. 

**Remediate.** With the node empty, fault-remediation turns the event's recommended action into a repair request. `RESTART_VM` means a `RebootNode` resource, which janitor acts on through its configured provider: an EC2 call on AWS, `instances.reset` on GCP. Here it is the `kind` provider, which simulates the reboot.

### Step 4: Back in service (`04-recover.sh`)

On real hardware the reboot is what clears the XID. A KIND node is a container that is never really rebooted, and the injected fault lives in the fake DCGM hostengine, so this step restarts that pod instead — the same effect, by the only means available here. The replacement hostengine carries no injected state and reports a healthy GPU.

From there NVSentinel is unassisted again. gpu-health-monitor sends a healthy event for the check that was failing; fault-quarantine, which tracks the checks it quarantined the node for, sees the last one recover and uncordons. The scheduler places the Pending pod and the workload is running again, on the node that was broken a minute earlier.
