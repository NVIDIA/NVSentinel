# Lifecycle Manager Configuration

## Overview

lifecycle-manager hosts the controllers that manage the transitions of a node into and out of service. It runs two independent controllers, and each controller has its own feature gate:

- **ValidationRequest controller** — runs validation tests against a node, and keeps the node gated until the tests pass. It is enabled by default.
- **MaintenanceRequest controller** — prepares a node for upcoming maintenance. It releases the node after the deletion of the request. It is disabled by default.

The two controllers are independent at runtime. You can enable one without the other.

## Module Enable/Disable

```yaml
global:
  lifecycleManager:
    enabled: true
```

## Logging

This value sets the verbosity level of the lifecycle-manager logs.

```yaml
lifecycle-manager:
  logLevel: info  # Options: debug, info, warn, error
```

---

## MaintenanceRequest Controller

A `MaintenanceRequest` is a cluster-scoped resource that carries a health event. When you create one, the controller publishes that health event into the NVSentinel pipeline, and NVSentinel prepares the node. When you delete one, the controller publishes the matching clearing event, and NVSentinel releases the node. **Presence means active, absence means cleared.** The deletion of the resource is the instruction, not only a cleanup.

### Enabling

```yaml
lifecycle-manager:
  controllers:
    maintenanceRequest:
      enabled: true
```

The controller is disabled by default.

This setting does more than enable the reconciler. It also:

- registers the mutating and validating webhooks for `MaintenanceRequest`
- grants RBAC for the resource, its status and its finalizers
- mounts the platform-connector socket into the pod
- projects a ServiceAccount token into the pod, if authentication is enabled
- adds this ServiceAccount to the cross-node allowlist of platform-connector

While the gate is off, the validating webhook rejects any `MaintenanceRequest` at admission. As a result, you cannot create a request that the controller then ignores silently.

> **Operational note:** the allowlist is in the ConfigMap of platform-connector. The chart writes the checksum of that ConfigMap on the DaemonSet pod template. As a result, when you enable this controller, platform-connector gets a rolling update on every node. The same is true when you enable any cross-node publisher, not only this one. Schedule this change deliberately.

By default, the controller publishes through the socket. With `lifecycle-manager.publishTo: deployment`, it publishes to the [deployment platform connector](./platform-connectors.md#deployment-platform-connector) instead. The chart then sets the `HEALTH_PUBLISH_*` environment variables on the pod. The controller presents the same projected token as on the socket path. The socket mount stays in the pod, but the controller does not use it.

### Authentication

This part is the easiest to miss. A `MaintenanceRequest` can name **any** node, but lifecycle-manager is a Deployment that runs on one node. Also, platform-connector pins a publisher to its own node, unless the publisher is on the cross-node allowlist and presents a valid token. For this reason, both of these items are necessary:

- the pod gets a projected ServiceAccount token, minted for `global.platformConnectorAuth.audience`, and the process gets the token through `--platform-connector-token-path`
- the umbrella chart adds `system:serviceaccount:<namespace>:lifecycle-manager` to `AuthCrossNodeServiceAccounts`

The chart derives both items from the feature gate above, so you do not configure anything more. Do **not** add lifecycle-manager to `global.platformConnectorAuth.crossNodeServiceAccounts` manually. That list is for publishers that this chart does not ship. A manual entry with the wrong namespace renders without errors, but then platform-connector rejects every event at runtime.

With `global.platformConnectorAuth.enabled: false`, the chart projects no token and adds no allowlist entry. This matches the unauthenticated behaviour of every other publisher.

For the full node-binding model, see [Authentication](./authentication.md). If platform-connector rejects events, watch the `platform_connector_auth_violations_total` metric.

### Quarantine Rule

The publication of the health event is only half of the result. If no fault-quarantine ruleset matches the health event, fault-quarantine does not cordon the node. The bundled `MaintenanceRequest ruleset` in the fault-quarantine chart matches on the `maintenanceRequestName` metadata key. The mutating webhook puts this key on every health event that this controller publishes. If you remove or narrow that ruleset, the API server still accepts MaintenanceRequests, but they have no effect. The resource exists and its condition reports success, but fault-quarantine never cordons the node.

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
```

The webhook changes `agent` to `lifecycle-manager` before it stores the object. It keeps `csp-maintenance-integration` in `healthEvent.metadata.maintenanceRequestRequesterAgent`.

### Validation Rules

The validating webhook rejects a request that:

- omits `spec.healthEvent` or `spec.healthEvent.nodeName`
- sets `isHealthy: true`, because an MR describes the start of work, not a recovery
- leaves `version` at zero
- names a node that does not exist
- sets `startTime` in the past
- names a node that already has an open MR
- changes any stored field of `healthEvent` after creation

If `id` and `generatedTimestamp` are absent, the mutating webhook sets them. It sets the publishing agent to `lifecycle-manager`, and it keeps the supplied agent in `maintenanceRequestRequesterAgent`. It always sets the `maintenanceRequestName` metadata key.

### Status and Deletion

When platform-connector accepts the opening event, the controller sets a `HealthEventEmitted` condition. It also adds a finalizer to the resource. On deletion, the controller publishes the clearing event and releases its leases. It removes the finalizer only after these steps. As a result, if the clearing publish fails, the object stays in `Terminating`, and the controller retries. The controller does not lose the clearing event, and the node does not stay cordoned indefinitely.

The controller uses two Lease objects in the lifecycle-manager namespace:

- **Claim** (`mr-claim.<node-name>`): the MR takes it before the opening event and holds it until deletion, so a node has at most one open MR. If a second MR for the node passes the webhook, the controller sets `HealthEventEmitted=False` with reason `Rejected`. It does not retry this MR. If the node name is too long for a Lease name, the controller replaces the end of the name with a short hash.
- **Janitor node lock** (`<node-name>`): the lock that janitor operations, such as RebootNode, share. The MR holds it only while it publishes the opening event, so the event does not start maintenance while a janitor operation runs. If another holder has the lock, the controller sets reason `Blocked` and retries every 30 seconds. The MR releases the lock immediately after it publishes, so the janitor operation that its event starts can take the lock.

If the controller can never deliver the clearing event, the resource stays in `Terminating`. As a last resort, remove the finalizer manually. But then the node stays cordoned until an operator clears it.
