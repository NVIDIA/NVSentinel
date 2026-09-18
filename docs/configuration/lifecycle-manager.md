# Lifecycle Manager Configuration

## Overview

The Lifecycle Manager hosts the controllers that manage a node's transitions in and out of
service. It runs two independent controllers, each with its own feature gate:

- **ValidationRequest controller** — runs validation tests against a node and keeps it gated
  until they pass. On by default. See [ADR-045](../designs/049-node-validation.md); its Helm
  values are not yet covered here.
- **MaintenanceRequest controller** — lets an external system declare that maintenance is
  coming for a node, so NVSentinel cordons and drains it before the work starts, and releases
  it afterwards. Opt-in, and documented below. See
  [ADR-051](../designs/051-maintenance-request.md).

The two are unrelated at runtime. Enabling one does not require the other.

## Module Enable/Disable

```yaml
global:
  lifecycleManager:
    enabled: true
```

---

## MaintenanceRequest Controller

A `MaintenanceRequest` is a cluster-scoped resource carrying a health event. Creating one
publishes that event into the NVSentinel pipeline so the node is prepared; deleting one
publishes the matching clearing event so the node is released. **Presence means active,
absence means cleared** — deleting the resource is the instruction, not merely cleanup.

### Enabling

```yaml
lifecycle-manager:
  controllers:
    maintenanceRequest:
      enabled: true
```

The controller is disabled by default.

Turning this on changes more than the reconciler. It also:

- registers the mutating and validating webhooks for `MaintenanceRequest`
- grants RBAC for the resource, its status and its finalizers
- mounts the platform-connector socket into the pod
- projects a ServiceAccount token into the pod, when authentication is enabled
- adds this ServiceAccount to platform-connector's cross-node allowlist

While the gate is off, the validating webhook rejects any `MaintenanceRequest` at admission
rather than letting it be created and silently ignored.

> **Operational note:** the allowlist lives in platform-connector's ConfigMap, and that
> ConfigMap's checksum is stamped on the DaemonSet pod template. Enabling this controller
> therefore rolls platform-connector across every node. This is true of enabling any
> cross-node publisher, not specific to this one, but it is worth scheduling deliberately.

### Authentication

This is the part most easily missed. A `MaintenanceRequest` may name **any** node, but
lifecycle-manager is a Deployment running on one node. platform-connector pins a publisher to
its own node unless that publisher is on the cross-node allowlist and presents a valid token,
so both halves are required:

- the pod is given a projected ServiceAccount token, minted for
  `global.platformConnectorAuth.audience`, and passed to the process with
  `--platform-connector-token-path`
- the umbrella chart adds `system:serviceaccount:<namespace>:lifecycle-manager` to
  `AuthCrossNodeServiceAccounts`

Both are derived from the feature gate above, so there is nothing extra to configure. Do **not**
add lifecycle-manager to `global.platformConnectorAuth.crossNodeServiceAccounts` by hand; that
list is for publishers this chart does not ship, and a hand-written entry with the wrong
namespace renders cleanly and then has every event rejected at runtime.

With `global.platformConnectorAuth.enabled: false`, no token is projected and no allowlist
entry is added, which matches the unauthenticated behaviour of every other publisher.

See [Authentication](./authentication.md) for the full node-binding model, and the
`platform_connector_auth_violations_total` metric to watch if events are being rejected.

### Quarantine Rule

Publishing the event is only half of the outcome. Whether the node is actually cordoned
depends on fault-quarantine having a ruleset that matches the event. The bundled
`MaintenanceRequest ruleset` in the fault-quarantine chart matches on the
`maintenanceRequestName` metadata key, which the mutating webhook stamps on every event this
controller publishes. Removing or narrowing that ruleset leaves MaintenanceRequests accepted
but inert: the resource is created, its condition goes green, and the node is never cordoned.

### Example

```yaml
apiVersion: nvsentinel.dgxc.nvidia.com/v1
kind: MaintenanceRequest
metadata:
  name: csp-maintenance-ip-10-0-31-7
spec:
  healthEvent:
    version: 1
    agent: csp-maintenance-integration
    componentClass: Node
    checkName: ScheduledHostMaintenance
    nodeName: ip-10-0-31-7
    isHealthy: false
    isFatal: false
    recommendedAction: RESTART_VM
    message: "Host maintenance scheduled by the cloud provider"
  startTime: "2026-10-01T02:00:00Z"
```

### Validation Rules

The validating webhook rejects a request that:

- omits `spec.healthEvent` or `spec.healthEvent.nodeName`
- sets `isHealthy: true`, since an MR describes work starting, not a recovery
- leaves `version` at zero
- names a node that does not exist
- sets `startTime` in the past
- changes any requester-authored field of `healthEvent` after creation

The mutating webhook fills in `id` and `generatedTimestamp` if absent, and always sets the
`maintenanceRequestName` metadata key.

### Status and Deletion

The controller sets a `HealthEventEmitted` condition once the opening event has been accepted
by platform-connector, and takes a finalizer on the resource. On deletion it publishes the
clearing event, releases the node lock, and only then drops the finalizer — so a failed
clearing publish leaves the object in `Terminating` and retries, rather than losing the clear
and leaving the node cordoned indefinitely.

If the clearing event can never be delivered, the resource stays in `Terminating`. Removing
the finalizer by hand is the escape hatch, at the cost of the node staying cordoned until an
operator clears it.
