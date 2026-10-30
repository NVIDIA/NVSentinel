# ADR-044: GPU Health Monitor - DCGM Source Modes

## Context

NVSentinel currently treats the GPU Operator DCGM pod and service as the normal DCGM source. The labeler derives `nvsentinel.dgxc.nvidia.com/dcgm.version` from DCGM pod images, and the GPU health monitor DaemonSets use that label to select the matching DCGM-major image.

Two additional deployment topologies need support:

- an externally owned host `nv-hostengine` already exists on the GPU node and is configured through `global.dcgm.externalHostengine.endpoint/port`;
- no DCGM pod, service, or hostengine exists, and GPU health monitor must run an in-process embedded DCGM hostengine before health checks can run. The same engine must be reachable by pod-local DCGM tools such as `dcgmi` for diagnostics and fault injection.

The existing GPU health monitor remote-handle behavior must remain intact for Kubernetes Service endpoints and configured local endpoints.

DCGM 4.7.1 changes the message versions of `dcgmGetEntityGroupEntities` and `dcgmRunDiagnostic` (`dcgmi diag`), and the hostengine accepts only the new versions. A client and a hostengine on different sides of 4.7.1 fail these calls with `DCGM_ST_VER_MISMATCH` (`-12`), in both directions. Diag has no message version that works with both sides. A DCGM major version is therefore not enough to select a compatible client: every NVSentinel DCGM client (GPU health monitor, preflight `dcgm-diag`, lifecycle-manager `dcgm-diag-test`) must be on the same side of 4.7.1 as the hostengine on its node. Nodes in one cluster can run different DCGM versions.

## Decision

Add one DCGM source-mode contract with three modes:

| Mode                  | DCGM owner                          | GPU health monitor image version source               | GPU health monitor behavior                                      |
| --------------------- | ----------------------------------- | ----------------------------------------------------- | ---------------------------------------------------------------- |
| `operator-service`    | GPU Operator DCGM pod/service       | Node `dcgm.version` label derived from DCGM pod image | Connects to configured GPU Operator service                      |
| `external-hostengine` | External node-local `nv-hostengine` | Existing node `dcgm.version` label                    | Connects to configured hostengine endpoint                       |
| `embedded-mode`       | GPU health monitor                  | Existing node `dcgm.version` label                    | Runs an in-process hostengine with a pod-local loopback listener |

`operator-service` remains the default. In `operator-service`, labeler remains the writer of `nvsentinel.dgxc.nvidia.com/dcgm.version` from DCGM pod detection. In `external-hostengine`, the same node label is the scheduling contract, and labeler assumes DCGM is available when a valid label exists but no DCGM pod source exists.

The `dcgm.version` label selects a DCGM version lane, not only a DCGM major version. Each lane has its own client image:

| `dcgm.version` | Hostengine versions | Client DCGM version |
| -------------- | ------------------- | ------------------- |
| `3.x`          | 3.x                 | 3.x                 |
| `4.x`          | 4.0.0 – 4.7.0       | 4.7.0               |
| `4.7.1-plus`   | 4.7.1 and newer 4.x | 4.7.1               |

Kubernetes label values cannot contain `+`, so the new lane is named `4.7.1-plus`.

## Configuration

The primary user-facing knob is `global.dcgm.mode`. In `external-hostengine` and `embedded-mode`, operators provide the DCGM version lane through the existing `nvsentinel.dgxc.nvidia.com/dcgm.version` node label (`3.x`, `4.x` or `4.7.1-plus`), which is the same label GPU health monitor already uses for image selection.

```yaml
# Existing GPU Operator DCGM service. This is the default.
global:
  dcgm:
    mode: operator-service
    service:
      endpoint: nvidia-dcgm.gpu-operator.svc
      port: 5555
```

```yaml
# Externally owned nv-hostengine already exists on each GPU node.
global:
  dcgm:
    mode: external-hostengine
    externalHostengine:
      endpoint: localhost
      port: 5555
```

