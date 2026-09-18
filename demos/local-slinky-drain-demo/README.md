# NVSentinel Local Demo: Custom Drain with Slinky

**Hand the drain to an external scheduler, end to end, on a KIND cluster with no GPU hardware.**

This demo breaks a GPU and then gets out of the way. NVSentinel detects the fault and cordons the node — and then, instead of evicting the pods itself, node-drainer writes a `DrainRequest` and waits. An external controller picks it up, negotiates with the cluster scheduler, and only deletes a pod once the scheduler says that pod's work is finished.

> **No GPU required.** The GPU is simulated by a fake DCGM hostengine backed by NVML injection. Everything above it — the health monitor, the event pipeline, the quarantine rules, the custom drain handshake — is the code that runs in production clusters, unmodified.

## What You'll Learn

1. **Detect** — gpu-health-monitor polls DCGM, and a fault becomes a health event and a node condition
2. **Delegate** — fault-quarantine cordons the node; node-drainer writes a `DrainRequest` CR instead of draining, and blocks on its status
3. **Coordinate** — slinky-drainer annotates the node, the scheduler marks each pod drainable, and only then are the pods deleted
4. **Return to service** — the fault clears, fault-quarantine uncordons, and the workload is rescheduled

The point of the exercise is step 2. Draining an HPC node is not a Kubernetes eviction: the scheduler owns the jobs, and killing a pod out from under it loses work. Custom drain lets NVSentinel say *this node needs to be emptied* and leave *how* to whoever owns the workload.

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
│            ┌─────────────────────────┴─────────────────────────┐          │
│            ▼                                                   ▼          │
│    fault-quarantine                                     node-drainer      │
│      cordon the node                                          │           │
│                                            custom drain: write a          │
│                                            DrainRequest, then wait        │
│                                                               │           │
│                                                               ▼           │
│                                                      ┌────────────────┐   │
│                                                      │  DrainRequest  │   │
│                                                      └────────┬───────┘   │
│                                                               │ watch     │
│                                                               ▼           │
│                                                        slinky-drainer     │
│                                    ┌──────────────────────────┤           │
│                                    │ 1. annotate the node     │           │
│                                    ▼                          │           │
│                          mock-slurm-operator                  │           │
│                            2. mark each pod                   │           │
│                            SlurmNodeStateDrain=True           │           │
│                                    │                          ▼           │
│                                    └────────────►  3. delete the pods     │
│                                                    4. clear the annotation│
│                                                    5. DrainComplete=True  │
└───────────────────────────────────────────────────────────────────────────┘
```

The core modules coordinate only through the datastore — none of them calls another — which is why each can be enabled, disabled or replaced on its own. The one direct hop is at the edge: a health monitor hands its events to the platform-connectors instance on its own node over a Unix socket, which is what puts them in the datastore in the first place.

The custom drain adds one more seam of the same kind: node-drainer and slinky-drainer never talk either, they just take turns writing to a `DrainRequest`.

## Prerequisites

Runs on Linux and on macOS, Intel or Apple Silicon.

- **Docker** — runs the KIND cluster ([install](https://docs.docker.com/get-docker/))
- **kind** — Kubernetes in Docker ([install](https://kind.sigs.k8s.io/docs/user/quick-start/#installation))
- **kubectl** — Kubernetes CLI ([install](https://kubernetes.io/docs/tasks/tools/))
- **helm** — Kubernetes package manager ([install](https://helm.sh/docs/intro/install/))
- **ko** — builds the mock Slurm operator ([install](https://ko.build/install/))
- **go** — Go 1.25+, used by `ko` ([install](https://go.dev/dl/))
- **jq** — JSON processor ([install](https://jqlang.github.io/jq/download/))
- **curl** — used to look up the current NVSentinel release

Resources: roughly 4 CPU cores, 8 GB RAM and 20 GB of free disk.

## Quick Start

```bash
cd demos/local-slinky-drain-demo

