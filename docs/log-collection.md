# Log Collection

## Overview

NVSentinel's log collection feature automatically gathers diagnostic logs from GPU nodes when faults are detected. These logs help with troubleshooting and root cause analysis of GPU hardware and software issues.

When a node is drained due to a fault, NVSentinel can optionally create a log collection job that gathers comprehensive diagnostics and stores them in an in-cluster file server for easy access.

### Why Do You Need This?

When GPU nodes fail, you need diagnostic information to understand what went wrong:

- **Root cause analysis**: Determine if the issue is hardware, driver, or configuration related
- **Support requests**: Provide comprehensive logs to NVIDIA support or your infrastructure team
- **Trend analysis**: Build historical data on failure patterns
- **Faster resolution**: All relevant logs collected automatically instead of manual gathering

Without log collection, you'd need to manually SSH to failed nodes, locate the right log files, and extract them before the node is rebooted or replaced - a time-consuming and error-prone process.

## How It Works

When log collection is enabled, NVSentinel automatically collects diagnostics after a node is drained:

1. Fault Remediation module detects a drained node
2. Creates a Kubernetes Job on the target node
3. Job runs with privileged access to collect diagnostics
4. Logs are uploaded to the in-cluster file server
5. Job completes and is automatically cleaned up after 1 hour

The file server stores logs organized by node name and timestamp, accessible via web browser through port-forwarding.

## What Logs Are Collected

### NVIDIA Bug Report
Comprehensive NVIDIA driver and GPU diagnostic report:
- GPU configuration and status
- Driver version and details
- GPU error logs
- PCIe information
- DCGM diagnostics

### GPU Operator Must-Gather
Kubernetes resources and logs for GPU operator components:
- GPU operator pod logs
- DCGM exporter logs
- Device plugin logs
- GPU feature discovery logs
- Operator configuration

### Cloud Provider SOS Reports (Optional)
System logs and configuration from GCP or AWS instances when enabled.

## Configuration

Configure log collection through Helm values:

```yaml
fault-remediation:
  logCollector:
    enabled: false       # Enable automatic log collection
    uploadURL: "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload"
    gpuOperatorNamespaces: "gpu-operator"
    timeout: "10m"
    
    # Cloud-specific SOS collection
    enableGcpSosCollection: false
    enableAwsSosCollection: false
```

### File Server Configuration

The in-cluster file server stores collected logs:

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

- **Enable/Disable**: Turn log collection on or off per deployment
- **Storage Size**: Configure persistent volume size based on expected log volume
- **Log Retention**: Automatically clean up old logs after configurable retention period
- **Timeout**: Set maximum time for log collection job
- **Cloud-Specific SOS**: Enable additional cloud provider diagnostics

## Key Features

### Automatic Collection
Logs are gathered automatically after node drain completes - no manual intervention required.

### Privileged Access
Collection job runs with necessary privileges to access all diagnostic sources on the node.

### Organized Storage
Logs are organized by node name and timestamp in a browsable directory structure:
```
/node-name/timestamp/
  ├── nvidia-bug-report.log.gz
  └── gpu-operator-must-gather.tar.gz
```

### Web Access
Simple port-forward to browse and download logs via web browser:
```bash
kubectl port-forward -n nvsentinel svc/nvsentinel-incluster-file-server 8080:80
```
Then access `http://localhost:8080` in your browser.

### Automatic Cleanup
Built-in log rotation removes old logs based on retention policy to manage disk space.

### Job Lifecycle Management
Collection jobs are automatically cleaned up after completion with configurable TTL (default: 1 hour).

## Storage and Retention

### Storage Architecture
Logs are stored in a persistent volume attached to an NGINX-based file server running in the cluster. The file server is accessible only within the cluster or via port-forward.

### Log Retention
Automatic cleanup service runs periodically (default: daily) to remove logs older than the configured retention period (default: 7 days). This prevents disk space issues from accumulating logs.

### Directory Structure
```
/usr/share/nginx/html/
└── <node-name>/
    └── <timestamp>/
        ├── nvidia-bug-report-<node-name>-<timestamp>.log.gz
        └── gpu-operator-must-gather-<node-name>-<timestamp>.tar.gz
```
