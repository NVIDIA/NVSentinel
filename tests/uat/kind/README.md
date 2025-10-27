# Kind UAT Configuration

This directory contains configuration files for running NVSentinel UAT tests with Kind (Kubernetes in Docker).

## Overview

When running UAT tests with Kind, the GPU Operator is **not** installed. Instead, KWOK (Kubernetes WithOut Kubelet) is used to create fake GPU nodes, and fake GPU drivers and DCGM daemonsets are deployed to simulate a GPU environment without requiring actual GPU hardware.

## Usage

To run the UAT tests with Kind:

```bash
export CSP=kind
export FAKE_GPU_NODE_COUNT=3  # Optional, defaults to 3
./tests/uat/install-apps.sh
```

## What Gets Installed

When `CSP=kind` is set:

1. **Prometheus Operator** - For metrics collection
2. **cert-manager** - For certificate management
3. **KWOK Controller** - For simulating Kubernetes nodes without kubelet (from `sigs-kwok/kwok` chart)
4. **KWOK Stage-Fast** - For managing fake node lifecycle (from `sigs-kwok/stage-fast` chart)
5. **Fake GPU Nodes** - Creates fake nodes with GPU labels and taints (via KWOK)
6. **Fake GPU Driver** - Simulates NVIDIA drivers (from `nvidia-driver-daemonset.yaml`)
7. **Fake DCGM** - Simulates DCGM monitoring (from `nvidia-dcgm-daemonset.yaml`)
8. **NVSentinel** - The main application with Kind-specific configuration

## Files

- `nvsentinel-values.yaml` - NVSentinel Helm values for Kind with janitor enabled, global affinity, and MongoDB anti-affinity
- `cert-manager-values.yaml` - cert-manager configuration
- `prometheus-operator-values.yaml` - Prometheus Operator configuration with resource limits
- `kwok-node-template.yaml` - Template for creating fake GPU nodes with KWOK
- `nvidia-driver-daemonset.yaml` - Fake GPU driver daemonset
- `nvidia-dcgm-daemonset.yaml` - Fake DCGM daemonset with service

## Key Differences from Cloud CSP

- **No GPU Operator**: GPU Operator is not installed
- **KWOK Integration**: Uses KWOK controller and stage-fast charts to create fake GPU nodes
- **Fake GPU Stack**: Uses lightweight fake daemonsets instead
- **Janitor Enabled**: Janitor module is enabled with `provider: kind`
- **No CSP Health Monitor**: CSP-specific health monitoring is disabled
- **Global Affinity**: All components avoid KWOK fake nodes and control-plane nodes
- **MongoDB Anti-Affinity**: MongoDB is configured to avoid KWOK nodes and control-plane nodes
- **Faster Deployment**: Fake GPU stack deploys much faster than real GPU Operator
- **Storage Class**: Uses `standard` storage class (Kind default) instead of CSP-specific classes

## KWOK Components

The KWOK setup uses two Helm charts from the `sigs-kwok` repository:

1. **kwok** (`sigs-kwok/kwok`) - Main KWOK controller with `hostNetwork=true`
   - Simulates kubelet behavior for fake nodes
   - Manages fake node lifecycle

2. **kwok-stage-fast** (`sigs-kwok/stage-fast`) - Stage controller
   - Manages node state transitions
   - Ensures fake nodes appear healthy and ready

## Scheduling Configuration

### Global Affinity (for all NVSentinel components)

All NVSentinel components are configured with global node affinity to run only on real worker nodes:

```yaml
global:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: type
            operator: NotIn
            values: ["kwok"]
          - key: node-role.kubernetes.io/control-plane
            operator: DoesNotExist
```

This ensures all NVSentinel components avoid:
- KWOK fake nodes (which don't have kubelet to run actual pods)
- Control-plane nodes (which should remain dedicated to cluster management)

### MongoDB Scheduling

MongoDB is additionally configured with node affinity to ensure it runs only on real worker nodes:

```yaml
mongodb:
  affinity:
    nodeAffinity:
      requiredDuringSchedulingIgnoredDuringExecution:
        nodeSelectorTerms:
        - matchExpressions:
          - key: type
            operator: NotIn
            values: ["kwok"]
          - key: node-role.kubernetes.io/control-plane
            operator: DoesNotExist
  tolerations:
  - operator: Exists
```

This prevents MongoDB from being scheduled on:
- KWOK fake nodes (which don't have kubelet to run actual pods)
- Control-plane nodes (which should remain dedicated to cluster management)

## Environment Variables

- `CSP` - Set to `kind` to enable Kind-specific installation
- `FAKE_GPU_NODE_COUNT` - Number of fake GPU nodes to create (default: 3)
- `KWOK_VERSION` - KWOK version to install (default: v0.7.0)
- `NVSENTINEL_VERSION` - NVSentinel version to deploy (required)

## Fake DCGM Image

The fake DCGM daemonset uses the image:
```
ghcr.io/nvidia/nvsentinel-fake-dcgm:4.2.0
```

This image is pre-built with NVML injection mode enabled to simulate GPU metrics without actual GPUs.