./demo.sh            # every step, start to finish
./demo.sh cleanup    # delete the cluster
```

To read what happens at each stage, run the steps yourself instead:

```bash
./scripts/00-setup.sh          # build the cluster, install NVSentinel and both plugins
./scripts/01-show-cluster.sh   # look at the cluster before anything is broken
./scripts/02-inject-fault.sh   # inject XID 95 into the GPU
./scripts/03-watch-drain.sh    # detect, delegate, coordinate, drain
./scripts/04-recover.sh        # clear the fault, watch the node return to service
./scripts/99-cleanup.sh        # delete the cluster
```

These are ordered stages, not independent scripts: step 2 needs the cluster and DCGM pod that step 0 builds, and step 3 needs the fault step 2 injects. Run them in order.

## What Happens at Each Step

### Step 0: Setup (`00-setup.sh`)

Creates a two node KIND cluster, installs cert-manager, installs the `DrainRequest` CRD, then installs NVSentinel from `oci://ghcr.io/nvidia/nvsentinel` with [config/nvsentinel-values.yaml](config/nvsentinel-values.yaml).

It resolves the version rather than pinning one: the highest semver tag published to the chart's OCI repository wins, so a clone of this repo installs the current release however old the clone is. Set `NVSENTINEL_CHART_VERSION=v1.23.0` to pin. The chart pins its own matching image tag, so nothing here overrides it — setting one and not the other is how a chart ends up rendering flags the image does not have.

It then deploys the fake DCGM hostengine, labels the worker as a GPU node, deploys both drain plugins, and starts a two-replica Deployment in the `slinky` namespace to stand in for Slurm-managed jobs.

Only one of the plugins is built from this checkout. slinky-drainer is a released NVSentinel component, so it is pulled as `ghcr.io/nvidia/nvsentinel/slinky-drainer:<chart version>` — the same version as everything else the demo installs. mock-slurm-operator exists only to stand in for a Slurm control plane and is published nowhere, so `ko` builds it locally. Point `SLINKY_DRAINER_IMAGE` at your own build to test a change to the drainer.

Two gates before it reports success:

- the `GpuDcgmConnectivityFailure` condition on the node must read `False`. A running gpu-health-monitor pod proves nothing about whether it can reach the hostengine; that condition is only written once a poll has actually succeeded. Without the gate, a fault injected in step 2 could be silently dropped.
- every pod must still be `Ready` after a settle window, with no container restarts during it. A pod that reports `Ready` once can still be crash-looping, and the point of setup is to catch that here rather than three steps later.

### Step 1: Before the fault (`01-show-cluster.sh`)

The baseline: both nodes `Ready` and schedulable, the workload running on the GPU node, both drain controllers up, and no `DrainRequest` in existence yet.

### Step 2: Break the GPU (`02-inject-fault.sh`)

```bash
dcgmi test --inject --gpuid 0 -f 230 -v 95
```

Field 230 is `DCGM_FI_DEV_XID_ERRORS`. XID 95 is an uncontained ECC error, which DCGM reports as `DCGM_FR_UNCONTAINED_ERROR` and NVSentinel classes as fatal.

### Step 3: Detect, delegate, coordinate, drain (`03-watch-drain.sh`)

| Stage | Who acts | Waited on by |
|---|---|---|
| Detect | gpu-health-monitor → platform-connectors | a `Gpu*` node condition turning `True` |
| Delegate | fault-quarantine, then node-drainer | the node cordoned, then a `DrainRequest` existing |
| Coordinate | slinky-drainer, then mock-slurm-operator | the node annotation, then `SlurmNodeStateDrain=True` on the pods |
| Drain | slinky-drainer | no workload pods left, and `DrainComplete` reaching a terminal reason |

Every check reads an object — a node condition, an annotation, a pod condition, a CR status — never a controller's log. Logs are for debugging; a demo that asserts on them asserts on wording.

The coordinate stage is the only one allowed to miss its evidence. The annotation is removed and the pods are deleted as soon as the handshake completes, so a slow poll can legitimately arrive after the fact. The durable record is the `DrainComplete` reason, checked in the next stage.

**Completion is not remediation.** slinky-drainer closes the `DrainRequest` whether it drained pods or found none, recording:

| `reason` | Means |
|---|---|
| `DrainComplete` | pods were marked drainable by the scheduler, then deleted |
| `NoPods` | there was nothing on the node; the request completed without remediating anything |

The script fails on `NoPods`. An empty `slinky` namespace is not evidence that a drain happened — the pods may never have been there.

### Step 4: Back in service (`04-recover.sh`)

A drained node is still a node nobody can schedule on, so the demo is not finished until it comes back.

Restarting the DCGM pod drops the injected fault, which is what a real repair would have done. The monitor then reports the check healthy, fault-quarantine uncordons the node on its own, and the Deployment's pods are rescheduled onto it.