```yaml
# GPU health monitor runs its own in-process embedded DCGM hostengine and
# exposes that engine on pod-local loopback.
global:
  dcgm:
    mode: embedded-mode
    # Optional; defaults to localhost:5555 and must remain loopback.
    embedded:
      endpoint: localhost
      port: 5555
gpu-health-monitor:
  runtimeClassName: nvidia
```

Each source mode owns its connection settings: `global.dcgm.service.endpoint/port` for `operator-service`, `global.dcgm.externalHostengine.endpoint/port` for `external-hostengine`, and `global.dcgm.embedded.endpoint/port` for `embedded-mode`. The external and embedded settings default to `localhost:5555`; the external endpoint can be overridden, while the embedded endpoint must be `localhost`, `127.0.0.1`, or `::1` because it binds the in-process listener. The embedded monitor itself continues to use the embedded handle directly.

Embedded-mode runs the DCGM hostengine inside GPU health monitor container, so the pod needs direct GPU and driver access. `gpu-health-monitor.runtimeClassName` is required and must name the cluster's NVIDIA RuntimeClass (commonly `nvidia`) so the NVIDIA Container Toolkit mounts the GPUs and driver.

## Implementation

### Helm Rendering

- `operator-service` renders the existing service-backed monitor DaemonSets selected by `dcgm.version`.
- `external-hostengine` renders the regular remote monitor path with host networking and is selected by the existing node `dcgm.version` label.
- `embedded-mode` renders the regular monitor DaemonSets and is selected by the existing node `dcgm.version` label, just like `external-hostengine`, and sets `privileged: true` on the monitor container.

In `external-hostengine` and `embedded-mode`, the chart sets `--assume-dcgm-available` on labeler. This tells labeler not to remove an existing valid `dcgm.version` label when no DCGM pod is present, which is the expected topology for an externally managed hostengine or an embedded hostengine selected by node label.

### External Hostengine

`external-hostengine` expects an externally managed `nv-hostengine` to be reachable from the GPU health monitor pod at `global.dcgm.externalHostengine.endpoint:port`. Because the default endpoint is node-local `localhost:5555`, the GPU health monitor DaemonSets run with host networking in this mode.

The DCGM version lane is supplied through the existing node label:

```text
nvsentinel.dgxc.nvidia.com/dcgm.version: 3.x|4.x|4.7.1-plus
```

No separate discovery workload or Lease is required. If the label is absent, no DCGM-major GPU health monitor DaemonSet schedules on that node. If the label is present and valid, the matching GPU health monitor image runs and connects to the configured external hostengine endpoint.

### Labeler Resolution

Labeler resolves the expected DCGM version from DCGM pods when they exist, matching the existing `operator-service` behavior. If no DCGM pod source exists:

- `operator-service` removes a stale `dcgm.version` label, because the operator DCGM pod is the source of truth;
- `external-hostengine` and `embedded-mode` preserve an existing valid `dcgm.version` label, because that label is the operator-provided version contract for selecting the matching GPU health monitor image.

Labeler parses `<major>.<minor>.<patch>` from the DCGM pod image tag and maps it to a lane: 3.x to `3.x`, 4.0.0 – 4.7.0 to `4.x`, and 4.7.1 or newer 4.x to `4.7.1-plus`. A DCGM 4 image tag without a parseable minor and patch version maps to `4.x`, and labeler logs a warning.

### DCGM Version Lanes

Each NVSentinel DCGM client selects its lane on the node where it runs:

- **GPU health monitor** runs one DaemonSet and one image per lane (`-dcgm-3.x`, `-dcgm-4.x`, `-dcgm-4.7.1-plus`). Each DaemonSet schedules only on nodes with its `dcgm.version` value.
- **Lifecycle-manager** `dcgm-diag-test` runs as a Job on a known node. The test configuration maps `dcgm.version` values to images through `imageByNodeLabel`, and falls back to `image` when the label has no entry.
- **Preflight** `dcgm-diag` cannot use the label. The webhook injects the init container at admission, before the pod has a node, and the init container runs as the workload's ServiceAccount, which has no access to Node objects. The image therefore contains both 4.x client lanes. The check connects with the 4.7.1 lane, reads the hostengine version with `dcgmHostengineVersionInfo`, and re-executes itself with the 4.7.0 lane when the hostengine is older than 4.7.1. One process can load only one `libdcgm`, so lane selection uses a re-exec.

When DCGM restarts with a version in another lane, the GPU health monitor pod of the old lane gets `DCGM_ST_VER_MISMATCH` until labeler updates the label and the new lane pod starts. GPU health monitor logs the version mismatch and exits with code `1`, so the pod restarts until it is replaced. It does not report `GpuDcgmConnectivityFailure` for the mismatch. The DCGM restart itself can still report `GpuDcgmConnectivityFailure`, as any DCGM restart does; the new lane pod clears it when it connects.

### Embedded Mode

`embedded-mode` runs an in-process embedded DCGM hostengine inside the GPU health monitor process and exposes the same engine on pod-local loopback. It uses the DCGM APIs rather than a separate `nv-hostengine` process.

Runtime behavior:

1. Create the DCGM handle with no IP address (`pydcgm.DcgmHandle(opMode=DCGM_OPERATION_MODE_AUTO)`), which calls `dcgmStartEmbedded` to start the embedded hostengine in-process.
2. Call `dcgmEngineRun` through the Python bindings to expose that engine on `127.0.0.1:<port>`. GPU health monitor continues using the embedded handle; pod-local tools such as `dcgmi` connect to the loopback listener and therefore operate on the same hostengine cache.
3. Run the normal health-check loop against the embedded handle.
4. On shutdown or reconnect, `DcgmHandle.Shutdown()` stops the embedded hostengine (`dcgmStopEmbedded`) and its listener as part of the normal cleanup path.

This starts DCGM the same way `dcgm-exporter` does in embedded deployments (`dcgm.Init(dcgm.Embedded)`), then follows DCGM's own embedded test utility pattern by calling `dcgmEngineRun` for local client access. The CLI selects it via `--dcgm-mode local-managed`, derived in the chart from `global.dcgm.mode=embedded-mode`.

Because the embedded engine runs in the monitor's own pod, that pod must have GPU and driver access: set `runtimeClassName` to the NVIDIA RuntimeClass so the toolkit injects the GPUs and driver. The chart additionally sets `privileged: true` on the monitor container in this mode; on toolkits that ignore `NVIDIA_VISIBLE_DEVICES` for unprivileged containers, privileged is what makes the injection happen at all.

## Rationale

- Reusing the existing `dcgm.version` node label keeps GPU health monitor image selection consistent across all three source modes.
- Assuming DCGM availability from the label in `external-hostengine` and `embedded-mode` avoids requiring a DCGM pod or a separate discovery workload just to keep the selected monitor image running.
- `embedded-mode` runs the hostengine in-process via the DCGM API, so there is no child-process lifecycle to manage. The loopback listener is owned by the same hostengine and has the same lifetime as the embedded handle.
- The mode contract avoids secondary booleans such as "start if missing" or "discovery enabled"; behavior follows directly from the selected mode.
- Version lanes keep one node label as the version contract for every scheduled DCGM client. Diag has no cross-version message, so matching the client to the hostengine on each node is the only way to keep diag working in clusters with mixed DCGM versions.

## Consequences

### Positive

- Existing GPU Operator deployments remain unchanged by default.
- Host-installed `nv-hostengine` can schedule GPU health monitor without manual node labels.
- GPU health monitor can run in clusters with no DCGM pod, service, or hostengine.
- `embedded-mode` has no dependency on a separately deployed hostengine or external network endpoint; the engine and its pod-local listener run in-process and are torn down with the monitor.
- Version lanes support every DCGM 4.x hostengine version, per node, in clusters with mixed DCGM versions.

