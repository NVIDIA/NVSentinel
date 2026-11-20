# Janitor Configuration

## Overview

The Janitor module watches for the Kubernetes Custom Resources (`RebootNode`, `TerminateNode`, `GPUReset`) that fault-remediation creates. For a RebootNode or TerminateNode CR, it does the node operation through calls to the Janitor Provider over gRPC. For a GPUReset CR, it runs a privileged reset Job on the node. This document gives all the Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the Helm chart deploys the Janitor module in the cluster.

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

Defines CPU and memory resource requests and limits for the janitor pod. The chart sets no defaults. Configure these values for production use.

```yaml
janitor:
  resources: {}
```

### Logging

Sets the verbosity level for janitor logs.

```yaml
janitor:
  logLevel: info  # Options: debug, info, warn, error
```

## Operation Timeout

Global default timeout for all node operations that have no controller-specific timeout.

```yaml
janitor:
  config:
    timeout: "25m"
```

Choose a value that covers the slowest expected operation for your cloud provider. AWS, Azure, and OCI node reboots typically complete within 25 minutes. GCP can be shorter. On Akamai (LKE), a GPU node reboot and a node recycle each take about 3 minutes. For `kind` clusters in development, a much shorter value, for example `"5m"`, is sufficient.

## CSP Provider Connection

The Janitor connects to the Janitor Provider over gRPC. Configure the endpoint and connection security here.

```yaml
janitor:
  config:
    cspProviderHost: "janitor-provider.nvsentinel.svc.cluster.local:50051"
```

`cspProviderHost` is the gRPC address of the Janitor Provider. If you deploy the Janitor Provider in a different namespace or under a custom service name, change this value. Otherwise, keep the default.

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

When `true`, the Janitor uses TLS for the gRPC connection to the Janitor Provider. In isolated test environments, you can disable it. In all other environments, keep it enabled.

### caSecretName

Name of the Kubernetes Secret that contains `ca.crt`. The Janitor uses this file to verify the TLS certificate of the Janitor Provider. cert-manager manages the self-signed certificate, and is a required dependency.

### insecure

In development, you can set it to `true` to skip TLS certificate verification. In all other environments, including production, keep it `false`.

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

The token audience must match the `auth.audiences` value that you configure on the Janitor Provider.

### expirationSeconds

Requested lifetime of the projected ServiceAccount token in seconds. Kubernetes automatically rotates the token before expiry.

## Manual Mode

```yaml
janitor:
  config:
    manualMode: false
```

When `true`, the RebootNode and TerminateNode controllers do not call the Janitor Provider. Thus, the Janitor does not trigger real node reboots or terminations. Manual mode does not apply to GPUReset CRs: the Janitor still runs each GPU reset. Use this setting to test the creation of janitor CRs by fault-remediation, without real node reboots or terminations.

## HTTP Port

```yaml
janitor:
  config:
    httpPort: 8082
```

Port for the Janitor's internal HTTP server (health and readiness endpoints).

## Node Exclusions

Prevents the creation of new `RebootNode`, `TerminateNode`, and `GPUReset` CRs for the matching nodes.

```yaml
janitor:
  config:
    nodes:
      exclusions: []
```

Each entry is a Kubernetes [label selector](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors), not a node name. Use `matchLabels`, `matchExpressions`, or both. The Janitor excludes a node that matches **any** entry.

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

To exclude one node, select it through the `kubernetes.io/hostname` label that kubelet sets on every node. Read the value first, because it does not always match the object name of the node:

```bash
kubectl get node gpu-node-07 -o jsonpath='{.metadata.labels.kubernetes\.io/hostname}'
```

One list covers all three controllers: RebootNode, TerminateNode, and GPUReset.

### How exclusions are enforced

When a client creates a Janitor CR, a validating admission webhook compares the labels of the node with every exclusion. The webhook rejects the request on the first match:

```text
node 'control-plane-1' is excluded from janitor operations due to a label on the node matching the label exclusion 'node-role.kubernetes.io/control-plane=' from config value global.nodes.exclusions
```

The check runs server-side on the API request. Thus, it applies to every client, and `force: true` does not bypass it. The check reads the node labels at admission time. Thus, when you change the labels of a node, the excluded nodes change without a Janitor restart.

The webhook enforces exclusions only at admission. The controllers do not check them again during reconciliation. Thus, a CR that the webhook admitted before you added the exclusion, or before you labelled the node, still runs to completion. After you add an exclusion, check for CRs that are already in progress for those nodes:

