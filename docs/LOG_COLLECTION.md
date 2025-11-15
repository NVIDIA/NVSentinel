# NVSentinel Log Collection Guide

This guide explains NVSentinel's automatic log collection functionality for troubleshooting GPU node faults.

## Table of Contents

- [Overview](#overview)
- [What Logs Are Collected](#what-logs-are-collected)
- [Where Logs Are Stored](#where-logs-are-stored)
- [When Logs Are Collected](#when-logs-are-collected)
- [How to Download Logs](#how-to-download-logs)
- [Log Rotation and Retention](#log-rotation-and-retention)
- [Additional Resources](#additional-resources)

---

## Overview

When NVSentinel detects a fault on a GPU node, it automatically collects diagnostic logs to help with troubleshooting and root cause analysis. These logs are stored in an in-cluster file server and can be easily downloaded via your web browser.

---

## What Logs Are Collected

### 1. NVIDIA Bug Report
- **File**: `nvidia-bug-report-<node-name>-<timestamp>.log.gz`
- **Description**: Comprehensive NVIDIA driver and GPU diagnostic report
- **Collection Method**:
  - **GPU Operator clusters**: Runs `nvidia-bug-report.sh` inside the nvidia-driver-daemonset pod
  - **GCP COS clusters**: Executes pre-installed nvidia-bug-report from host filesystem
- **Contains**:
  - GPU configuration and status
  - Driver version and details
  - System information
  - GPU error logs
  - PCIe information
  - DCGM diagnostics

### 2. GPU Operator Must-Gather
- **File**: `gpu-operator-must-gather-<node-name>-<timestamp>.tar.gz`
- **Description**: Kubernetes resources and logs for GPU operator components
- **Contains**:
  - GPU operator pod logs
  - DCGM exporter logs
  - Device plugin logs
  - GPU feature discovery logs
  - Operator configuration
  - Kubernetes events

### 3. GCP SOS Report (Optional)
- **File**: `sosreport-<hostname>-<timestamp>.tar.xz`
- **When Collected**: Only on GCP instances when `enableGcpSosCollection: true`
- **Contains**: System logs, configuration files, network diagnostics, storage information

### 4. AWS SOS Report (Optional)
- **File**: `sosreport-<hostname>-nvsentinel-<unique-id>-<timestamp>.tar.xz`
- **When Collected**: Only on AWS instances when `enableAwsSosCollection: true`
- **Contains**: System logs, configuration files, network diagnostics, EC2 metadata

---

## Where Logs Are Stored

### Storage Architecture

```text
Log Collector Job → In-Cluster File Server → Persistent Volume
```

### In-Cluster File Server

- **Service Name**: `nvsentinel-incluster-file-server`
- **Namespace**: `nvsentinel`
- **Internal URL**: `http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local`
- **Technology**: NGINX with WebDAV support

### Storage Configuration

Configure persistence in your Helm values:

```yaml
# Helm values for file server persistence
inclusterFileServer:
  persistence:
    enabled: true
    storageClassName: ""  # Uses default storage class
    accessModes:
      - ReadWriteOnce
    size: 50Gi  # Default size
```

### Directory Structure

Logs are organized by node name and timestamp:

```text
/usr/share/nginx/html/
└── <node-name>/
    └── <timestamp>/
        ├── nvidia-bug-report-<node-name>-<timestamp>.log.gz
        ├── gpu-operator-must-gather-<node-name>-<timestamp>.tar.gz
        ├── sosreport-<hostname>-<timestamp>.tar.xz  (if GCP SOS enabled)
        └── sosreport-<hostname>-nvsentinel-<id>-<timestamp>.tar.xz  (if AWS SOS enabled)
```

**Example**:
```text
/usr/share/nginx/html/
└── worker-node-01/
    └── 20250106-143022/
        ├── nvidia-bug-report-worker-node-01-20250106-143022.log.gz
        └── gpu-operator-must-gather-worker-node-01-20250106-143022.tar.gz
```

---

## When Logs Are Collected

### Automatic Collection Triggers

Logs are automatically collected when:

1. Fault Remediation Module detects a drain completion on a node
2. Log collection is enabled in the fault-remediation chart configuration
3. Node has experienced a fault that triggered quarantine and drain

### Configuration

Enable log collection in your Helm values:

```yaml
faultRemediation:
  enabled: true
  logCollector:
    enabled: true  # Set to true to enable automatic log collection
    uploadURL: "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload"
    gpuOperatorNamespaces: "gpu-operator"  # Comma-separated list
    enableGcpSosCollection: false  # Enable for GCP clusters
    enableAwsSosCollection: false  # Enable for AWS clusters
```

### Job Lifecycle

1. **Creation**: Fault-remediation module creates log collector job after node drain completes
2. **Execution**: Job runs with privileged access on the target node
3. **Collection**: Gathers all configured diagnostic logs (5-15 minutes typical duration)
4. **Upload**: Uploads collected logs to file server
5. **Completion**: Job completes and is cleaned up after TTL expires
6. **TTL**: Job is automatically deleted 1 hour after completion (`ttlSecondsAfterFinished: 3600`)

### Timeout Configuration

You can configure the collection timeout:

```yaml
logCollector:
  collectionTimeout: 900  # 15 minutes default
```

---

## How to Download Logs

### Using Port-Forward and Browser

This is the simplest way to browse and download logs from your local machine.

#### Step 1: Set up port-forward

```bash
kubectl port-forward -n nvsentinel svc/nvsentinel-incluster-file-server 8080:80
```

#### Step 2: Access via web browser

Open your browser to:
```text
http://localhost:8080
```

You'll see a directory listing with all node folders. Navigate through the folders to find your logs.

#### Step 3: Download files

Click on any file to download it directly from the browser.

### Viewing Collected Logs

After downloading, extract and view the logs:

#### NVIDIA Bug Report
```bash
# Decompress and view
gunzip nvidia-bug-report-<node-name>-<timestamp>.log.gz
less nvidia-bug-report-<node-name>-<timestamp>.log
```

#### GPU Operator Must-Gather
```bash
# Extract tarball
tar -xzf gpu-operator-must-gather-<node-name>-<timestamp>.tar.gz
cd gpu-operator-must-gather-<node-name>-<timestamp>/

# Browse collected resources
ls -R
```

#### SOS Reports
```bash
# Extract SOS report
tar -xJf sosreport-<hostname>-<timestamp>.tar.xz
cd sosreport-<hostname>-<timestamp>/

# View summary
less sos_reports/sos.txt
```

---

## Log Rotation and Retention

### Overview

The file server includes an automated log cleanup service that manages disk space by removing old log files based on a configurable retention policy.

### Configuration

Configure log rotation in your Helm values:

```yaml
inclusterFileServer:
  logCleanup:
    enabled: true
    retentionDays: 7  # Keep logs for 7 days (minimum: 1 day)
    sleepInterval: 86400  # Run cleanup every 24 hours (in seconds)
```

### How Log Rotation Works

1. **Continuous Monitoring**: Cleanup service runs as a sidecar container in the file server pod
2. **Periodic Cleanup**: Executes cleanup every `sleepInterval` seconds (default: 24 hours)
3. **Age-Based Deletion**: Removes files older than `retentionDays` days based on file modification time
4. **Safe Operation**: Only operates within `/usr/share/nginx/html` directory for security

### Cleanup Process

The cleanup service uses the `find` command to identify and delete old files:

```bash
find /usr/share/nginx/html -type f -mtime +<retentionDays> -delete
```

### Safety Features

1. **Minimum Retention**: Helm chart validates `retentionDays >= 1` to prevent accidental data loss
2. **Path Validation**: Only cleans files within the designated directory
3. **Timeout Protection**: Cleanup operations timeout after 5 minutes
4. **Error Tracking**: Failed cleanups are logged and tracked in metrics

### Manual Cleanup

If needed, you can manually trigger cleanup or remove specific logs:

```bash
# Get the file server pod name
FILE_SERVER_POD=$(kubectl get pods -n nvsentinel -l app.kubernetes.io/name=incluster-file-server -o jsonpath='{.items[0].metadata.name}')

# Remove logs for a specific node
kubectl exec -n nvsentinel $FILE_SERVER_POD -- rm -rf /usr/share/nginx/html/<node-name>

# Remove logs older than a specific date
kubectl exec -n nvsentinel $FILE_SERVER_POD -- find /usr/share/nginx/html -type f -mtime +14 -delete
```

---

## Additional Resources

- **[Metrics Documentation](METRICS.md)** - Prometheus metrics for monitoring log collection and file server operations
- **[Troubleshooting Runbooks](runbooks/)** - Step-by-step guides for resolving common issues:
  - [Log Collection Job Failures](runbooks/log-collection-job-failures.md)
  - [Log Rotation Failures](runbooks/log-rotation-failures.md)
- **[NVSentinel Overview](OVERVIEW.md)** - General overview of NVSentinel
- **[Helm Chart Configuration](../distros/kubernetes/README.md)** - Complete Helm chart documentation

---

## Support

For issues or questions:
- 🐛 **Bug Reports**: [Create an issue](https://github.com/NVIDIA/NVSentinel/issues/new)
- ❓ **Questions**: [Start a discussion](https://github.com/NVIDIA/NVSentinel/discussions/new?category=q-a)
- 📖 **Documentation**: [NVSentinel Docs](https://github.com/NVIDIA/NVSentinel/tree/main/docs)
