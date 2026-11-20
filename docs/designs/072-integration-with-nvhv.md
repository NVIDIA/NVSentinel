# ADR-072: Health Monitors — Integration with NVHV

## Context

NVHV ([Health Validation](https://github.com/dsx-ai-factory/health-validation)) runs node health checks, such as DCGM's health checks and Xid detection, and reports the result as one verdict for the node. Serving that verdict over gRPC (`health.verdict.v1.VerdictService/Evaluate`, through `health-validation serve`) isn't in NVHV yet: it is in flight in [NVHV PR 268](https://github.com/dsx-ai-factory/health-validation/pull/268), and this design depends on it. Each result in the verdict has a severity and an action. NVSentinel plans to use these checks in place of its own, one kind at a time, such as Xid detection, done today by syslog-health-monitor, and the DCGM health checks, done today by gpu-health-monitor (GHM).

Every kind of check needs the same things from NVSentinel: a way to run NVHV on each GPU node, call it, turn its verdict into health events, and handle NVHV being down. This ADR decides those shared parts once; how each kind of check's results become events will be designed separately.

## Decision

Add a health monitor, **nvidia-health-validation-monitor**, that runs NVHV as a sidecar in the same pod and calls NVHV's gRPC verdict API on loopback every poll interval. It compares each verdict with the faults it already reported and publishes only what changed. Every kind of NVHV check NVSentinel adopts goes through this one monitor.

## Implementation

```mermaid
flowchart TB
  subgraph POD["GPU node: one pod, two containers"]
    direction LR
    MON["<b>nvidia-health-validation-monitor</b><br/>polls NVHV every poll interval<br/>publishes only what changed"]
    NVHV["<b>NVHV sidecar</b><br/>health-validation serve, loopback only<br/>runs the checks"]
    MON -- "gRPC Evaluate" --> NVHV
    NVHV -- "NodeVerdict" --> MON
  end
  PC["<b>NVSentinel platform connector</b>"]
  SRC["<b>Sources NVHV reads</b><br/>for example DCGM, NVML,<br/>the host journal"]
  MON -- "HealthEvents" --> PC
  NVHV --> SRC
```

- **Pod.** One pod per GPU node, from a DaemonSet in NVSentinel's chart, with two containers: the monitor, and NVHV (`health-validation serve`, loopback only) from the image NVHV publishes.
  - **NVHV owns:** running the checks, each result's severity and action, and the connections to the sources it reads (for example DCGM, NVML and the host journal).
  - **The monitor owns:** the poll interval, the faults it reported, saved state, and the events.
- **Poll.** Every poll interval (configurable, default 15 s) the monitor calls `Evaluate` and gets one verdict for the node. It turns the results into events in the way designed for each kind of check, and publishes only what changed; when nothing changed, it publishes nothing.
- **Publishing.** The events of one poll go to NVSentinel's platform connector in one batch, through NVSentinel's shared publishing client: the node-local platform connector socket, or a central connector when `HEALTH_PUBLISH_TARGET` is set. The monitor updates its record of reported faults only after the batch was delivered; if publishing fails, the next poll sends the same events again.
- **NVHV unreachable.** When the call to NVHV fails (`DEADLINE_EXCEEDED`, `UNAVAILABLE`, `INTERNAL`, or no connection), the monitor logs it, sends no health event, and neither raises nor clears any fault until NVHV answers again. Kubernetes restarts NVHV through its liveness probe.
- **Chart.** NVSentinel's chart provides:
  - `global.nvidiaHealthValidationMonitor.enabled`, which turns the monitor on for the whole cluster.
  - one poll interval, configurable in the chart (default 15 s), passed to the monitor and to the NVHV settings that depend on it;
  - NVHV's request budget, derived from the poll interval, so a request never outlives the next poll;
  - the monitor's processing strategy (default `EXECUTE_REMEDIATION`);
  - host mounts: the state file and the platform connector socket, or the `HEALTH_PUBLISH_*` settings for a central connector;
- **Startup order.** On Kubernetes 1.29 and later, NVHV is a native sidecar: an init container with `restartPolicy: Always` and a startup probe on NVHV's port, so the monitor starts only after NVHV is listening, and Kubernetes restarts NVHV on its own. On Kubernetes 1.28 and earlier, NVHV is an ordinary second container, and the monitor's polls fail as NVHV unreachable until NVHV is up.

## Rationale

- **NVHV owns its build.** The sidecar runs the image NVHV publishes, including the libraries its checks load, such as DCGM's.
- **Failures stay apart.** Kubernetes restarts each container on its own, so an NVHV crash doesn't take the monitor down, and NVHV gets its own liveness probe.
- **One monitor for every check.** Polling, publishing, deployment and NVHV error handling are written once; a new kind of check adds only how its results become events and its settings.

## Consequences

### Positive

- The checks live in one place, NVHV, and any other NVHV consumer gets the same checks and verdicts.
- NVHV turns sources into a verdict; the monitor turns verdicts into events and keeps the state. Each part is smaller and can be changed and tested on its own.
- No new monitor or sidecar for each new kind of check.

### Negative

- **NVSentinel's detection depends on NVHV.** Everything the monitor reports comes from NVHV's verdict. While NVHV is unreachable (crashed, hung, restarting, or never started), no checks run on the node and no fault is raised or cleared; the monitor only logs it and emit the metric for alert.

### Mitigations

- NVHV's liveness probe restarts it when it hangs, and the native sidecar keeps the monitor from polling before NVHV is listening.

## Alternatives Considered

### Build NVHV's image outside NVHV

The sidecar design stays as it is and only the image's source changes: NVSentinel builds NVHV's image from a pinned NVHV release. NVHV's release process already expects users to build deployment images from a source version they choose.

- **Internal users:** an internal CI pipeline builds NVHV's image from a pinned NVHV release, with the native libraries its checks load (for example DCGM's `libdcgm.so.4`), and publishes it to a private registry. The chart's values for these clusters point the NVHV sidecar at that image.
- **External users:** NVSentinel provides a short note for building the NVHV image from a pinned NVHV release. The user builds it, pushes it to their own registry, and sets it in the Helm values for the NVHV sidecar (for example `<nvhv-sidecar-image>`).

**Rejected** because:

1. **NVSentinel owns the image build:** it pins the NVHV version and the versions of the libraries NVHV's checks load, and rebuilds for every NVHV release or fix it needs.
2. **The build must add those libraries.** NVHV's own image build leaves out the ones it expects to be mounted from the host (for example DCGM's `libdcgm.so.4`), so the script adds them itself.
3. **External users have an extra step before installing.** The chart should refuse to turn on the monitor when no NVHV image is set, rather than start a pod that can't pull its sidecar.
4. **Two builds must stay the same.** The internal pipeline runs the same script, so internal and external images are built the same way.

