# Janitor Configuration

## Overview

The Janitor module watches for Kubernetes Custom Resources (`RebootNode`, `TerminateNode`, `GPUReset`) created by fault-remediation and carries out the actual node operations by calling the Janitor Provider over gRPC. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the janitor module is deployed in the cluster.

```yaml
global:
  janitor:
    enabled: true
```

### Replica Count

```yaml
janitor:
  replicaCount: 1
```

The Janitor processes one CR at a time per controller. A single replica is sufficient for most deployments.

### Resources

Defines CPU and memory resource requests and limits for the janitor pod. No defaults are set; configure for production use.

```yaml
janitor:
  resources: {}
```

### Logging

Sets the verbosity level for janitor logs. Inherited from `global.logLevel` when not overridden.

```yaml
janitor:
  logLevel: info  # Options: debug, info, warn, error
```

## Operation Timeout

Global default timeout applied to all node operations when no controller-specific timeout is set.

```yaml
janitor:
  config:
    timeout: "25m"
```

Choose a value that covers the slowest expected operation for your cloud provider. AWS, Azure, and OCI node reboots typically complete within 25 minutes. GCP can be shorter. For `kind` clusters used in development, a much shorter value (e.g. `"5m"`) is sufficient.

## CSP Provider Connection

The Janitor connects to the Janitor Provider over gRPC. Configure the endpoint and connection security here.

```yaml
janitor:
  config:
    cspProviderHost: "janitor-provider.nvsentinel.svc.cluster.local:50051"
```

`cspProviderHost` is the gRPC address of the Janitor Provider. Change this only when the Janitor Provider is deployed in a different namespace or under a custom service name.

### TLS

```yaml
janitor:
  config:
    cspProvider:
      tls:
        enabled: true
        caSecretName: "janitor-provider-grpc-cert"
        insecure: false
```

### enabled
When `true`, the Janitor uses TLS for the gRPC connection to the Janitor Provider. Disable only in isolated test environments.

### caSecretName
Name of the Kubernetes Secret containing `ca.crt` used to verify the Janitor Provider's TLS certificate. The self-signed certificate is managed by cert-manager, which is a required dependency.

### insecure
Set to `true` to skip TLS certificate verification. For development use only; never enable in production.

### Service Account Token Auth

```yaml
janitor:
  config:
    cspProvider:
      auth:
        enabled: true
        audience: "nvsentinel-csp-provider"
        expirationSeconds: 3600
```

### enabled
When `true`, the Janitor mounts a projected ServiceAccount token and sends it to the Janitor Provider for authentication.

### audience
The token audience must match the `auth.audiences` value configured on the Janitor Provider.

### expirationSeconds
Requested lifetime of the projected ServiceAccount token in seconds. Kubernetes automatically rotates the token before expiry.

## Manual Mode

```yaml
janitor:
  config:
    manualMode: false
```

When `true`, the Janitor creates the Custom Resource but does not call the Janitor Provider to execute any node operation. Use this to test the fault-remediation → janitor CR creation pipeline without triggering actual node reboots, terminations, or GPU resets.

## HTTP Port

```yaml
janitor:
  config:
    httpPort: 8082
```

Port for the Janitor's internal HTTP server (health and readiness endpoints).

## Node Exclusions

Stops new `RebootNode`, `TerminateNode`, and `GPUReset` CRs from being created for the matching nodes.

```yaml
janitor:
  config:
    nodes:
      exclusions: []
```

Each entry is a Kubernetes [label selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors), not a node name. Use `matchLabels`, `matchExpressions`, or both. A node that matches **any** entry is excluded.

```yaml
janitor:
  config:
    nodes:
      exclusions:
        # Every control-plane node
        - matchLabels:
            node-role.kubernetes.io/control-plane: ""

        # One specific node, selected through its hostname label
        - matchLabels:
            kubernetes.io/hostname: gpu-node-07

        # Any node in a critical or production tier
        - matchExpressions:
            - key: workload-tier
              operator: In
              values:
                - critical
                - production
```

To exclude one node, select it through the `kubernetes.io/hostname` label that kubelet sets on every node. Read the value first, because it does not always match the node's object name:

```bash
kubectl get node gpu-node-07 -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}'
```

One list covers all three controllers: RebootNode, TerminateNode, and GPUReset.

### How exclusions are enforced

A validating admission webhook compares the node's labels against every exclusion when a Janitor CR is created, and rejects the request on the first match:

```text
node 'control-plane-1' is excluded from janitor operations due to a label on the node matching the label exclusion 'node-role.kubernetes.io/control-plane=' from config value global.nodes.exclusions
```

The check runs server-side on the API request, so it applies to every client, and `force: true` does not bypass it. Because it matches on labels read at admission time, relabelling a node changes what is excluded without a Janitor restart.

