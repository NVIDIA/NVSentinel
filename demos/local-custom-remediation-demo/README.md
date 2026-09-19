# NVSentinel Local Demo: Custom Remediation Actions

**Route a repair to a controller NVSentinel has never heard of, end to end, on a KIND cluster with no GPU hardware.**

This demo exhausts a node's memory and then gets out of the way. A health monitor nobody shipped reports the fault, NVSentinel cordons the node and asks for a repair it has no code to perform, and a third-party controller carries it out — after which the node comes back on its own.

> **No GPU required.** The fault is real memory pressure on a real node, and nothing about the pipeline is stubbed. What is custom is both ends of it: the monitor that detects the fault and the controller that repairs it. Everything between them is the code that runs in production clusters, unmodified.

## What You'll Learn

1. **Custom health monitors** — a standalone process that reports a fault NVSentinel has no built-in check for
2. **Custom remediation actions** — `recommendedAction: CUSTOM` plus a name routes the event to an arbitrary CRD
3. **Third-party controllers** — what a controller has to write back for NVSentinel to consider the repair finished
4. **Return to service** — the fault clears, fault-quarantine uncordons, and the node is usable again

The point of the exercise is step 2. NVSentinel ships actions for the faults it knows about; the extension point exists so a fault it has never heard of can still be routed somewhere useful, without a fork.

## The Pipeline

```text
┌───────────────────────────────────────────────────────────────────────────┐
│  KIND cluster                                                             │
│                                                                           │
│  Worker node                                                              │
│  ┌───────────────────────────────────────────────────────┐                │
│  │  /proc/meminfo  ◄── you fill it here                  │                │
│  │          │                                            │                │
│  │          ▼                                            │                │
│  │  memory-pressure-monitor        (custom, this demo)   │                │
│  │          │  health event, over a Unix socket          │                │
│  │          │  recommendedAction: CUSTOM/RECLAIM_MEMORY  │                │
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
│      cordon the node           no-op: no user          render the         │
│                                namespaces              RECLAIM_MEMORY     │
│                                                        template           │
│                                                                │          │
│                                                                ▼          │
│                                                     ┌────────────────┐    │
│                                                     │ MemoryReclaim  │    │
│                                                     └────────┬───────┘    │
│                                                              │ watch      │
│                                                              ▼            │
│                                              memory-reclaim-controller    │
│                                                    (custom, this demo)    │
│                                              delete the pods, then write  │
│                                              reclaimedPods + the condition│
└───────────────────────────────────────────────────────────────────────────┘
```

The core modules coordinate only through the datastore — none of them calls another. The custom action adds one more seam of the same kind: fault-remediation and the demo controller never talk, they just take turns writing to a `MemoryReclaim`.

## Prerequisites

Runs on Linux and on macOS, Intel or Apple Silicon.