### Import NVHV as a Go module

The monitor can import NVHV's Go module and run the checks it needs in its own process, through the same library call NVHV's CLI uses, which returns the same verdict. The monitor builds a catalog with only those checks and passes only their sensors, so there is no check-selection question. There is also no transport to secure, and a breaking API change shows up at build time.

**Rejected** because:

1. **The monitor must handle a hung check itself.** A check can hang in a native library call (for example a DCGM call), which Go can't cancel, so the monitor must not start a new evaluation while one is still running, and its liveness probe must fail once the call runs past a deadline. With the sidecar, NVHV runs one evaluation at a time and is restarted through its own liveness probe; a restart here restarts the whole monitor, not just NVHV.
2. **A crash in a native library NVHV loads stops the monitor** (for example DCGM's or NVML's), because the library is loaded into the monitor's process.
3. **An NVHV fix needs a monitor release**, not just a new image tag.
4. **No release to pin yet:** NVHV's module has no release tag yet, and before 1.0 a minor release can change its Go API.
5. **The monitor must repeat what NVHV's CLI does around the library call:** set the execution context, cadence, node ID and timeouts; and report a failed GPU architecture discovery as a failed verdict, so that a node whose GPUs are all lost doesn't add up to PASS.

### Run NVHV as a child process in the monitor's container

The monitor's image can include NVHV's binary, built from NVHV's source at a pinned version, with the native libraries its checks load. The monitor starts NVHV's server as a long-lived child process and calls `Evaluate` on it over loopback, as it would call the sidecar. NVHV still runs in its own process, so it keeps its connections to its sources, its one-evaluation-at-a-time admission and the work around the library call, and the monitor's own build needs no cgo or NVHV dependencies.

**Rejected** because:

1. **The monitor supervises NVHV itself.** It restarts the child when the child exits, and kills and restarts it when a check hangs past a deadline, which is what the kubelet does for the sidecar. The monitor keeps its own state across these restarts.
2. **One image to build:** the monitor's image carries NVHV's binary and the native libraries its checks load, so an NVHV fix needs a rebuild of the monitor's image, not just a new image tag.

### Add NVHV to the existing health monitors

Instead of a new monitor, each existing monitor gets a flag that switches it between its current logic and NVHV: gpu-health-monitor for the DCGM checks, syslog-health-monitor for Xid detection, each with NVHV as a sidecar in its own pod.

**Rejected** because:

1. **NVHV doesn't fit cleanly into any one existing monitor.** Its checks span areas that NVSentinel's monitors split between them today (DCGM in gpu-health-monitor, Xid in syslog-health-monitor), and more kinds of check will be added. Putting NVHV into one of them would tie unrelated checks to that monitor: a user who wants a new kind of NVHV check would have to turn on gpu-health-monitor for it.
2. **Regressions in monitors already in production.** The existing monitors run in production clusters; adding a second detection path inside them risks breaking what works today.
3. **Each check runs in exactly one monitor.** A kind of check comes either from NVHV or from the old monitor, never both. A separate monitor keeps that split clear; flags inside the existing monitors would spread it across several of them.

### A separate monitor for each kind of NVHV check

**Rejected** because: each would run its own NVHV sidecar and repeat the poll, publishing and deployment.

## References (Optional)

- [NVHV (Health Validation) repository](https://github.com/dsx-ai-factory/health-validation)
- [NVHV PR 268: serve node verdicts over HTTP and gRPC](https://github.com/dsx-ai-factory/health-validation/pull/268) (in flight), which adds the verdict API (`proto/health/v1/verdict.proto`)
