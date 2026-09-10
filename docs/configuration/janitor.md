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

Prevents specific nodes from being targeted by any Janitor operation.

```yaml
janitor:
  config:
    nodes:
      exclusions: []
```

Provide a list of node names to exclude:

```yaml
janitor:
  config:
    nodes:
      exclusions:
        - control-plane-node-1
        - infra-node-2
```

Use this for control-plane nodes, infrastructure nodes, or any node that must never be rebooted or terminated by NVSentinel.

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

### resetJob.image
Container image for the GPU reset Job. Leave `tag` empty to use the chart default.

### resetJob.resources
Resource requests and limits for the GPU reset Job container.

### resetJob.jobTemplate
An optional `batchv1.JobTemplateSpec` that fully replaces the built-in GPU reset Job template. It is empty by default, and the built-in template is used.

Use `jobTemplate` when the built-in template does not fit the node. The built-in template mounts the GPU Operator driver root from the host path `/run/nvidia/driver`, and sets `DRIVER_ROOT` to the same path. On a cluster where the host installs the driver instead of the GPU Operator, that path does not exist and the reset job fails.

The Janitor validates the template when it starts:

- An unknown field is an error. A typo fails at startup, not at the first reset.
- The template must define at least one container.
- `metadata.namespace` must be empty or the Janitor namespace. The controller finds its jobs by that namespace.
- `metadata.name` and `metadata.generateName` must be empty. The controller derives the job name from the `GPUReset` CR.
- `spec.template.spec.nodeName` must be empty. The controller sets it to the node named by the `GPUReset` CR.

The Janitor sets three things on each job it creates from the template: `spec.template.spec.nodeName`, the `GPU_RESETS` environment variable on every container, and the owner reference to the `GPUReset` CR. Everything else comes from the template, so the template must supply its own image, volumes, resources and environment.

**A replacement inherits no defaults from the built-in template.** The built-in template sets `activeDeadlineSeconds: 300`, `backoffLimit: 2` and `ttlSecondsAfterFinished: 86400`. Your template gets none of them unless you set them. Set `activeDeadlineSeconds` in particular: without it, a reset Job that hangs runs until the Janitor timeout instead of failing, and the node stays cordoned for longer. Without `ttlSecondsAfterFinished`, completed reset Jobs are never garbage collected.

The other keys under `resetJob` build the built-in template only. When you set `jobTemplate`, the chart stops emitting them and the Janitor ignores any that remain, logging a warning that names each one.

#### Example: driver installed by the host

This example resets GPUs on GKE, where `kube-system/nvidia-driver-installer` installs the driver at `/home/kubernetes/bin/nvidia`. It is the built-in template with the driver root moved to the host path, and it also shows that the template owns the fields the built-in path sets for you.

```yaml
janitor:
  config:
    controllers:
      gpuReset:
        resetJob:
          jobTemplate:
            spec:
              activeDeadlineSeconds: 300
              backoffLimit: 2
              ttlSecondsAfterFinished: 86400
              template:
                spec:
                  runtimeClassName: nvidia
                  restartPolicy: OnFailure
                  tolerations:
                    - operator: Exists
                  containers:
                    - name: gpu-reset
                      image: ghcr.io/nvidia/nvsentinel/gpu-reset:v1.20.0
                      imagePullPolicy: Always
                      securityContext:
                        privileged: true
                      env:
                        - name: NVIDIA_VISIBLE_DEVICES
                          value: void
                        - name: DRIVER_ROOT
                          value: /home/kubernetes/bin/nvidia
                        - name: WRITE_SYSLOG_EVENT
                          value: "true"
                        - name: NODE_NAME
                          valueFrom:
                            fieldRef:
                              fieldPath: spec.nodeName
                      resources:
                        requests:
                          cpu: 50m
                          memory: 64Mi
                        limits:
                          cpu: 100m
                          memory: 128Mi
                      volumeMounts:
                        - name: host-dev
                          mountPath: /dev
                        - name: dev-log
                          mountPath: /run/systemd/journal/dev-log
                        - name: driver-root
                          mountPath: /home/kubernetes/bin/nvidia
                        - name: host-sys
                          mountPath: /home/kubernetes/bin/nvidia/sys
                  volumes:
                    - name: host-dev
                      hostPath:
                        path: /dev
                    - name: dev-log
                      hostPath:
                        path: /run/systemd/journal/dev-log
                    - name: driver-root
                      hostPath:
                        path: /home/kubernetes/bin/nvidia
                    - name: host-sys
                      hostPath:
                        path: /sys
```

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
