# NVSentinel UAT Testing

User Acceptance Testing (UAT) suite for NVSentinel on cloud service providers.

## Overview

This test suite creates a production-like Kubernetes cluster on a CSP, installs NVSentinel with all dependencies, and runs end-to-end tests to verify GPU health monitoring and node remediation workflows.

## Prerequisites

### Required Tools

- **kubectl** (v1.28+)
- **helm** (v3.0+)
- **CSP-specific tools** (AWS CLI, eksctl, etc.) - see CSP-specific README

### CSP-specific Setup

For detailed prerequisites and setup instructions for your CSP, see:
- **AWS**: [`aws/README.md`](aws/README.md)

## Quick Start

### 1. CSP-specific Prerequisites

Follow the CSP-specific README to:
- Install required tools
- Configure credentials
- Create capacity reservations (for GPU instances)

Example for AWS: see [`aws/README.md`](aws/README.md)

### 2. Set Required Environment Variables

```bash
# Required
export CAPACITY_RESERVATION_ID="cr-0123456789abcdef0"  # AWS only
export NVSENTINEL_VERSION="v0.1.0"

# Optional
export CLUSTER_NAME="nvsentinel-uat"
export AWS_REGION="us-east-1"
```

### 3. Run Test Suite

```bash
cd tests/uat
./run.sh
```

The script will:
1. Create cluster with CPU and GPU nodes
2. Install Prometheus, cert-manager, GPU Operator, NVSentinel
3. Run UAT tests
4. Optionally delete cluster on completion

## Environment Variables

### Required

| Variable | Description |
|----------|-------------|
| `CAPACITY_RESERVATION_ID` | CSP capacity reservation for GPU instances |
| `NVSENTINEL_VERSION` | NVSentinel helm chart version (e.g., `v0.1.0`) |

### Optional - Cluster Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `CSP` | `aws` | Cloud service provider |
| `CLUSTER_NAME` | `nvsentinel-uat` | Cluster name |
| `AWS_REGION` | `us-east-1` | AWS region |
| `K8S_VERSION` | `1.34` | Kubernetes version |
| `GPU_AVAILABILITY_ZONE` | `e` | AZ suffix for GPU nodes |
| `CPU_NODE_TYPE` | `m7a.4xlarge` | Instance type for CPU nodes |
| `CPU_NODE_COUNT` | `3` | Number of CPU nodes |
| `GPU_NODE_TYPE` | `p5.48xlarge` | Instance type for GPU nodes |
| `GPU_NODE_COUNT` | `2` | Number of GPU nodes |

### Optional - Application Versions

| Variable | Default | Description |
|----------|---------|-------------|
| `NVSENTINEL_TAG` | `main` | Docker image tag |
| `PROMETHEUS_OPERATOR_VERSION` | `78.5.0` | Prometheus helm chart version |
| `GPU_OPERATOR_VERSION` | `v25.10.0` | GPU Operator helm chart version |
| `CERT_MANAGER_VERSION` | `1.19.1` | cert-manager helm chart version |

### Optional - Cleanup

| Variable | Default | Description |
|----------|---------|-------------|
| `DELETE_CLUSTER_ON_EXIT` | `false` | Delete cluster after tests complete |

## Test Scenarios

### Test 1: GPU Monitoring via DCGM

**Objective**: Verify DCGM-detected errors trigger node remediation.

**Flow**:
1. Select a GPU node and capture boot ID
2. Inject Inforom error via DCGM
3. Wait for node to be quarantined and rebooted
4. Wait for node to be uncordoned
5. Verify node is healthy

**Expected**: Node is automatically remediated and returns to service.

### Test 2: XID Monitoring via Syslog

**Objective**: Verify syslog-detected XID errors trigger node remediation.

**Flow**:
1. Select a GPU node and capture boot ID
2. Inject XID 119 message via logger
3. Wait for node to be quarantined and rebooted
4. Wait for node to be uncordoned
5. Verify node is healthy

**Expected**: Node is automatically remediated and returns to service.

## Cleanup

### Manual Cleanup

Delete the cluster:
```bash
export CLUSTER_NAME="nvsentinel-uat"
cd tests/uat/aws
./delete-eks-cluster.sh
```

Cancel capacity reservation (AWS):
```bash
aws ec2 cancel-capacity-reservation \
  --capacity-reservation-id "$CAPACITY_RESERVATION_ID" \
  --region "$AWS_REGION"
```

### Automatic Cleanup

Set `DELETE_CLUSTER_ON_EXIT=true` to automatically delete the cluster after tests:
```bash
export DELETE_CLUSTER_ON_EXIT=true
./run.sh
```

## Cost Considerations

See CSP-specific README for cost estimates:
- **AWS**: [`aws/README.md`](aws/README.md#cost-considerations)

⚠️ **Always clean up resources after testing!**
