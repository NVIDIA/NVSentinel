# In-Cluster File Server Configuration

## Overview

The In-Cluster File Server is an nginx-based server that stores diagnostic log bundles collected by the fault-remediation log collector. Logs are organised on a persistent volume by node name and timestamp, and are served over HTTP inside the cluster. This module is only needed when `fault-remediation.logCollector.enabled: true`. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the incluster-file-server module is deployed in the cluster. The metrics ports used by the server and its cleanup sidecar are also configured here.

```yaml
global:
  inclusterFileServer:
    enabled: false

incluster-file-server:
  metricsPort: 9001
  cleanupMetricsPort: 9002
```

#### metricsPort
Port on which the nginx access-log Prometheus exporter listens. Configured under `incluster-file-server:`, not `global`. Default: `9001`.

#### cleanupMetricsPort
Port on which the log-cleanup sidecar exposes its Prometheus metrics. Configured under `incluster-file-server:`, not `global`. Default: `9002`.

### Persistence

Configures the PersistentVolumeClaim used to store diagnostic log bundles.

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
When `true`, a PersistentVolumeClaim is created and mounted into the nginx container. Set to `false` only in development or test environments where log persistence is not required.

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
Total capacity to request. The default is `50Gi`. The appropriate size depends on the number of nodes that may fail simultaneously and the size of each `nvidia-bug-report` bundle (typically 5–50 MB per collection event). Monitor the `fileserver_disk_space_free_bytes` Prometheus metric to detect when the volume is approaching capacity.

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
Kubernetes Service type. `ClusterIP` is the default; the server is accessible only from within the cluster.

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
Number of days to retain log bundles before deletion. Must be 1 or greater; values less than 1 are rejected to prevent accidental data loss. Default: `7`.

#### sleepInterval
Time in seconds between cleanup cycles. Default: `86400` (24 hours). Reduce this value if storage space is limited and you need more frequent cleanup.

### Metrics

Configures the Prometheus nginx access-log exporter sidecar.

```yaml
incluster-file-server:
  metrics:
    enabled: true
```

#### enabled
When `true`, deploys the `prometheus-nginxlog-exporter` sidecar and exposes nginx access-log metrics on `metricsPort`. These metrics include request counts, response codes, and `fileserver_disk_space_free_bytes`. Requires a Prometheus installation to scrape the metrics endpoint.

### Resources

Defines CPU and memory resource requests and limits for the nginx container.

```yaml
incluster-file-server:
  resources: {}
```

No resource requests or limits are set by default. For production deployments, define explicit limits to prevent the file server from competing with workload pods for node resources.

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

Log bundles are served at `http://{service}/{NODE_NAME}/` and organised by collection timestamp beneath each node directory. To browse logs from a local machine, port-forward the service:

```bash
kubectl port-forward -n nvsentinel svc/nvsentinel-incluster-file-server 8080:80
```

Then open `http://localhost:8080/{NODE_NAME}/` in a browser or fetch files with `curl`:

```bash
curl http://localhost:8080/{NODE_NAME}/
```

## Log Cleanup and Storage Management

The cleanup sidecar runs on the `sleepInterval` schedule and deletes any file whose modification time is older than `retentionDays`. The cleanup runs in the background; it does not interrupt active HTTP downloads.

If the volume fills unexpectedly — for example, after a large-scale node failure that triggers many concurrent log collections — follow the emergency cleanup procedure in `docs/runbooks/log-rotation-failures.md`.

Monitor the `fileserver_disk_space_free_bytes` metric to receive early warning before the volume reaches capacity.
