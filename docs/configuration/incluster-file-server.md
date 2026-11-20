# In-Cluster File Server Configuration

## Overview

The In-Cluster File Server is an nginx-based server. It stores the diagnostic log bundles that the fault-remediation log collector collects. The server organises the logs on a persistent volume by node name and timestamp, and serves them over HTTP inside the cluster. You need this module only when `fault-remediation.logCollector.enabled: true`. This document gives all the Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the Helm chart deploys the incluster-file-server module in the cluster. You also configure the metrics ports of the server and its cleanup sidecar here.

```yaml
global:
  inclusterFileServer:
    enabled: false

incluster-file-server:
  metricsPort: 9001
  cleanupMetricsPort: 9002
```

#### metricsPort

Port on which the nginx access-log Prometheus exporter listens. You configure it under `incluster-file-server:`, not `global`. Default: `9001`.

#### cleanupMetricsPort

Port on which the log-cleanup sidecar exposes its Prometheus metrics. You configure it under `incluster-file-server:`, not `global`. Default: `9002`.

### Persistence

Configures the PersistentVolumeClaim that stores the diagnostic log bundles.

```yaml
incluster-file-server:
  persistence:
    enabled: true
    storageClassName: ""
    accessModes:
      - ReadWriteOnce
    size: 50Gi
```

#### enabled

When `true`, the chart creates a PersistentVolumeClaim and mounts it into the nginx container. In development or test environments that do not need log persistence, you can set it to `false`. In other environments, keep it enabled.

#### storageClassName

Storage class to use for the PVC. An empty string (`""`) selects the cluster default storage class. For production deployments, specify a storage class that supports dynamic provisioning and meets your I/O and redundancy requirements.

```yaml
incluster-file-server:
  persistence:
    storageClassName: "standard-rwo"
```

#### accessModes

Access modes for the PVC. `ReadWriteOnce` is sufficient because the file server runs as a single pod.

#### size

Total capacity to request. The default is `50Gi`. The appropriate size depends on these values:

- The number of nodes that can fail at the same time.
- The size of each `nvidia-bug-report` bundle (typically 5–50 MB per collection event).

To detect when the volume approaches capacity, monitor the `fileserver_disk_space_free_bytes` Prometheus metric.

### Service

Configures the Kubernetes Service that exposes the nginx server to other pods in the cluster.

```yaml
incluster-file-server:
  service:
    type: ClusterIP
    port: 80
    targetPort: 8080
```

#### type

Kubernetes Service type. `ClusterIP` is the default. With this type, the server is accessible only from within the cluster.

#### port

Port on which the Service listens. Default: `80`.

#### targetPort

Port on which nginx listens inside the container. Default: `8080`.

### Log Cleanup

Configures the sidecar that periodically deletes log files older than a retention threshold.

```yaml
incluster-file-server:
  logCleanup:
    enabled: true
    retentionDays: 7
    sleepInterval: 86400
```

#### enabled

When `true`, a cleanup sidecar runs alongside nginx and deletes files that exceed the retention period.

#### retentionDays

Number of days to retain log bundles before deletion. The value must be 1 or greater. The chart rejects values less than 1, to prevent accidental data loss. Default: `7`.

#### sleepInterval

Time in seconds between cleanup cycles. Default: `86400` (24 hours). If storage space is limited and you need more frequent cleanup, reduce this value.

### Metrics

Configures the Prometheus nginx access-log exporter sidecar.

```yaml
incluster-file-server:
  metrics:
    enabled: true
```

#### enabled

When `true`, the chart deploys the `prometheus-nginxlog-exporter` sidecar. This sidecar exposes nginx access-log metrics on `metricsPort`. These metrics include request counts and response codes. A Prometheus installation must scrape the metrics endpoint.

### Cleanup Metrics

Image for the sidecar that exports metrics about the log cleanup job and the free disk space (`fileserver_disk_space_free_bytes`). The sidecar serves these metrics on `cleanupMetricsPort` (default `9002`).

```yaml
incluster-file-server:
  cleanupMetrics:
    image:
      repository: ghcr.io/nvidia/nvsentinel/file-server-cleanup
      tag: ""
      pullPolicy: IfNotPresent
```

An empty `tag` inherits `global.image.tag`. If you mirror images into a private registry, change these values. Otherwise, keep the defaults.

### Security Context

Runs the file server as a non-root user, and controls how Kubernetes sets the ownership of the persistent volume.

```yaml
incluster-file-server:
  podSecurityContext:
    fsGroup: 101
    fsGroupChangePolicy: OnRootMismatch
  containerSecurityContext:
    runAsUser: 1001
    runAsGroup: 1001
    allowPrivilegeEscalation: false
    readOnlyRootFilesystem: false
```

Kubernetes sets the group ownership of the volume to `fsGroup`. It also adds that group to the container process as a supplementary group. Thus, the server writes to the volume, although its primary group is different. If you change `fsGroup` and do not also reconcile the ownership of an existing volume, the server cannot write to the volume.

`fsGroupChangePolicy: OnRootMismatch` re-applies ownership only when the volume root does not already match. This avoids a recursive `chown` over a large log archive on every pod start.

`readOnlyRootFilesystem` is `false` because nginx writes its temporary files to the container filesystem.

### Resources

Defines CPU and memory resource requests and limits for the nginx container.

```yaml
incluster-file-server:
  resources: {}
```

By default, the chart sets no resource requests or limits. For production deployments, define explicit limits, so that the file server does not compete with workload pods for node resources.

**Example:**

```yaml
incluster-file-server:
  resources:
    limits:
      cpu: 500m
      memory: 256Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

## Accessing Stored Logs

The file server serves the log bundles at `http://{service}/{NODE_NAME}/`. It organises them by collection timestamp in each node directory. To browse logs from a local machine, port-forward the service:

```bash
kubectl port-forward -n nvsentinel svc/nvsentinel-incluster-file-server 8080:80
```

Then open `http://localhost:8080/{NODE_NAME}/` in a browser or fetch files with `curl`:

```bash
curl http://localhost:8080/{NODE_NAME}/
```

## Log Cleanup and Storage Management

The cleanup sidecar runs on the `sleepInterval` schedule. It deletes each file whose modification time is older than `retentionDays`. The cleanup runs in the background, and does not interrupt active HTTP downloads.

The volume can fill unexpectedly, for example after a large-scale node failure that triggers many concurrent log collections. In that case, follow the emergency cleanup procedure in [Log Rotation Failures](../runbooks/log-rotation-failures.md).

To get an early warning before the volume reaches capacity, monitor the `fileserver_disk_space_free_bytes` metric.
