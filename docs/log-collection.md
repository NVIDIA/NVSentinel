# Log Collection

## Overview

The log collection feature of NVSentinel automatically gathers diagnostic logs from GPU nodes when NVSentinel detects faults. These logs help with troubleshooting and root cause analysis of GPU hardware and software faults.

If you enable log collection, NVSentinel can create a log collection job after it drains a node because of a fault. The job gathers comprehensive diagnostics and stores them in an in-cluster file server for easy access.

### Why Do You Need This?

When GPU nodes fail, you need diagnostic information to understand what went wrong:

- **Root cause analysis**: Determine if the fault is related to hardware, the driver, or the configuration
- **Support requests**: Provide comprehensive logs to NVIDIA support or your infrastructure team
- **Trend analysis**: Build historical data on failure patterns
- **Faster resolution**: NVSentinel collects all relevant logs automatically, so you do not gather them manually

Without log collection, you must do these steps manually before the node is rebooted or replaced:

1. SSH to the failed node.
2. Find the right log files.
3. Extract the log files.

This manual process is slow and error-prone.

## How It Works

When log collection is enabled, NVSentinel automatically collects diagnostics if it detects a fault with a **supported remediation action**:

1. The Fault Remediation module receives a health event with a supported action
2. Fault Remediation creates a Kubernetes Job on the target node to collect diagnostics
3. The Job runs with privileged access to gather logs (after the node drain and before the remediation)
4. The Job uploads the logs to the in-cluster file server
5. The Job completes, and Kubernetes automatically deletes it after 1 hour

> **Note**: NVSentinel **skips** log collection for unsupported remediation actions. For these actions, NVSentinel does no automated remediation, and the node stays accessible for manual log collection.

The file server organizes the logs by node name and timestamp. You can access the logs with a web browser through port-forwarding.

## What Logs Are Collected

### NVIDIA Bug Report

Comprehensive NVIDIA driver and GPU diagnostic report:

- GPU configuration and status
- Driver version and details
- GPU error logs
- PCIe information
- DCGM diagnostics

### GPU Operator Must-Gather (Optional)

> **Warning**: Must-gather is **disabled by default**. It collects logs from **ALL nodes** in the cluster. On large clusters, this can take a long time, for example on GB200 clusters with 100+ nodes.

When must-gather is enabled, it collects Kubernetes resources and logs for GPU operator components:

- GPU operator pod logs
- DCGM exporter logs
- Device plugin logs
- GPU feature discovery logs
- Operator configuration

**To enable must-gather**, set `enableGpuOperatorMustGather: true`. Then **increase the timeout** to match (see [Timeout Configuration](#timeout-configuration) below).

### Cloud Provider SOS Reports (Optional)

System logs and configuration from GCP or AWS instances, when this option is enabled.

## Configuration

Configure log collection through Helm values:

```yaml
fault-remediation:
  logCollector:
    enabled: false       # Enable automatic log collection
    uploadURL: "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload"
    gpuOperatorNamespaces: "gpu-operator"
    timeout: "10m"
    
    # GPU Operator must-gather (disabled by default - see warning below)
    enableGpuOperatorMustGather: false
    
    # Cloud-specific SOS collection
    enableGcpSosCollection: false
    enableAwsSosCollection: false
```

### Timeout Configuration

The default timeout of `10m` is sufficient if the job collects only the nvidia-bug-report (must-gather disabled).

> **Important**: If you enable `enableGpuOperatorMustGather`, you **MUST** increase the timeout.
>
> Must-gather collects logs from **all nodes** in the cluster. It takes approximately **2-3 minutes per node**.
>
> **Recommended timeout formula**: `(number of nodes) × 2-3 minutes`
>
> | Cluster Size | Recommended Timeout |
> |--------------|---------------------|
> | 10 nodes     | 30m                 |
> | 50 nodes     | 2h                  |
> | 100 nodes    | 4h                  |
> | 200+ nodes   | 8h+                 |

Example configuration for a 100-node cluster with must-gather enabled:

```yaml
fault-remediation:
  logCollector:
    enabled: true
    enableGpuOperatorMustGather: true
    timeout: "4h"  # Increased for 100-node cluster
```

### File Server Configuration

The in-cluster file server stores the collected logs:

```yaml
incluster-file-server:
  enabled: true          # Deploy the file server
  
  persistence:
    enabled: true
    storageClassName: ""  # Uses default storage class
    size: 50Gi
  
  logCleanup:
    enabled: true
    retentionDays: 7     # Keep logs for 7 days
    sleepInterval: 86400  # Run cleanup every 24 hours
```

### Configuration Options

- **Enable/Disable**: Enable or disable log collection for each deployment
- **Storage Size**: Configure the size of the persistent volume to match the expected log volume
- **Log Retention**: Automatically remove old logs after a configurable retention period
- **Timeout**: Set the maximum time for the log collection job (increase it if you enable must-gather)
- **Enable Must-Gather**: Enable GPU Operator must-gather collection (disabled by default because of the performance impact on large clusters)
- **Cloud-Specific SOS**: Enable additional cloud provider diagnostics

## Key Features

### Automatic Collection

NVSentinel gathers logs automatically when a supported remediation action is triggered. No manual intervention is necessary.

### Privileged Access

The collection job runs with the necessary privileges to access all diagnostic sources on the node.

### Organized Storage

The file server organizes logs by node name and timestamp, in a browsable directory structure:

```text
/node-name/timestamp/
  ├── nvidia-bug-report.log.gz
  └── gpu-operator-must-gather.tar.gz
```

### Web Access

Use a port-forward to browse and download logs with a web browser:

```bash
kubectl port-forward -n nvsentinel svc/nvsentinel-incluster-file-server 8080:80
```

Then open `http://localhost:8080` in your browser.

### Automatic Cleanup

Built-in log rotation removes old logs according to the retention policy, to manage disk space.

### Job Lifecycle Management

Kubernetes automatically deletes collection jobs 1 hour after completion.

## Storage and Retention

### Storage Architecture

An NGINX-based file server that runs in the cluster stores the logs in an attached persistent volume. The file server is accessible only from within the cluster or through a port-forward.

### Log Retention

An automatic cleanup service runs periodically (default: daily). It removes logs that are older than the configured retention period (default: 7 days). This prevents disk space problems from accumulated logs.

### Directory Structure

```text
/usr/share/nginx/html/
└── {node-name}/
    └── {timestamp}/
        ├── nvidia-bug-report-{node-name}-{timestamp}.log.gz
        └── gpu-operator-must-gather-{node-name}-{timestamp}.tar.gz
```