```bash
kubectl get rebootnodes,terminatenodes,gpuresets -A
```

Use exclusions for control-plane nodes, infrastructure nodes, or any node that NVSentinel must not reboot or terminate.

## Controllers

Each controller handles one CR type. By default, all three controllers are enabled.

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
          driverBinPath: ""
          jobTemplatePatch: {}
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

Handles `GPUReset` CRs. For each GPU reset, the controller does these steps:

1. It removes the GPU Operator operands that hold device handles from the node. To do this, it sets their `nvidia.com/gpu.deploy.*` node labels to `false`.
2. It waits for the pods to leave.
3. It runs the reset Job.
4. It sets the labels back to `true`.
5. It waits for the pods to be Ready.

### serviceManager.name

Name of the built-in GPU services manager. Use `gpu-operator` for the NVIDIA GPU Operator.

#### GPU Operator modes

The `gpu-operator` service manager supports both GPU Operator modes: device plugin and DRA. You do not set the mode. The controller finds the GPU Operator operands that run on each node.

Each managed operand has a deploy node label. To stop an operand on the node, the controller sets its label to `false`. After the reset, the controller sets the label back to `true` and waits until the operand pods are ready.

| GPU Operator mode | Pod labels | Node label |
|---|---|---|
| device plugin | `app=nvidia-device-plugin-daemonset`, `app.kubernetes.io/managed-by=gpu-operator` | `nvidia.com/gpu.deploy.device-plugin` |
| device plugin | `app=nvidia-dcgm`, `app.kubernetes.io/managed-by=gpu-operator` | `nvidia.com/gpu.deploy.dcgm` |
| device plugin | `app=nvidia-dcgm-exporter`, `app.kubernetes.io/managed-by=gpu-operator` | `nvidia.com/gpu.deploy.dcgm-exporter` |
| device plugin | `app=gpu-feature-discovery`, `app.kubernetes.io/managed-by=gpu-operator` | `nvidia.com/gpu.deploy.gpu-feature-discovery` |
| DRA | `app=nvidia-dcgm-dra` | `nvidia.com/gpu.deploy.dcgm-dra` |
| DRA | `app=nvidia-dcgm-exporter-dra` | `nvidia.com/gpu.deploy.dcgm-exporter-dra` |

When the teardown starts, the controller lists the pods of each operand on the node. A pod in any phase counts. The controller stops only the operands that have a pod on the node. It records their node labels in the GPUReset status, in `status.managedServices.nodeLabels`. The restore and the GPUReset finalizer use this record. The controller does not change the node labels of the other operands.

The controller changes a node label only when the label holds the enabled or the disabled value. A label that is absent does not count as held. The teardown sets an absent label to the disabled value, and the restore sets it to the enabled value.

Another controller can hold a label at a different value. For example, the GPU Operator upgrade controller sets `nvidia.com/gpu.deploy.dcgm=paused-for-driver-upgrade` during a driver upgrade. The controller then waits and retries:

- Before the teardown starts, the controller checks the labels of every operand of the service manager. It also checks the operands that have no pod on the node, because a driver upgrade removes the operand pods. The `gpuReset.timeout` value limits this wait.
- The same check runs before the controller creates the reset Job.
- During the teardown, the `teardownTimeout` of the service manager limits the wait. During the restore, `restoreTimeout` limits the wait.

When the wait times out, the GPUReset fails with the reason `ServiceTeardownTimeoutExceeded`, `ResetJobCreationFailed` or `RestoreTimeoutExceeded`.

A GPUReset can fail after it stopped services, for example on the restore timeout or on a held label before the reset Job. Then the GPUReset keeps the node lock for one more `gpuReset.timeout`. During this time, it continues to restore the node labels, but it does not wait for the pods. Only the controller writes those labels back. The GPU Operator does not change a label that holds the disabled value.

If a label stays held for that long, these things occur:

- The controller leaves that label as it is.
- The controller restores the other labels.
- The `ServicesRestored` condition names the held label.
- The controller releases the lock.

The deletion of a GPUReset waits in the same way.

When no operand pod runs on the node, the controller skips the teardown and the restore. The `ServicesTornDown` and `ServicesRestored` conditions then have the reason `Skipped`.

Before the controller creates the reset Job, it verifies that no pod of a stopped operand runs on the node.