- **Docker** — runs the KIND cluster and builds the two demo images ([install](https://docs.docker.com/get-docker/))
- **kind** — Kubernetes in Docker ([install](https://kind.sigs.k8s.io/docs/user/quick-start/#installation))
- **kubectl** — Kubernetes CLI ([install](https://kubernetes.io/docs/tasks/tools/))
- **helm** — Kubernetes package manager ([install](https://helm.sh/docs/intro/install/))
- **go** — Go 1.25+ ([install](https://go.dev/dl/))
- **jq** — JSON processor ([install](https://jqlang.github.io/jq/download/))
- **curl** — used to look up the current NVSentinel release

Resources: roughly 4 CPU cores, 8 GB RAM and 20 GB of free disk.

## Quick Start

```bash
cd demos/local-custom-remediation-demo

./demo.sh            # every step, start to finish
./demo.sh cleanup    # delete the cluster
```

To read what happens at each stage, run the steps yourself instead:

```bash
./scripts/00-setup.sh              # build the cluster, install NVSentinel, monitor and controller
./scripts/01-show-cluster.sh       # look at the cluster before anything is broken
./scripts/02-trigger-pressure.sh   # fill the node's memory
./scripts/03-watch-remediation.sh  # protect, remediate, back in service
./scripts/99-cleanup.sh            # delete the cluster
```

These are ordered stages, not independent scripts: step 2 needs the cluster step 0 builds, and step 3 needs the fault step 2 creates. Run them in order.

## What Happens at Each Step

### Step 0: Setup (`00-setup.sh`)

Creates a two node KIND cluster, installs cert-manager, then installs NVSentinel from `oci://ghcr.io/nvidia/nvsentinel` with [config/nvsentinel-values.yaml](config/nvsentinel-values.yaml).

It resolves the version rather than pinning one: the highest semver tag published to the chart's OCI repository wins, so a clone of this repo installs the current release however old the clone is. Set `NVSENTINEL_CHART_VERSION=v1.23.0` to pin. The chart pins its own matching image tag, so nothing here overrides it — setting one and not the other is how a chart ends up rendering flags the image does not have.

It then installs the `MemoryReclaim` CRD, grants fault-remediation access to it, builds both demo images, and deploys the monitor as a DaemonSet and the controller as a Deployment.

Before it reports success, every pod must still be `Ready` after a settle window with no container restarts during it. A pod that reports `Ready` once can still be crash-looping, and the point of setup is to catch that here rather than two steps later.

### Step 1: Before the fault (`01-show-cluster.sh`)

The baseline: both nodes `Ready` and schedulable, the monitor and controller up, current `MemAvailable` against the monitor's threshold, and no `MemoryReclaim` in existence yet.

### Step 2: Fill the memory (`02-trigger-pressure.sh`)

Deploys a `stress` pod that reserves 300 MB and holds it, which pushes `MemAvailable` below the monitor's threshold.

**It recalibrates the threshold first, and this matters.** KIND nodes share the host's kernel, so `/proc/meminfo` inside the worker reports *your machine's* free memory, not a fixed slice of it. A threshold computed during setup is routinely stale minutes later — the host frees or consumes hundreds of MB on its own, and a 300 MB allocation then fails to cross it. Recomputing `MemAvailable - MEM_MARGIN_MB` immediately before the hog starts makes the gap exactly the margin.

The step then waits for the node condition `MemoryAvailableCheck` to turn `True`. platform-connectors writes one condition per check, named after the monitor's `checkName`, and `True` means a fault is present (NVSentinel inverts the usual sense). Gating here means a stale calibration is reported now, rather than looking like a broken pipeline in the next step.

On a busy machine, retry once with more headroom:

```bash
HOG_MEM_MB=1024 MEM_MARGIN_MB=700 ./scripts/02-trigger-pressure.sh
```

If that fails too, reset rather than retrying further — a half-processed event leaves the node cordoned and the pipeline mid-flight:

```bash
./demo.sh cleanup && ./scripts/00-setup.sh
```

### Step 3: Protect, remediate, back in service (`03-watch-remediation.sh`)

| Stage | Who acts | Waited on by |
|---|---|---|
| Protect | fault-quarantine, then node-drainer | the node cordoned |
| Remediate | fault-remediation, then memory-reclaim-controller | a `MemoryReclaim` existing, then `status.reclaimedPods` being written, then the pod gone |
| Back in service | the monitor, then fault-quarantine | `MemoryAvailableCheck` turning `False`, then the node uncordoned |

Every check reads an object — a node condition, a CR status, a `deletionTimestamp` — never a controller's log. Logs are for debugging; a demo that asserts on them asserts on wording.

**Completion is not remediation.** The controller closes the request whether or not it found anything to delete, so `MemoryReclaimed=True` on its own proves nothing. It records what it actually did:

| `reclaimedPods` | `reason` | Means |
|---|---|---|
| ≥ 1 | `HogPodsDeleted` | this controller deleted the pods |
| 0 | `NoHogPodsFound` | it ran, but something else had already removed them |

The script fails on a zero count. This is also why `node-drainer.userNamespaces` is empty in the values file: the drain runs *before* remediation, so listing `default` there would let node-drainer evict the hog first and leave the custom controller with nothing to do — while the pod still looked "cleaned up".