### Negative

- Each release builds and publishes one more GPU health monitor image, and the chart renders one more DaemonSet.
- The preflight `dcgm-diag` image contains a second `libdcgm` and its Python bindings (about 7 MB).
- The lifecycle-manager `TestConfig` API gets the optional `imageByNodeLabel` field.
- A future DCGM release that changes the client/hostengine protocol again needs a new lane.

### Mitigations

- Gate non-default behavior behind explicit `global.dcgm.mode`.
- The embedded engine and listener lifetime are tied to the DCGM handle, so they are torn down with the monitor without an orphaned process.
- GPU health monitor exits on `DCGM_ST_VER_MISMATCH` and does not report it as a connectivity failure. The gap while labeler moves a node to a new lane shows as pod restarts, and adds no fatal event beyond the one any DCGM restart can cause.
- The NVSentinel release with the `4.7.1-plus` lane must be deployed before the GPU Operator moves any node to DCGM 4.7.1.

## Alternatives Considered

### Runtime hostengine version discovery

**Approach.** Add a per-node discovery DaemonSet that probes `dcgmi -v --host <endpoint>` and writes a Lease containing the discovered DCGM major version.

**Rejected.** This adds another workload, RBAC, freshness semantics, and labeler informer path for a version signal that can already be represented by the existing `dcgm.version` node label. The simpler contract is to let operators set the label for externally managed hostengines and have labeler assume DCGM is available from that label when no DCGM pod source is present.

Preflight's version check is not this alternative. It runs inside the existing init container, uses the hostengine connection that the check already opens, and writes no label or Lease.

### One DCGM client version for all hostengine versions

**Approach.** Build every NVSentinel DCGM client from the newest DCGM release.

**Rejected.** A DCGM 4.7.1 client fails `dcgmGetEntityGroupEntities` and diag against every hostengine of 4.7.0 or older, and a 4.7.0 client fails both calls against a 4.7.1 hostengine. No single client version works with all 4.x hostengines.

### Preflight injects one init container per lane

**Approach.** The webhook injects one `dcgm-diag` init container per lane, and each container exits when the hostengine is in another lane.

**Rejected.** Every workload pod pulls and runs extra images. Runtime selection inside one image costs one RPC.

### Deploy a separate NVSentinel-managed hostengine DaemonSet

**Approach.** Implement `embedded-mode` by shipping a dedicated NVSentinel-owned `nv-hostengine` DaemonSet that GPU health monitor connects to, rather than GPU health monitor running an in-process embedded hostengine itself.

**Rejected (for the first implementation).** It adds another always-on component and an independent lifecycle to manage and monitor. Running the hostengine in-process keeps the no-DCGM case self-contained and ties the engine lifecycle to the monitor that actually needs it. There is no separately owned process, workload, or external network dependency; the pod-local loopback listener and its configured port share the embedded handle's lifecycle. Left open as a future option if a shared NVSentinel-managed hostengine is ever needed by multiple consumers on the node.

## References

- [GitHub issue #756](https://github.com/NVIDIA/NVSentinel/issues/756) - GPU health monitor support when no DCGM pod exists.
- [GitHub issue #1031](https://github.com/NVIDIA/NVSentinel/issues/1031) - labeler support for host-installed DCGM.
- [GitHub issue #2160](https://github.com/NVIDIA/NVSentinel/issues/2160) - upgrade DCGM from 4.7.0 to 4.7.1.
- [GitHub discussion #755](https://github.com/NVIDIA/NVSentinel/discussions/755) - hostengine sharing between GPU health monitor and dcgm-exporter.
- [ADR-018: Syslog Monitor Preinstalled Driver Support](./018-syslog-monitor-preinstalled-driver-support.md) - preinstalled-driver deployment context.