The janitor Pod cache holds only the pods in the service manager namespace that have one of the `app` labels of the service manager.

The controller finds the operands only one time, when the teardown starts. The controller does not stop an operand pod that is not on the node at that time. For example, a DaemonSet rolling update can remove the pod for a short time. The pod can then start again during the reset and keep the GPU open. Then the reset fails.

#### Values per GPU Operator mode

The built-in `gpu-operator` service manager and the `/run/nvidia/driver` driver paths work unchanged on ClusterPolicy and `GPUCluster` (DRA) clusters. The one value that differs is `resetJob.runtimeClassName`. With `global.gpuDraEnabled: true`, this value must be `""`. Set `serviceManager.spec` only in these cases:

- For a GPU services manager other than the GPU Operator.
- To stop a GPU Operator operand that the built-in entry does not cover, as in the [host-installed driver example](#example-host-installed-driver-on-gke).

The spec replaces the built-in entry completely, including its mode detection. Thus, list every operand to stop.

#### GPU clients

The controller also stops the GPU client pods before the reset. It does this with every service manager, and also when you set no service manager.

A GPU client is a pod that holds the GPUs open, but that the GPU Operator does not manage. Examples are a monitoring agent and a health agent. If a GPU client runs on the node, `nvidia-smi --gpu-reset` fails with `In use by another client`. GPU clients can run in any namespace, so the `serviceManager` cannot stop them.

GPU Operator 26.7 and later sets the `nvidia.com/gpu.deploy.client=true` node label on each GPU node. A GPU client selects this label in its `nodeSelector`. The NVSentinel metadata collector selects it in its required node affinity. The controller does these steps:

1. When the teardown starts, the controller reads `nvidia.com/gpu.deploy.client` on the node. The controller stops the GPU clients only when the label is `true` and at least one GPU client pod runs on the node.
2. The controller sets the label to `false`, and records it in `status.managedServices.nodeLabels`. The DaemonSet controller then removes the GPU client pods from the node.
3. The controller waits until no GPU client pod runs on the node. A GPU client pod is a pod in any namespace that selects the label. The pod selects the label in its `nodeSelector` or in a term of its required node affinity. For a DaemonSet pod, the controller also checks the pod template of the DaemonSet. The reason is that the DaemonSet controller replaces the required node affinity of the pod with a term on the node name. A pod that has finished is not a GPU client pod.
4. After the reset, the controller sets the label back to `true`. It does not wait for the GPU client pods to be Ready.

The controller does not change the label in these cases:

- The node has no label. GPU Operator versions before 26.7, and `GPUCluster` mode, do not set it.
- The label is `false`. The controller does not wait for GPU client pods. A DaemonSet does not own some GPU client pods, for example a Deployment or Job pod. Such a pod can still run on the node and keep the GPU open.
- The label is `true`, but no GPU client pod runs on the node.
- The label has another value, for example `paused-for-driver-upgrade`. The GPU Operator holds the label during a driver upgrade or a MIG change. The controller waits, as for an operand label.

Know these limits:

- The controller does not delete pods. Only the DaemonSet controller removes a pod when the label changes. A Deployment, StatefulSet or Job pod stays on the node. The teardown then fails with the reason `ServiceTeardownTimeoutExceeded`, and the controller logs the names of the GPU client pods that still run.
- The controller does not stop processes that run on the host outside Kubernetes, for example `nvidia-persistenced` or a host DCGM.
- The controller reads the GPU client pods from the API server, not from the janitor Pod cache. Each check lists the pods of one node, and reads the DaemonSet of each DaemonSet pod that does not select the label itself.

For GPU Operator versions before 26.7, put the GPU client on another node label. Use a label that the `gpu-operator` service manager already sets to `false`, for example `nvidia.com/gpu.deploy.dcgm=true`.

### resetJob.writeSysLogEvent

When `true`, the reset job writes a kernel syslog message when the reset completes. This message helps you correlate reset events with node-level logs.

### resetJob.runtimeClassName

RuntimeClass name for the GPU reset Job. The default is `nvidia`.

GPU Operator `GPUCluster` (DRA) mode creates no `nvidia` RuntimeClass. Thus, with `global.gpuDraEnabled: true`, this value must be `""`. Otherwise, the chart refuses to render. The Job is privileged and reaches the driver through `resetJob.hostDriverRootPath`. Thus, it does not need a RuntimeClass in either mode.

Some ClusterPolicy clusters have no `nvidia` RuntimeClass, for example with the GPU Operator NRI plugin enabled. On such a cluster, also set it to `""`.

### resetJob.hostDriverRootPath

Host path that contains the NVIDIA driver filesystem. The reset Job mounts it at `resetJob.driverRoot` inside the reset container.

For containerized driver installations, keep the default `/run/nvidia/driver`. If both of these conditions are true, set it to `/`:

- The driver is installed directly into the host filesystem.
- `nvidia-smi` is available at a path such as `/usr/bin/nvidia-smi`.

### resetJob.driverRoot

Path inside the reset container that the reset command chroots into. The reset command runs `chroot <driverRoot> nvidia-smi` for every `nvidia-smi` call. Thus, this path must hold a complete filesystem: the `nvidia-smi` binary, its shared libraries, and a dynamic loader. The reset Job also mounts the host `/sys` at `<driverRoot>/sys`, so the chroot can read sysfs.

Keep the default `/run/nvidia/driver`. The GPU Operator driver container bind-mounts its full container root to that host path. This mount satisfies the chroot. This default also works with `hostDriverRootPath: "/"`, because the host root filesystem is also complete.

If the driver root holds only driver files and not a complete filesystem, set `driverRoot` to `/`. A chroot into a partial driver root fails before `nvidia-smi` starts. With `/`, the chroot is a no-op, and the reset command uses the `nvidia-smi` in the reset container image. The Job then does not mount `hostDriverRootPath`, and it mounts the host `/sys` at `/sys`.

The value must be an absolute, clean path. The Janitor rejects any other value at startup.

### resetJob.driverBinPath

Directory inside `resetJob.driverRoot` that holds `nvidia-smi` and `nvidia-bug-report.sh`. The reset command adds it to the front of `PATH` for every command that it runs in the chroot. If these files are on the default `PATH`, for example with the GPU Operator driver container, keep it empty.

The value is empty by default. If you set a value, it must be an absolute, clean path without `:`. The Janitor rejects any other value at startup.

#### Example: host-installed driver on GKE

On Container-Optimized OS (COS) nodes, the GKE driver installer puts the driver in `/home/kubernetes/bin/nvidia` on the host. No GPU Operator driver container publishes `/run/nvidia/driver`. The GPU Operator then runs with `driver.enabled: false`, and its container toolkit is installed in the same directory.

This setup needs three changes:

- Chroot into the host root. Point the reset command at the driver binaries with `driverBinPath`. The installer adds `/home/kubernetes/bin/nvidia/lib64` to the dynamic loader configuration of the host. Thus, the chroot finds the driver libraries without more settings.
- Stop the GPU Operator container toolkit during the reset. In this setup, the toolkit process keeps every GPU device open, and the reset fails with `In use by another client`. The built-in `gpu-operator` entry does not stop the toolkit. Thus, set `serviceManager.spec` with the device plugin mode operands and the toolkit.
- Set `runtimeClassName` to `""`. When the toolkit stops, it removes the `nvidia` runtime from containerd. Then a reset Job that requests that RuntimeClass cannot start.

```yaml
janitor:
  config:
    controllers:
      gpuReset:
        serviceManager:
          name: "gpu-operator"
          spec:
            namespace: "gpu-operator"
            teardownTimeout: "5m"
            restoreTimeout: "10m"
            apps:
              - appSelector: {app: "nvidia-device-plugin-daemonset", app.kubernetes.io/managed-by: "gpu-operator"}
                nodeLabel: "nvidia.com/gpu.deploy.device-plugin"
                enabledValue: "true"
                disabledValue: "false"
              - appSelector: {app: "nvidia-dcgm", app.kubernetes.io/managed-by: "gpu-operator"}
                nodeLabel: "nvidia.com/gpu.deploy.dcgm"
                enabledValue: "true"
                disabledValue: "false"
              - appSelector: {app: "nvidia-dcgm-exporter", app.kubernetes.io/managed-by: "gpu-operator"}
                nodeLabel: "nvidia.com/gpu.deploy.dcgm-exporter"
                enabledValue: "true"
                disabledValue: "false"
              - appSelector: {app: "gpu-feature-discovery", app.kubernetes.io/managed-by: "gpu-operator"}
                nodeLabel: "nvidia.com/gpu.deploy.gpu-feature-discovery"
                enabledValue: "true"
                disabledValue: "false"
              - appSelector: {app: "nvidia-container-toolkit-daemonset", app.kubernetes.io/managed-by: "gpu-operator"}
                nodeLabel: "nvidia.com/gpu.deploy.container-toolkit"
                enabledValue: "true"
                disabledValue: "false"
        resetJob:
          runtimeClassName: ""
          hostDriverRootPath: "/"
          driverRoot: "/run/nvidia/driver"
          driverBinPath: "/home/kubernetes/bin/nvidia/bin"
```

The reset fails while any process holds the GPU open. If another GPU client runs on the node, for example a DRA kubelet plugin, also add its operand to the spec.

### resetJob.jobTemplatePatch

Changes the GPU reset Job. Use it for these changes:

- Add host mounts, init containers or a service account.
- Wrap the reset command.
- Give the Job a longer deadline.

It is empty by default. An empty value leaves the Job unchanged.

The Janitor applies the value as a Kubernetes strategic merge patch on top of the built-in Job, after the other `resetJob` keys. If the patch and a `resetJob` key set the same field, the patch wins. The Janitor checks the patch when it starts. Each of these problems stops the Janitor with an error:

- A misspelled field
- A field name in the wrong case
- A value of the wrong type

Some fields are specific to each reset. Thus, the Janitor always sets these fields itself and ignores the patch for them:

- The Job name, namespace and owner labels
- `nodeName`
- `NVIDIA_GPU_RESETS` in each container and init container

#### How lists merge

A strategic merge patch does not replace a list. It matches the items in your patch with the built-in items by a key:

- `containers`, `initContainers`, `volumes` and `env` match by `name`. Use a new name to add an item. To change a built-in item, use its name, such as `gpu-reset`. Only the fields that you set change, and the other fields stay as they are.
- `volumeMounts` match by `mountPath`, not by name. If you give a built-in mount a different `mountPath`, you get a second mount and the first one stays. To move the driver mount, use `hostDriverRootPath` and `driverRoot` instead.
- `tolerations` have no key. Thus, a patch that sets `tolerations` replaces the whole list. The built-in list has one toleration, `operator: Exists`. This toleration keeps the reset pod running on a node with a `NoExecute` taint. To keep this toleration, include it in your list.
- To change a built-in env var that has a value, such as `UPLOAD_URL_BASE`, to `valueFrom`, also set `value: null`. Otherwise, the variable has both `value` and `valueFrom`, and the API server rejects the Job.
- To remove a built-in item, list it by its key with `$patch: delete`.

#### Example

```yaml
janitor:
  config:
    controllers:
      gpuReset:
        resetJob:
          jobTemplatePatch:
            spec:
              activeDeadlineSeconds: 900
              template:
                spec:
                  serviceAccountName: provider-reset
                  volumes:
                    - name: host-systemd
                      hostPath:
                        path: /run/systemd
                    - name: reset-hooks
                      configMap:
                        name: provider-reset-hooks
                        defaultMode: 0755
                  initContainers:
                    - name: stop-provider-daemonsets
                      image: provider/reset-hooks:1.0
                  containers:
                    - name: gpu-reset
                      command: ["/hooks/reset-with-restore.sh"]
                      volumeMounts:
                        - name: host-systemd
                          mountPath: /run/systemd
                        - name: reset-hooks
                          mountPath: /hooks
```

This patch does these changes:

- It sets the deadline to 900 seconds.
- It adds a service account, two volumes and an init container.
- It runs the reset through a wrapper script.

The `gpu-reset` container keeps everything else: its image, environment, security context and built-in mounts.

The reset image does not contain the wrapper. Thus, you must supply it. In this example, the wrapper comes from the `provider-reset-hooks` ConfigMap in the janitor namespace. The Job mounts this ConfigMap at `/hooks`. You can also build your own image on top of the reset image.

#### Before you set a patch

- Keep the reset in the Job. The Janitor uses the Job result as the reset result. Thus, a wrapper must run the reset and exit with its exit code.
- Create everything that the patch refers to, such as the ServiceAccount, in the janitor namespace. If something is missing, you find the problem only when a reset runs. At that time, the node is already cordoned and drained.
- Undo any host changes that the Job makes. The Janitor restores only the GPU services that it stopped.
- Set `gpuReset.timeout` higher than the Job's `activeDeadlineSeconds` plus the time that the Janitor needs before it starts the Job. In that time, the Janitor stops the GPU services and waits for node labels that another controller holds. The timeout starts when the GPUReset starts. If you delete a GPUReset, the Janitor waits for a running Job only until the timeout. Then it restores the GPU services, even if the Job still runs. Do not delete a GPUReset while its Job runs.
- Before you use a patch across the cluster, try it on one node.

### resetJob.image

Container image for the GPU reset Job. To use the chart default, keep `tag` empty.

### resetJob.resources

Resource requests and limits for the GPU reset Job container.

## TTL-Based CR Cleanup

The Janitor deletes maintenance CRs after their TTL expires.

```yaml
janitor:
  ttl:
    enabled: true
    defaultTTL: "336h"
```

### enabled

When `true`, the TTL controller deletes `RebootNode`, `TerminateNode`, `GPUReset` and `ExternalRemediationRequest` CRs after `defaultTTL` has elapsed.

The TTL controller never deletes an open `ExternalRemediationRequest`. Its TTL starts at `status.completionTime`. A request where the external system set `ExternalRemediationComplete=False` is also open, because the node is still released to the external system. To keep an `ExternalRemediationRequest` after completion, set the `nvsentinel.nvidia.com/preserve: "true"` annotation.

For a completed `ExternalRemediationRequest`, the Janitor calculates the deletion time on every reconcile. The deletion time is `status.completionTime` plus the `nvsentinel.nvidia.com/ttl` value. The Janitor writes the result to `nvsentinel.nvidia.com/expiry` for information. A change to the `ttl` annotation after completion changes the deletion time.

### defaultTTL

Time after which the Janitor deletes a CR. The default is `336h` (14 days). For `RebootNode`, `GPUReset` and `TerminateNode`, the time starts when the Janitor first reconciles the CR. For `ExternalRemediationRequest`, the time starts at `status.completionTime`. In test environments, use a shorter value, for example `"24h"`, to keep the CR list clean.

## Webhook

The Janitor uses an admission webhook to validate CRs before the API server stores them.

```yaml
janitor:
  webhook:
    port: 9443
    certIssuer: "janitor-selfsigned-issuer"
    certProvider: cert-manager
```

### certProvider

cert-manager is a required dependency. The `certIssuer` Issuer issues the webhook certificate, and cert-manager renews it automatically.

## Metrics TLS

Serves the Prometheus metrics endpoint over TLS, with a cert-manager issued certificate.

```yaml
janitor:
  metrics:
    tls:
      enabled: false
      certDir: "/tmp/k8s-metrics-server/metrics-certs"
      issuerName: ""
      issuerKind: "Issuer"
      issuerGroup: ""
      duration: "2160h"
      renewBefore: "720h"
      organization: "NVIDIA"
```

### enabled

Enables TLS for the metrics endpoint. It is disabled by default. If you enable it, your scrape configuration must trust the issuing CA. Otherwise, Prometheus stops collecting janitor metrics.

### certDir

Directory from which the metrics server reads its certificate and key.

### issuerName

cert-manager issuer that signs the certificate. If it is empty, the chart uses `webhook.certIssuer` instead.

### issuerKind

`Issuer` for a namespaced issuer, or `ClusterIssuer` for a cluster-scoped issuer. The default is `Issuer`. Thus, if `issuerName` names a cluster-scoped issuer, set it to `ClusterIssuer`.

### issuerGroup

API group of the issuer. For the built-in issuers of cert-manager, keep it empty. For an external issuer implementation, set it.

### duration

Certificate lifetime. The default is `2160h`, which is 90 days.

### renewBefore

The time before expiry at which cert-manager renews the certificate. The default is `720h`, which is 30 days. It must stay shorter than `duration`.

### organization

Organization name in the certificate subject.

## Inactive Values

The janitor chart has `autoscaling`, `livenessProbe`, `readinessProbe`, `podSecurityContext` and `securityContext` in `values.yaml`, but no template reads them:

- `autoscaling` renders no HorizontalPodAutoscaler. The Deployment always uses `replicaCount`.
- `livenessProbe` and `readinessProbe` add no probes to the pod.
- The chart does not apply `podSecurityContext` and `securityContext`. The only pod security context that the chart sets is `fsGroup: 65532`. The chart adds it automatically when `global.auditLogging.enabled` is true.

A change to any of these values has no effect. Do not rely on them.