Admission is the only place exclusions are enforced. The controllers do not re-check them while reconciling, so a CR admitted before you added the exclusion, or before you labelled the node, still runs to completion. After you add an exclusion, check for CRs that are already in flight for those nodes:

```bash
kubectl get rebootnodes,terminatenodes,gpuresets -A
```

Use this for control-plane nodes, infrastructure nodes, or any node that NVSentinel should not reboot or terminate.

## Controllers

Each controller handles one CR type. All three are enabled by default.

### RebootNode Controller

```yaml
janitor:
  config:
    controllers:
      rebootNode:
        enabled: true
        timeout: "25m"
```

Handles `RebootNode` CRs. `timeout` overrides `config.timeout` for reboot operations.

### TerminateNode Controller

```yaml
janitor:
  config:
    controllers:
      terminateNode:
        enabled: true
        timeout: "25m"
```

Handles `TerminateNode` CRs. `timeout` overrides `config.timeout` for terminate operations.

### GPUReset Controller

```yaml
janitor:
  config:
    controllers:
      gpuReset:
        enabled: true
        timeout: "25m"
        serviceManager:
          name: "gpu-operator"
        resetJob:
          writeSysLogEvent: true
          runtimeClassName: "nvidia"
          hostDriverRootPath: "/run/nvidia/driver"
          driverRoot: "/run/nvidia/driver"
          image:
            repository: ghcr.io/nvidia/nvsentinel/gpu-reset
            tag: ""
          resources:
            requests:
              cpu: "50m"
              memory: "64Mi"
            limits:
              cpu: "100m"
              memory: "128Mi"
```

Handles `GPUReset` CRs. Before issuing the GPU reset, the controller pauses the deployment or DaemonSet named by `serviceManager.name` to prevent the GPU Operator from interfering with the reset sequence.

### serviceManager.name
Name of the Kubernetes Deployment or DaemonSet to pause during GPU reset. Set to the GPU Operator deployment name in your cluster.

### resetJob.writeSysLogEvent
When `true`, the reset job writes a kernel syslog message on reset completion. Useful for correlating reset events with node-level logs.

### resetJob.runtimeClassName
NVIDIA RuntimeClass name used by the GPU reset Job. Must match a RuntimeClass installed in the cluster.

### resetJob.hostDriverRootPath
Host path containing the NVIDIA driver filesystem. The reset Job mounts it at `resetJob.driverRoot` inside the reset container.

Keep the default `/run/nvidia/driver` for containerized driver installations. Set it to `/` when the driver is installed directly into the host filesystem and `nvidia-smi` is available at a path such as `/usr/bin/nvidia-smi`.

### resetJob.driverRoot
Path inside the reset container that the reset command chroots into. The reset command runs `chroot <driverRoot> nvidia-smi` for every `nvidia-smi` call, so this path must hold a complete filesystem: the `nvidia-smi` binary, its shared libraries, and a dynamic loader. The reset Job also mounts the host `/sys` at `<driverRoot>/sys`, so the chroot can read sysfs.

Keep the default `/run/nvidia/driver`. The GPU Operator driver container bind-mounts its full container root to that host path, which satisfies the chroot. This default also works with `hostDriverRootPath: "/"`, because the host root filesystem is complete as well.

Set `driverRoot` to `/` when the driver root holds only driver files and not a complete filesystem. A chroot into a partial driver root fails before `nvidia-smi` starts. With `/`, the chroot is a no-op and the reset command uses the `nvidia-smi` in the reset container image. The Job then does not mount `hostDriverRootPath`, and it mounts the host `/sys` at `/sys`.

The value must be an absolute, clean path. The janitor rejects any other value at startup.

### resetJob.image
Container image for the GPU reset Job. Leave `tag` empty to use the chart default.

### resetJob.resources
Resource requests and limits for the GPU reset Job container.

## TTL-Based CR Cleanup

Completed CRs are automatically deleted after the TTL expires.

```yaml
janitor:
  ttl:
    enabled: true
    defaultTTL: "336h"
```

### enabled
When `true`, the TTL controller deletes completed `RebootNode`, `TerminateNode`, and `GPUReset` CRs after `defaultTTL` has elapsed since completion.

### defaultTTL
Duration after which completed CRs are deleted. Default is `336h` (14 days). Use a shorter value (e.g. `"24h"`) in test environments to keep the CR list clean.

## Webhook

The Janitor uses an admission webhook to validate CRs before they are persisted.

```yaml
janitor:
  webhook:
    port: 9443
    certIssuer: "janitor-selfsigned-issuer"
    certProvider: cert-manager
```

### certProvider
cert-manager is a required dependency. The webhook certificate is issued by the `certIssuer` ClusterIssuer and renewed automatically.
